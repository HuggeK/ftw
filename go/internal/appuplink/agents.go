package appuplink

import (
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/protocol"
)

// AgentAccess is the session's view of agent grants (appproto.Access): the
// grants and approval keys live in enrolment, and a removed row's sessions
// end through the uplink.
type AgentAccess struct {
	Enroll *appenroll.Identity
	// Drop ends every session a removed key holds. Nil ends none.
	Drop func(key []byte)
	// Push sends a catalogue kind to every owner, with the client given
	// access on agent.added. Nil sends nothing.
	Push func(kind, client string)
	// Clock is the wall clock and whether NTP has set it. The same clock
	// enrolment expires agent rows by.
	Clock func() (time.Time, bool)
	// Holds releases every hold an agent set, by its subject. Nil releases
	// none.
	Holds func(subject string)
}

func approver(deviceID string) appenroll.Approver { return appenroll.Approver{DeviceID: deviceID} }

func (a *AgentAccess) AddAgent(deviceID string, m protocol.AgentAdd) (protocol.AgentRow, error) {
	row, err := a.Enroll.AddAgent(approver(deviceID), m)
	if err == nil {
		a.notify(protocol.PushAgentAdded, row.Principal.Client)
	}
	return row, err
}

func (a *AgentAccess) WidenAgent(deviceID string, m protocol.AgentWiden) (protocol.AgentRow, error) {
	return a.Enroll.WidenAgent(approver(deviceID), m)
}

// RevokeAgent removes an agent row at once. Its sessions end on their own
// goroutine, because the caller may be one of them, answering its own
// revoke; the grant is already gone, so its next call fails either way.
func (a *AgentAccess) RevokeAgent(id string) error {
	removed, err := a.Enroll.Revoke(id, appenroll.OverASession)
	if err != nil {
		return err
	}
	a.drop(removed)
	return nil
}

func (a *AgentAccess) drop(removed []appenroll.Removed) {
	if len(removed) == 0 {
		return
	}
	// The holds end at once: a hold must not outlive the grant that set it.
	for _, gone := range removed {
		a.EndHolds(gone.ID)
	}
	if a.Drop == nil {
		return
	}
	go func() {
		for _, gone := range removed {
			a.Drop(gone.Key)
		}
	}()
}

// EndHolds releases every hold the agent set.
func (a *AgentAccess) EndHolds(agentID string) {
	if a.Holds != nil {
		a.Holds(apiauth.KindAgent + ":" + agentID)
	}
}

// ExpireAgents removes the rows that have run out and ends their sessions.
// The caller runs it now and then; each call is also checked on its own.
func (a *AgentAccess) ExpireAgents() []appenroll.Removed {
	removed := a.Enroll.ExpireAgents()
	a.drop(removed)
	return removed
}

func (a *AgentAccess) Agents() []protocol.AgentRow { return a.Enroll.Agents() }

func (a *AgentAccess) AgentInvites() []protocol.AgentInvite { return a.Enroll.AgentInvites() }
func (a *AgentAccess) CreateAgentInvite(deviceID string, m protocol.AgentInviteCreate) (protocol.AgentInvite, error) {
	return a.Enroll.CreateAgentInvite(approver(deviceID), m)
}
func (a *AgentAccess) CancelAgentInvite(id string) (protocol.AgentInvite, error) {
	return a.Enroll.CancelAgentInvite(id)
}

func (a *AgentAccess) ApprovalKeys() []protocol.ApprovalKeyInfo { return a.Enroll.ApprovalKeys() }

func (a *AgentAccess) SetApprovalKey(deviceID string, m protocol.ApprovalKeySet) (int64, error) {
	return a.Enroll.SetApprovalKey(deviceID, m)
}

func (a *AgentAccess) CheckConfirm(deviceID string, m protocol.AgentConfirm, req protocol.Request) error {
	return a.Enroll.CheckConfirm(deviceID, m, req)
}

func (a *AgentAccess) Wall() (int64, bool) {
	if a.Clock == nil {
		return 0, false
	}
	now, ok := a.Clock()
	return now.UnixMilli(), ok
}

func (a *AgentAccess) Notify(kind, client string) { a.notify(kind, client) }

func (a *AgentAccess) notify(kind, client string) {
	if a.Push != nil {
		a.Push(kind, client)
	}
}
