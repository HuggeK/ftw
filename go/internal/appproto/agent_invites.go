package appproto

import "github.com/srcfl/ftw/go/protocol"

type InviteAccess interface {
	AgentInvites() []protocol.AgentInvite
	CreateAgentInvite(deviceID string, m protocol.AgentInviteCreate) (protocol.AgentInvite, error)
	CancelAgentInvite(id string) (protocol.AgentInvite, error)
}

func (h *Handler) onAgentInvite(env Envelope) error {
	if _, ok, err := h.ownerPhone(env.ID); !ok {
		return err
	}
	a, ok := h.cfg.Access.(InviteAccess)
	if !ok {
		return h.refuse(env.ID, ErrUnavailable, nil)
	}
	if env.T == protocol.MsgAgentInviteList {
		now, ready := h.wall()
		return h.agentOK(env.ID, map[string]any{"invitations": a.AgentInvites(), "nowMs": now, "clockReady": ready})
	}
	var v protocol.AgentInvite
	var err error
	if env.T == protocol.MsgAgentInviteCreate {
		var m protocol.AgentInviteCreate
		if protocol.UnmarshalSigned(env.B, &m) != nil {
			return h.refuse(env.ID, ErrUnknownOp, map[string]any{"field": "body"})
		}
		v, err = a.CreateAgentInvite(h.selfID(), m)
	} else {
		var m protocol.AgentInviteCancel
		if protocol.UnmarshalSigned(env.B, &m) != nil {
			return h.refuse(env.ID, ErrUnknownOp, map[string]any{"field": "body"})
		}
		v, err = a.CancelAgentInvite(m.ID)
	}
	h.audit("", env.T, v.ID, resultOf(err))
	if err != nil {
		return h.refuseErr(env.ID, err)
	}
	return h.agentOK(env.ID, map[string]any{"invitation": v})
}
