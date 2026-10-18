package protocol

import (
	"path"
	"strings"
	"unicode"
)

// APIRead is a diagnostic GET, with no method, body, headers or write path.
// Offset and MaxBytes select a byte page. IfSHA256 prevents joining pages
// from different answers. The answer uses api.head/chunk/end, carrying JSON.
type APIRead struct {
	Path          string            `cbor:"path"`
	Query         map[string]string `cbor:"query,omitempty"`
	MaxBytes      int64             `cbor:"maxBytes,omitempty"`
	Offset        int64             `cbor:"offset,omitempty"`
	IfSHA256      string            `cbor:"ifSHA256,omitempty"`
	ArchiveMember string            `cbor:"archiveMember,omitempty"`
}

func DecodeAPIRead(body []byte) (APIRead, error) {
	var read APIRead
	err := strictDec.Unmarshal(body, &read)
	return read, err
}

const (
	AgentAPIMaxBytes    = 1 << 20   // Complete JSON wire answer, including escaped text.
	AgentAPIPageBytes   = 128 << 10 // Source bytes per page; not a total data limit.
	AgentAPICatalogPath = "/api/diagnostics/catalog"
)

// AgentAPIRoute describes a route registered by the connected Core version.
// Query is guidance; the existing HTTP handler owns parameter validation.
type AgentAPIRoute struct {
	Path        string            `json:"path"`
	Scope       string            `json:"scope"`
	Description string            `json:"description"`
	Query       map[string]string `json:"query,omitempty"`
}

// ValidAgentAPIPath validates syntax only. Core's router decides whether the
// path is a read; Cloud does not keep a second list of the box's API.
func ValidAgentAPIPath(value string) bool {
	return strings.HasPrefix(value, "/api/") && path.Clean(value) == value &&
		!strings.ContainsAny(value, "?%#\\") && strings.IndexFunc(value, unicode.IsControl) < 0
}
