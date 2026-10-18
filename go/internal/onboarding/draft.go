// Package onboarding keeps an unfinished setup on the box. It does not
// change live configuration or issue device commands.
package onboarding

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/srcfl/ftw/go/internal/config"
)

const (
	draftKey      = "onboarding/draft_v1"
	MaxDraftBytes = 1 << 20
)

var ErrConflict = errors.New("setup changed; read the current draft before editing")

// Storage is the box's durable settings store. A failed write must leave
// the previous value intact.
type Storage interface {
	ConfigValue(string) (string, bool, error)
	SaveConfig(string, string) error
}

// Draft is an uncommitted configuration. Config can contain credentials;
// a transport must mask them before returning it to a client.
type Draft struct {
	Version      int             `json:"version"`
	Revision     int64           `json:"revision"`
	BaseRevision int64           `json:"base_revision"`
	Config       json.RawMessage `json:"config"`
	SHA256       string          `json:"sha256"`
}

// Service serializes draft changes. Live config saves remain the caller's
// responsibility, including a final compare-and-save of BaseRevision.
type Service struct {
	mu              sync.Mutex
	store           Storage
	preserveSecrets func(incoming, previous *config.Config)
}

func New(store Storage) *Service { return NewWithSecrets(store, nil) }

// NewWithSecrets adds the running driver's secret metadata to the common
// config secret handling. The callback must be safe to call during an edit.
func NewWithSecrets(store Storage, preserve func(incoming, previous *config.Config)) *Service {
	return &Service{store: store, preserveSecrets: preserve}
}

func (s *Service) current() (Draft, bool, error) {
	if s.store == nil {
		return Draft{}, false, errors.New("setup storage is unavailable")
	}
	raw, ok, err := s.store.ConfigValue(draftKey)
	if err != nil {
		return Draft{}, false, fmt.Errorf("read setup draft: %w", err)
	}
	if !ok || raw == "" {
		return Draft{}, false, nil
	}
	var d Draft
	if len(raw) > MaxDraftBytes || json.Unmarshal([]byte(raw), &d) != nil || d.Version != 1 || d.Revision < 1 || d.BaseRevision < 0 {
		return Draft{}, false, errors.New("stored setup draft is invalid; it has not been replaced")
	}
	if _, err := decodeConfig(d.Config); err != nil || d.SHA256 != digest(d.Config, d.BaseRevision) {
		return Draft{}, false, errors.New("stored setup draft failed its integrity check; it has not been replaced")
	}
	return d, true, nil
}

// Read returns a saved draft, or a draft based on this exact live config.
// Reading never creates a stored draft or changes settings.
func (s *Service) Read(live *config.Config) (Draft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, found, err := s.current(); found || err != nil {
		return d, err
	}
	return initial(live)
}

func initial(live *config.Config) (Draft, error) {
	if live == nil {
		return Draft{}, errors.New("live configuration is unavailable")
	}
	raw, err := json.Marshal(live)
	if err != nil {
		return Draft{}, err
	}
	return Draft{Version: 1, BaseRevision: live.Revision, Config: raw, SHA256: digest(raw, live.Revision)}, nil
}

// Edit applies a JSON merge patch to a typed config. Fields omitted by the
// client keep their current value, including credentials. Arrays replace
// the whole array; the client must read it before editing an entry.
// Partial drafts may fail Config.Validate while still having valid types.
func (s *Service) Edit(live *config.Config, expected int64, patch json.RawMessage) (Draft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, found, err := s.current()
	if err != nil {
		return Draft{}, err
	}
	if !found {
		d, err = initial(live)
		if err != nil {
			return Draft{}, err
		}
	}
	if d.Revision != expected || live == nil || d.BaseRevision != live.Revision {
		return Draft{}, ErrConflict
	}
	if len(patch) == 0 || len(patch) > MaxDraftBytes {
		return Draft{}, errors.New("setup patch is empty or too large")
	}
	var before, changes map[string]json.RawMessage
	if err := strictObject(patch, &changes); err != nil {
		return Draft{}, err
	}
	if err := json.Unmarshal(d.Config, &before); err != nil {
		return Draft{}, err
	}
	merged, err := merge(before, changes)
	if err != nil {
		return Draft{}, err
	}
	cfg, err := decodeConfig(merged)
	if err != nil {
		return Draft{}, err
	}
	previous, err := decodeConfig(d.Config)
	if err != nil {
		return Draft{}, err
	}
	if cfg.API.LANAuth != previous.API.LANAuth {
		return Draft{}, errors.New("api.lan_auth changes through the house-password action on the box, not a setup draft")
	}
	if s.preserveSecrets != nil {
		s.preserveSecrets(cfg, previous)
	} else {
		cfg.PreserveMaskedSecrets(previous)
	}
	if err := config.PrepareDraftCredentials(cfg, previous); err != nil {
		return Draft{}, err
	}
	// Canonicalize through the current type. Do not default or validate yet:
	// both would hide missing answers or reject an unfinished installation.
	d.Config, err = json.Marshal(cfg)
	if err != nil {
		return Draft{}, err
	}
	d.Revision++
	d.SHA256 = digest(d.Config, d.BaseRevision)
	raw, err := json.Marshal(d)
	if err != nil || len(raw) > MaxDraftBytes {
		return Draft{}, errors.New("setup draft is too large")
	}
	if err := s.store.SaveConfig(draftKey, string(raw)); err != nil {
		return Draft{}, fmt.Errorf("save setup draft: %w", err)
	}
	return d, nil
}

