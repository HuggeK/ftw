package onboarding

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/srcfl/ftw/go/internal/config"
)

// Prepare holds one exact draft while a bounded setup operation uses it.
// Its callback may update credentials, never live settings. A concurrent
// edit cannot replace the driver halfway through authentication or a probe.
func (s *Service) Prepare(live *config.Config, revision int64, prepare func(*config.Config) error, check func() error) (Draft, error) {
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
	if live == nil || d.Revision != revision || d.BaseRevision != live.Revision {
		return Draft{}, ErrConflict
	}
	cfg, err := decodeConfig(d.Config)
	if err != nil {
		return Draft{}, err
	}
	if check != nil {
		if err := check(); err != nil {
			return Draft{}, err
		}
	}
	if err := prepare(cfg); err != nil {
		return Draft{}, err
	}
	previous, err := decodeConfig(d.Config)
	if err != nil {
		return Draft{}, err
	}
	if err := config.PrepareDraftCredentials(cfg, previous); err != nil {
		return Draft{}, err
	}
	if err := config.PrepareDraftCredentials(cfg, live); err != nil {
		return Draft{}, err
	}
	s.portablePaths(cfg)
	raw, err := json.Marshal(cfg)
	if err != nil {
		return Draft{}, err
	}
	if check != nil {
		if err := check(); err != nil {
			return Draft{}, err
		}
	}
	if bytes.Equal(raw, d.Config) {
		return d, nil
	}
	d.Config = raw
	d.Revision++
	d.NormalizedFromRevision, d.ValidationWarnings = 0, nil
	d.SHA256 = digest(raw, d.BaseRevision)
	encoded, err := json.Marshal(d)
	if err != nil {
		return Draft{}, err
	}
	if len(encoded) > MaxDraftBytes {
		return Draft{}, errors.New("setup draft is too large")
	}
	if err := s.store.SaveConfig(draftKey, string(encoded)); err != nil {
		return Draft{}, err
	}
	return d, nil
}
