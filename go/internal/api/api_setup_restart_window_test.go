package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/protocol"
)

func TestQueuedSetupRestartRechecksBeforeEffect(t *testing.T) {
	for _, change := range []string{"settings", "grant"} {
		t.Run(change, func(t *testing.T) {
			s, cfg := setupTestServer(t)
			q := savedSetupForRestart(t, s)
			var count atomic.Int32
			var revoked atomic.Bool
			s.deps.Restart = func(context.Context) error { count.Add(1); return nil }
			guard := func() error {
				if revoked.Load() {
					return errors.New("revoked")
				}
				return nil
			}
			w, _ := agentSetupCall(t, s, protocol.MsgSetupRestart, q, []string{protocol.ScopeSetupWrite}, guard)
			if w.Code != http.StatusAccepted {
				t.Fatal(w.Code, w.Body.String())
			}
			if change == "grant" {
				revoked.Store(true)
			} else {
				next := *cfg
				next.Site.Name = "Saved after restart was queued"
				raw, err := json.Marshal(next)
				if err != nil {
					t.Fatal(err)
				}
				if code := postConfig(t, s, string(raw)); code != http.StatusOK {
					t.Fatal(code)
				}
			}
			deadline := time.After(2 * time.Second)
			for {
				s.configWriteMu.Lock()
				queued := s.setupRestartRevision
				s.configWriteMu.Unlock()
				if queued == 0 {
					break
				}
				select {
				case <-deadline:
					t.Fatal("refused restart did not release its retry guard")
				case <-time.After(time.Millisecond):
				}
			}
			if count.Load() != 0 {
				t.Errorf("queued setup restart ran after %s changed before effect", change)
			}
		})
	}
}
