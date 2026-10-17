# Stale driver falls back to its default

A driver that stops reporting receives its autonomous default mode, and a
stale site meter stops dispatch (AGENTS.md invariants).

## Sub-features

- Watchdog marks the driver offline after `watchdog_timeout_s` (60 s in the
  simulator config).
- Core sends the driver's `driver_default_mode`.

## How to get to it (user POV)

Unplug a device. The overview marks it offline.

## Driving it with curl

Not scripted yet. Manual journey: launch, stop `sim-sungrow` by the pid on
line 2 of `$FTW_VERIFY_DIR/pids`, wait 65 s, then check
`drivers["sungrow-sim"].status` in `/api/status` and the Core log line
`watchdog → reverting to self-consumption`.

## Gotchas

Never stop the simulator by process name. Another agent's instance may run.
