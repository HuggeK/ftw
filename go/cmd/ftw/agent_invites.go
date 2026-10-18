package main

import (
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/protocol"
)

func (b boxPageAgents) AgentInvites() []protocol.AgentInvite { return b.p.access.Enroll.AgentInvites() }

func (b boxPageAgents) CreateAgentInvite(by string, m protocol.AgentInviteCreate) (protocol.AgentInvite, error) {
	v, err := b.p.access.Enroll.CreateAgentInvite(appenroll.AtTheBoxPage, m)
	b.record(by, protocol.MsgAgentInviteCreate, m.ID, err)
	return v, err
}

func (b boxPageAgents) CancelAgentInvite(by, id string) (protocol.AgentInvite, error) {
	v, err := b.p.access.Enroll.CancelAgentInvite(id)
	b.record(by, protocol.MsgAgentInviteCancel, id, err)
	return v, err
}

func (b boxPageAgents) PrepareAgentInviteHome() (string, error) {
	return b.p.access.Enroll.PrepareAgentInviteHome()
}