// Checked returns a normalized, valid candidate only when the caller names
// the exact current draft and live base. It neither persists nor applies it.
func (s *Service) Checked(liveRevision, revision int64, sha string) (*config.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkedLocked(liveRevision, revision, sha)
}

// ApplyChecked keeps the draft fixed while the caller compares and saves
// its live base. The callback must enforce the live configuration revision.
func (s *Service) ApplyChecked(liveRevision, revision int64, sha string, apply func(*config.Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.checkedLocked(liveRevision, revision, sha)
	if err != nil {
		return err
	}
	return apply(cfg)
}

func (s *Service) checkedLocked(liveRevision, revision int64, sha string) (*config.Config, error) {
	d, found, err := s.current()
	if err != nil {
		return nil, err
	}
	if !found || d.BaseRevision != liveRevision || d.Revision != revision || d.SHA256 != sha {
		return nil, ErrConflict
	}
	cfg, err := decodeConfig(d.Config)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("configuration is incomplete or invalid: %w", err)
	}
	return cfg, nil
}

// Restart abandons a stale draft only when the caller names its current
// revision. It stores a new draft from live settings, never an empty config.
func (s *Service) Restart(live *config.Config, expected int64) (Draft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, found, err := s.current()
	if err != nil {
		return Draft{}, err
	}
	if (!found && expected != 0) || (found && old.Revision != expected) {
		return Draft{}, ErrConflict
	}
	d, err := initial(live)
	if err != nil {
		return Draft{}, err
	}
	d.Revision = old.Revision + 1
	raw, err := json.Marshal(d)
	if err != nil || len(raw) > MaxDraftBytes {
		return Draft{}, errors.New("setup draft is too large")
	}
	if err := s.store.SaveConfig(draftKey, string(raw)); err != nil {
		return Draft{}, err
	}
	return d, nil
}

func digest(raw []byte, revision int64) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d\n", revision)
	h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
}

func decodeConfig(raw []byte) (*config.Config, error) {
	if len(raw) > MaxDraftBytes {
		return nil, errors.New("setup config is too large")
	}
	var cfg config.Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("setup config: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("setup config must contain one JSON object")
	}
	return &cfg, nil
}

func strictObject(raw []byte, out *map[string]json.RawMessage) error {
	if err := rejectDuplicates(raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil || *out == nil {
		return errors.New("setup patch must be a JSON object")
	}
	return nil
}

func merge(before, patch map[string]json.RawMessage) ([]byte, error) {
	for key, value := range patch {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			delete(before, key)
			continue
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(value, &object) == nil && object != nil {
			var previous map[string]json.RawMessage
			_ = json.Unmarshal(before[key], &previous)
			if previous == nil {
				previous = map[string]json.RawMessage{}
			}
			merged, err := merge(previous, object)
			if err != nil {
				return nil, err
			}
			before[key] = merged
		} else {
			before[key] = value
		}
	}
	return json.Marshal(before)
}

// JSON decoders normally accept repeated keys with the last one winning.
// A checked configuration must have one meaning at every boundary.
func rejectDuplicates(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value func() error
	value = func() error {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		delim, container := token.(json.Delim)
		if !container {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("setup patch contains a repeated key")
				}
				seen[name] = true
				if err := value(); err != nil {
					return err
				}
			}
		case '[':
			for dec.More() {
				if err := value(); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid setup JSON")
		}
		_, err = dec.Token()
		return err
	}
	if err := value(); err != nil {
		return fmt.Errorf("setup patch: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("setup patch must contain one JSON object")
	}
	return nil
}
