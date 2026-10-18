package api

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
)

// Archive reads stay on the box. Spool a generated export to a private temp
// file, inspect entries without extracting paths, and remove it on return.
// The existing page writer keeps only the requested member's byte page.
func (s *Server) serveAgentArchive(w *agentReadBuffer, r *http.Request, member string) {
	file, err := os.CreateTemp("", "ftw-diagnostic-*")
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "could not open diagnostic export"})
		return
	}
	defer os.Remove(file.Name())
	defer file.Close()
	spool := &agentArchiveSpool{ctx: r.Context(), file: file, header: make(http.Header), status: 200}
	s.serveAgentHTTP(spool, r)
	if spool.err != nil || r.Context().Err() != nil {
		writeJSON(w, 503, map[string]string{"error": "could not finish diagnostic export"})
		return
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not read diagnostic export"})
		return
	}
	// Preserve the handler's error answer rather than trying to open it as an archive.
	if spool.status != 200 {
		w.Header().Set("Content-Type", spool.header.Get("Content-Type"))
		w.WriteHeader(spool.status)
		_, _ = io.Copy(w, file)
		return
	}
	var files []map[string]any
	found := false
	relative := func(name string) string {
		_, rest, ok := strings.Cut(name, "/")
		if ok && rest != "" {
			return rest
		}
		return name
	}
	send := func(name string, size int64, open func() (io.ReadCloser, error)) error {
		name = relative(name)
		if member == "." {
			files = append(files, map[string]any{"name": name, "bytes": size})
			return nil
		}
		if name != member {
			return nil
		}
		body, err := open()
		if err != nil {
			return err
		}
		defer body.Close()
		found = true
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, err = io.Copy(w, body)
		return err
	}
	switch strings.Split(spool.header.Get("Content-Type"), ";")[0] {
	case "application/zip":
		stat, e := file.Stat()
		if e != nil {
			err = e
			break
		}
		archive, e := zip.NewReader(file, stat.Size())
		if e != nil {
			err = e
			break
		}
		for _, entry := range archive.File {
			if entry.FileInfo().IsDir() {
				continue
			}
			entry := entry
			err = send(entry.Name, int64(entry.UncompressedSize64), entry.Open)
			if err != nil || found {
				break
			}
		}
	case "application/gzip", "application/x-gzip":
		gz, e := gzip.NewReader(file)
		if e != nil {
			err = e
			break
		}
		defer gz.Close()
		archive := tar.NewReader(gz)
		for {
			header, e := archive.Next()
			if e == io.EOF {
				break
			}
			if e != nil {
				err = e
				break
			}
			if header.Typeflag != tar.TypeReg {
				continue
			}
			err = send(header.Name, header.Size, func() (io.ReadCloser, error) { return io.NopCloser(archive), nil })
			if err != nil || found {
				break
			}
		}
	default:
		writeJSON(w, 415, map[string]string{"error": "archive_member requires a ZIP or tar.gz export; omit it to read the original bytes"})
		return
	}
	if err != nil {
		w.failed = err
		return
	}
	if member == "." {
		writeJSON(w, 200, map[string]any{"files": files})
		return
	}
	if !found {
		writeJSON(w, 404, map[string]string{"error": "archive member not found; use archive_member '.' to list files"})
	}
}

type agentArchiveSpool struct {
	ctx    context.Context
	file   *os.File
	header http.Header
	status int
	wrote  bool
	err    error
}

func (s *agentArchiveSpool) Header() http.Header { return s.header }
func (s *agentArchiveSpool) WriteHeader(status int) {
	if !s.wrote {
		s.status, s.wrote = status, true
	}
}
func (s *agentArchiveSpool) Write(body []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if err := s.ctx.Err(); err != nil {
		s.err = err
		return 0, err
	}
	s.WriteHeader(200)
	n, err := s.file.Write(body)
	s.err = err
	return n, err
}
