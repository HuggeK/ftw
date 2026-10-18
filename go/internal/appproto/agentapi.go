package appproto

import (
	"context"
	"net/http"
	"sync"

	"github.com/srcfl/ftw/go/protocol"
)

type agentReadGateway interface {
	ServeAgentRead(http.ResponseWriter, *http.Request, protocol.APIRead)
}

func (h *Handler) onAPIRead(env Envelope) error {
	if env.ID == nil {
		return nil
	}
	read, err := protocol.DecodeAPIRead(env.B)
	if err != nil {
		return h.refuse(env.ID, ErrUnknownOp, map[string]any{"t": env.T})
	}
	known := protocol.ValidAgentAPIPath(read.Path)
	request := APIReq{Method: APIGet, Path: read.Path, Query: read.Query, MaxBytes: read.MaxBytes}
	if !known || badAPIRequest(request) != "" || read.MaxBytes < 0 || read.MaxBytes > protocol.AgentAPIPageBytes || read.Offset < 0 {
		return h.refuse(env.ID, ErrUnknownOp, map[string]any{"t": env.T})
	}
	gateway, available := h.cfg.API.(agentReadGateway)
	if !available {
		return h.refuse(env.ID, ErrUnavailable, map[string]any{"subsystem": "api"})
	}
	if !h.apiBusy.CompareAndSwap(false, true) {
		return h.refuse(env.ID, ErrUnavailable, map[string]any{"reason": "busy"})
	}
	id := *env.ID
	go func() {
		var once sync.Once
		free := func() { once.Do(func() { h.apiBusy.Store(false) }) }
		defer free()
		ctx, cancel := context.WithTimeout(h.ctx, apiHandlerTimeout)
		defer cancel()
		if h.isAgent() {
			_, refusal, end := h.agentGate()
			if refusal != nil {
				_ = h.sendError(&id, *refusal)
				if end != "" {
					_ = h.Terminate(end)
				}
				return
			}
		}
		caller, enrolled := h.liveCaller()
		if !enrolled || !caller.Scopes.Has(protocol.ScopeDiagRun) {
			_ = h.refuse(&id, ErrScopeDenied, map[string]any{"needScope": protocol.ScopeDiagRun})
			return
		}
		req := buildAPIRequest(ctx, caller, request)
		writer := &apiWriter{h: h, id: id, ctx: ctx, max: protocol.AgentAPIMaxBytes, free: free, header: http.Header{}, buf: make([]byte, 0, APIChunkBytes)}
		writer.recheck = func() *ErrorBody {
			if h.isAgent() {
				if _, refusal, _ := h.agentGate(); refusal != nil {
					return refusal
				}
			}
			current, ok := h.liveCaller()
			if !ok || !current.Scopes.Has(protocol.ScopeDiagRun) {
				return &ErrorBody{Code: ErrScopeDenied, Args: map[string]any{"needScope": protocol.ScopeDiagRun}}
			}
			return nil
		}
		func() {
			defer func() {
				if recover() != nil {
					writer.WriteHeader(http.StatusInternalServerError)
					_, _ = writer.Write([]byte(`{"error":"diagnostic read failed"}`))
				}
			}()
			gateway.ServeAgentRead(writer, req, read)
		}()
		if err := writer.finish(); err != nil {
			h.log.Warn("diagnostic API read failed", "err", err)
		}
	}()
	return nil
}
