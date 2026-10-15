package state

import (
	"path/filepath"
	"testing"
	"time"
)

func TestTheAgentAuditPagesNewestFirstAndForgetsOldRows(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UnixMilli()
	for i := range 5 {
		if err := st.AppendAgentAudit(AgentAuditRow{AtMs: now, Actor: "agent:abcd1234", Role: "support",
			Principal: "FTW-123", By: "staff-7", What: "logs.get", Result: "ok", Target: string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	page, more, err := st.AgentAudit(0, 2)
	if err != nil || !more || len(page) != 2 || page[0].Target != "e" || page[0].By != "staff-7" {
		t.Fatalf("first page %+v more=%v err=%v", page, more, err)
	}
	rest, more, err := st.AgentAudit(page[1].Seq, 10)
	if err != nil || more || len(rest) != 3 || rest[0].Target != "c" {
		t.Fatalf("second page %+v more=%v err=%v", rest, more, err)
	}

	old := now - (AgentAuditDaysKept+1)*24*60*60*1000
	if err := st.AppendAgentAudit(AgentAuditRow{AtMs: old, Actor: "agent:old", What: "plan.get", Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := st.pruneAgentAudit(6, time.Now()); err != nil {
		t.Fatal(err)
	}
	all, _, _ := st.AgentAudit(0, 200)
	for _, r := range all {
		if r.Actor == "agent:old" {
			t.Error("a row past 90 days survived the prune")
		}
	}
	if len(all) != 5 {
		t.Errorf("kept %d rows", len(all))
	}
}

// Agents' rows and owners' keep budgets of their own.
func TestAnAgentsRowsNeverPushOutAnOwners(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UnixMilli()
	if err := st.AppendAgentAudit(AgentAuditRow{AtMs: now, Actor: "app:owner", What: "agent.add", Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	for range AgentAuditRowsKept + 5 {
		if _, err := st.db.Exec(`INSERT INTO agent_audit (at_ms, actor, what, result, count) VALUES (?, 'agent:a', 'plan.get', 'ok', 3)`, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.pruneAgentAudit(0, time.Now()); err != nil {
		t.Fatal(err)
	}
	var reads, others int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM agent_audit WHERE actor LIKE 'agent:%'`).Scan(&reads)
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM agent_audit WHERE actor NOT LIKE 'agent:%'`).Scan(&others)
	if reads != AgentAuditRowsKept || others != 1 {
		t.Errorf("reads %d, others %d", reads, others)
	}
}
