package appproto

import (
	"context"
	"net/http"
	"sync"

	"github.com/srcfl/ftw/go/protocol"
)

type setupGateway interface {
	ServeAgentSetup(http.ResponseWriter, *http.Request, string, protocol.SetupCall, func() error)
}

func (h *Handler) onSetupCall(env Envelope) error {
	if env.ID == nil {
		return nil
	}
	call, err := protocol.DecodeSetupCall(env.B)
	if err != nil {
		return h.refuse(env.ID, ErrUnknownOp, map[string]any{"t": env.T})
	}
	gateway, available := h.cfg.API.(setupGateway)
	if !available {
		return h.refuse(env.ID, ErrUnavailable, map[string]any{"subsystem": "setup"})
	}
	if !h.apiBusy.CompareAndSwap(false, true) {
		return h.refuse(env.ID, ErrUnavailable, map[string]any{"reason": "busy"})
	}
	id := *env.ID
	go func() {
		var once sync.Once
		free := func() { once.Do(func() { h.apiBusy.Store(false) }) }
		defer free()
		ctx, cancel := context.WithTimeout(h.ctx, protocol.AgentAPIReadTimeout)
		defer cancel()
		authority := func() *ErrorBody {
			if _, refusal, _ := h.agentGate(); refusal != nil {
				return refusal
			}
			caller, ok := h.liveCaller()
			if !ok || !caller.Scopes.Has(protocol.RegistryAgentCalls[env.T]) {
				return &ErrorBody{Code: ErrScopeDenied, Args: map[string]any{"needScope": protocol.RegistryAgentCalls[env.T]}}
			}
			return nil
		}
		check := func() *ErrorBody {
			if ctx.Err() != nil {
				return &ErrorBody{Code: ErrUnavailable}
			}
			return authority()
		}
		if refusal := check(); refusal != nil {
			_ = h.sendError(&id, *refusal)
			return
		}
		caller, _ := h.liveCaller()
		req := buildAPIRequest(ctx, caller, APIReq{Method: APIGet, Path: "/api/setup/draft"})
		writer := &apiWriter{h: h, id: id, ctx: ctx, max: protocol.AgentAPIMaxBytes, free: free, header: http.Header{}, buf: make([]byte, 0, APIChunkBytes), recheck: check}
		func() {
			defer func() {
				if recover() != nil {
					writer.panicked = true
				}
			}()
			gateway.ServeAgentSetup(writer, req, env.T, call, func() error {
				// An accepted restart rechecks the live grant after the reply.
				// That check must survive this short request's completion.
				if refusal := authority(); refusal != nil {
					return protocol.Refuse(refusal.Code, refusal.Args)
				}
				return nil
			})
		}()
		result := "ok"
		if writer.status >= 400 || writer.panicked {
			result = "refused"
		}
		h.audit(env.By, env.T, "", result)
		if err := writer.finish(); err != nil {
			h.log.Warn("setup reply ended", "err", err)
		}
	}()
	return nil
}
