package appproto

import (
	"github.com/srcfl/ftw/go/internal/config"
)

// setPlannerPrefs stores the household safety factor and battery-export
// permission. Which planner mode that permission selects is the port's
// answer, read back after the write — this handler never maps an export
// permission onto a mode of its own.
func (h *Handler) setPlannerPrefs(cmd Cmd, uptimeMs int64) error {
	prefs := h.cfg.PlannerPrefs
	if prefs == nil {
		return h.sendCmdResult(CmdResult{
			CmdID: cmd.CmdID,
			State: CmdRejected,
			Error: &ErrorBody{
				Code:      ErrUnavailable,
				Retryable: ErrorRetryable[ErrUnavailable],
				Args:      map[string]any{"op": cmd.Op},
			},
		})
	}

	var k *float64
	if _, present := cmd.Args["safety_k"]; present {
		value, ok := argNum(cmd.Args, "safety_k")
		if !ok {
			return h.rejectArg(cmd, "safety_k", cmd.Args["safety_k"])
		}
		k = &value
	}
	var export *string
	if raw, present := cmd.Args["battery_export"]; present {
		value, ok := raw.(string)
		if !ok {
			return h.rejectArg(cmd, "battery_export", raw)
		}
		if _, ok := config.ParseBatteryExport(value); !ok {
			return h.rejectArg(cmd, "battery_export", raw)
		}
		export = &value
	}
	if k == nil && export == nil {
		return h.rejectArg(cmd, "safety_k", nil)
	}

	if took, err := h.acceptCmd(cmd, uptimeMs); !took || err != nil {
		return err
	}

	snap, err := prefs.Apply(k, export)
	if err != nil {
		return h.settleAndReport(cmd.CmdID, CmdResult{
			CmdID: cmd.CmdID,
			State: CmdRejected,
			Error: &ErrorBody{
				Code:      ErrUnavailable,
				Retryable: ErrorRetryable[ErrUnavailable],
				Args:      map[string]any{"op": cmd.Op},
			},
		})
	}

	readAtMs := h.cfg.Clock.UptimeMs()
	return h.settleAndReport(cmd.CmdID, CmdResult{
		CmdID: cmd.CmdID,
		State: CmdApplied,
		Observed: &Observed{
			Value:    snap.SafetyK,
			Src:      ObservedSrcCore,
			UptimeMs: readAtMs,
		},
	})
}
