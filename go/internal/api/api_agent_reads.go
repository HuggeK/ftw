package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"hash"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/protocol"
)

// ServeAgentRead gives a granted diagnostic session all technical reads.
// It keeps names, addresses, source code and measured data as the handlers
// return them. Existing handlers still mask saved credentials where they do
// so on the box page. api.req and all write handlers remain separate.
func (s *Server) ServeAgentRead(w http.ResponseWriter, request *http.Request, read protocol.APIRead) {
	caller, named := apiauth.FromRequest(request)
	if !named || !apiauth.AgentRole(caller.Role) || !caller.Scopes.Has(protocol.ScopeDiagRun) || request.Method != http.MethodGet {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this diagnostic read is not granted"})
		return
	}
	if request.URL.Path == protocol.AgentAPICatalogPath {
		writeJSON(w, http.StatusOK, map[string]any{
			"routes": s.agentReadCatalog(), "requiredScope": protocol.ScopeDiagRun,
			"timeoutMs":       protocol.AgentAPIReadTimeout.Milliseconds(),
			"maxPageBytes":    protocol.AgentAPIPageBytes,
			"troubleshooting": s.agentDebugContext(),
			"queryPolicy":     "Use the existing API's parameters. Query hints are not a restriction.",
			"timeUnit":        "Time query parameters since, until, since_ms, until_ms and ts use Unix milliseconds",
			"pagination":      "Large answers use byte pages. Pass next_offset as offset and sha256 as if_sha256 to detect a changed answer.",
		})
		return
	}
	if !s.agentReadAllowed(request) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this route is not a diagnostic read; settings and control use their command tools"})
		return
	}
	if read.Offset < 0 || read.MaxBytes < 0 || read.MaxBytes > protocol.AgentAPIPageBytes {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid diagnostic page"})
		return
	}
	pageBytes := read.MaxBytes
	if pageBytes == 0 {
		pageBytes = 64 << 10
	}
	answer := &agentReadBuffer{ctx: request.Context(), header: make(http.Header), status: http.StatusOK,
		offset: read.Offset, limit: pageBytes, digest: sha256.New()}

	// DiagRun covers all diagnostic reads, including pairing and vehicle
	// status. Add only these read scopes to the handler context; this does
	// not change the stored grant, role, caller or any command authority.
	scopes := append(caller.Scopes.Names(), protocol.ScopeMembersRead, protocol.ScopeCarsRead)
	caller.Scopes = apiauth.NewScopeSet(scopes...)
	request = request.WithContext(apiauth.WithCaller(request.Context(), caller))
	if read.ArchiveMember != "" {
		s.serveAgentArchive(answer, request, read.ArchiveMember)
	} else {
		s.serveAgentHTTP(answer, request)
	}
	if answer.failed != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not read the archive member completely"})
		return
	}
	if request.Context().Err() != nil {
		return
	}
	digest := hex.EncodeToString(answer.digest.Sum(nil))
	if read.IfSHA256 != "" && read.IfSHA256 != digest {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the answer changed between pages; start again or narrow the query"})
		return
	}
	if read.Offset > answer.total {
		writeJSON(w, http.StatusRequestedRangeNotSatisfiable, map[string]string{"error": "offset exceeds the answer length"})
		return
	}
	body := answer.body.Bytes()
	more := read.Offset+int64(len(body)) < answer.total
	complete := read.Offset == 0 && !more
	encoding := "base64"
	var data any = base64.StdEncoding.EncodeToString(body)
	if complete && json.Valid(body) {
		encoding, data = "json", json.RawMessage(body)
	} else if utf8.Valid(body) && !agentBinaryMedia(answer.header.Get("Content-Type")) {
		encoding, data = "utf-8", string(body)
	}
	result := map[string]any{"content_type": answer.header.Get("Content-Type"), "encoding": encoding, "body": data,
		"offset": read.Offset, "total_bytes": answer.total, "sha256": digest, "complete": complete, "more": more}
	if more {
		result["next_offset"] = read.Offset + int64(len(body))
	}
	writeJSON(w, answer.status, result)
}

// The LAN administration handlers have presence checks as well as data
// reads. Project their existing read results here without claiming that this
// remote caller is on the LAN or is an owner, and without admitting mutations.
func (s *Server) serveAgentHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/agent-invites":
		if s.deps == nil || s.deps.Agents == nil {
			writeJSON(w, 503, map[string]string{"error": "agents unavailable"})
			return
		}
		if a, ok := s.deps.Agents.(AgentInviteAdmin); ok {
			writeJSON(w, 200, map[string]any{"invitations": a.AgentInvites()})
			return
		}
		writeJSON(w, 503, map[string]string{"error": "invitations unavailable"})
	case "/api/agents", "/api/agents/audit":
		if s.deps.Agents == nil {
			writeJSON(w, 503, map[string]string{"error": "agents are not available on this box"})
			return
		}
		a := s.deps.Agents
		if r.URL.Path == "/api/agents/audit" {
			before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
			n, _ := strconv.Atoi(r.URL.Query().Get("n"))
			rows, more, err := a.Audit(before, n)
			if err != nil {
				agentError(w, err)
				return
			}
			writeJSON(w, 200, protocol.AuditPage{Rows: nonNil(rows), More: more})
			return
		}
		facts := a.Facts()
		facts.Widen = false
		writeJSON(w, 200, agentsPage{Agents: nonNil(a.Agents()), Requests: nonNil(a.Requests()), Keys: nonNil(a.ApprovalKeys()), Box: facts, Registry: registryForPage()})
	case "/api/cloud", "/api/cloud/preview":
		link, ok := s.cloudLink(w)
		if !ok {
			return
		}
		if r.URL.Path == "/api/cloud" {
			writeJSON(w, 200, link.Status())
		} else {
			writeJSON(w, 200, link.Preview())
		}
	default:
		s.ServeHTTP(w, r)
	}
}

// Retain one byte page while hashing and counting the whole answer. A large
// history export or backup has no total-byte cutoff. Context cancellation
// stops a handler that keeps writing after the session ends.
type agentReadBuffer struct {
	ctx                  context.Context
	header               http.Header
	status               int
	body                 bytes.Buffer
	wrote                bool
	offset, limit, total int64
	digest               hash.Hash
	failed               error
}

func (b *agentReadBuffer) Header() http.Header { return b.header }
func (b *agentReadBuffer) WriteHeader(status int) {
	if !b.wrote {
		b.status, b.wrote = status, true
	}
}
func (b *agentReadBuffer) Write(body []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	b.WriteHeader(http.StatusOK)
	_, _ = b.digest.Write(body)
	start := min(int64(len(body)), max(int64(0), b.offset-b.total))
	count := min(int64(len(body))-start, b.limit-int64(b.body.Len()))
	if count > 0 {
		_, _ = b.body.Write(body[start : start+count])
	}
	b.total += int64(len(body))
	return len(body), nil
}

func agentBinaryMedia(media string) bool {
	switch media {
	case "application/zip", "application/gzip", "application/x-gzip", "application/octet-stream":
		return true
	}
	return false
}
