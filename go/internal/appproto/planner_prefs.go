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

	k, export, bad := plannerPrefsArgs(cmd.Op, cmd.Args)
	if bad != nil {
		return h.rejectWith(cmd, bad)
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

// plannerPrefsArgs reads the prefs a planner.prefs.set carries: at least
// one, each of its kind.
func plannerPrefsArgs(op string, args map[string]any) (*float64, *string, *ErrorBody) {
	var k *float64
	if _, present := args["safety_k"]; present {
		value, ok := argNum(args, "safety_k")
		if !ok {
			return nil, nil, badArg(op, "safety_k", args["safety_k"])
		}
		k = &value
	}
	var export *string
	if raw, present := args["battery_export"]; present {
		value, ok := raw.(string)
		if !ok {
			return nil, nil, badArg(op, "battery_export", raw)
		}
		if _, ok := config.ParseBatteryExport(value); !ok {
			return nil, nil, badArg(op, "battery_export", raw)
		}
		export = &value
	}
	if k == nil && export == nil {
		return nil, nil, badArg(op, "safety_k", nil)
	}
	return k, export, nil
}
