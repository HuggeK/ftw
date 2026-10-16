package main

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appuplink"
	"github.com/srcfl/ftw/go/protocol"
)

// An agent added or widened at the box page reaches FTW Cloud as an
// agent.added notice that names its client; the link decides whether the
// name leaves the box.
func TestAnAgentAddedAtTheBoxPageNamesItsClient(t *testing.T) {
	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "box.key"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(1_792_000_000_000)
	clock := func() (time.Time, bool) { return now, true }
	enroll.SetClock(clock)
	type sent struct{ kind, client string }
	var notices []sent
	p := &agentPorts{access: &appuplink.AgentAccess{Enroll: enroll, Clock: clock},
		notice: func(kind, client string) bool { notices = append(notices, sent{kind, client}); return true }}

	row, err := boxPageAgents{p}.AddAgent("owner", protocol.AgentAdd{
		Key: bytes.Repeat([]byte{7}, 32), Role: apiauth.RoleAgent, Scopes: []string{protocol.ScopeLiveRead},
		ExpiresAtMs: now.UnixMilli() + time.Hour.Milliseconds(),
		Principal:   protocol.Principal{Client: "Claude", ClientID: "https://claude.ai/oauth/mcp-oauth-client-metadata"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 || notices[0] != (sent{protocol.PushAgentAdded, "Claude"}) {
		t.Fatalf("notices %+v", notices)
	}
	if _, err := (boxPageAgents{p}).WidenAgent("owner", protocol.AgentWiden{ID: row.ID, Epoch: row.Epoch,
		Scopes: []string{protocol.ScopeHistoryRead, protocol.ScopeLiveRead}, ExpiresAtMs: row.ExpiresAtMs}); err != nil {
		t.Fatal(err)
	}
	if len(notices) != 2 || notices[1] != (sent{protocol.PushAgentAdded, "Claude"}) {
		t.Fatalf("a widened grant: notices %+v", notices)
	}
}
