package state

import "time"

// AgentAuditRow is one line of the agents' audit log.
type AgentAuditRow struct {
	Seq       int64
	AtMs      int64
	Actor     string
	Role      string
	Principal string
	By        string
	What      string
	Target    string
	Result    string
	// Count is how many reads a folded row stands for; 0 for any other row.
	Count int64
}

// The audit log keeps this much, for agents' rows and for everyone else's
// each: enough for an owner to see a month of an assistant's calls, small
// enough that it never matters on an SD card.
const (
	AgentAuditRowsKept = 10_000
	AgentAuditDaysKept = 90
	// agentAuditPruneEvery is how many rows go in between two prunes.
	agentAuditPruneEvery = 100
)

// AppendAgentAudit writes one row, and every so often drops what is past
// the limits.
func (s *Store) AppendAgentAudit(r AgentAuditRow) error {
	res, err := s.db.Exec(`INSERT INTO agent_audit (at_ms, actor, role, principal, by_staff, what, target, result, count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.AtMs, r.Actor, r.Role, r.Principal, r.By, r.What, r.Target, r.Result, r.Count)
	if err != nil {
		return err
	}
	seq, err := res.LastInsertId()
	if err != nil || seq%agentAuditPruneEvery != 0 {
		return err
	}
	return s.pruneAgentAudit(seq, time.Now())
}

func (s *Store) pruneAgentAudit(_ int64, now time.Time) error {
	oldest := now.Add(-AgentAuditDaysKept * 24 * time.Hour).UnixMilli()
	// A row stamped 0 was written before NTP set the clock. Its age is
	// unknown, so only the row count retires it.
	if _, err := s.db.Exec(`DELETE FROM agent_audit WHERE at_ms > 0 AND at_ms < ?`, oldest); err != nil {
		return err
	}
	// Agents' rows and everyone else's keep budgets of their own, so no agent
	// can push an owner's grant, key or revoke out of the log.
	for _, agent := range []bool{true, false} {
		if _, err := s.db.Exec(`DELETE FROM agent_audit WHERE (actor LIKE 'agent:%') = ? AND seq <= (
			SELECT seq FROM agent_audit WHERE (actor LIKE 'agent:%') = ? ORDER BY seq DESC LIMIT 1 OFFSET ?)`,
			agent, agent, AgentAuditRowsKept); err != nil {
			return err
		}
	}
	return nil
}

// AgentAudit pages through the log, newest first: up to n rows before
// beforeSeq, or from the newest when it is 0. more says older rows remain.
func (s *Store) AgentAudit(beforeSeq int64, n int) ([]AgentAuditRow, bool, error) {
	if n <= 0 || n > 200 {
		n = 200
	}
	if beforeSeq <= 0 {
		beforeSeq = 1 << 62
	}
	rows, err := s.db.Query(`SELECT seq, at_ms, actor, role, principal, by_staff, what, target, result, count
		FROM agent_audit WHERE seq < ? ORDER BY seq DESC LIMIT ?`, beforeSeq, n+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []AgentAuditRow
	for rows.Next() {
		var r AgentAuditRow
		if err := rows.Scan(&r.Seq, &r.AtMs, &r.Actor, &r.Role, &r.Principal, &r.By, &r.What, &r.Target, &r.Result, &r.Count); err != nil {
			return nil, false, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > n
	if more {
		out = out[:n]
	}
	return out, more, nil
}
