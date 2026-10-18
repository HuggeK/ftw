package onboarding

import (
	"encoding/json"
	"errors"
)

// CommitKey names the record committed atomically with live Settings.
const CommitKey = "onboarding/commit_v1"

// Receipt proves that the named draft reached durable Settings. Whether
// the running process applied it and hardware works are separate facts.
type Receipt struct {
	ProcessID      string   `json:"process_id"`
	DraftRevision  int64    `json:"draft_revision"`
	SHA256         string   `json:"sha256"`
	BaseRevision   int64    `json:"base_revision"`
	SavedRevision  int64    `json:"saved_revision"`
	RestartReasons []string `json:"restart_reasons"`
}

func (s *Service) Receipt() (*Receipt, error) {
	if s.store == nil {
		return nil, errors.New("setup storage is unavailable")
	}
	raw, found, err := s.store.ConfigValue(CommitKey)
	if err != nil {
		return nil, err
	}
	if !found || raw == "" {
		return nil, nil
	}
	var receipt Receipt
	if len(raw) > MaxDraftBytes || json.Unmarshal([]byte(raw), &receipt) != nil || receipt.DraftRevision < 1 || receipt.SavedRevision != receipt.BaseRevision+1 || len(receipt.SHA256) != 64 {
		return nil, errors.New("stored setup receipt is invalid")
	}
	return &receipt, nil
}
