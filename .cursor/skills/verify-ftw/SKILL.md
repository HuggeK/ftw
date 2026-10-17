---
name: verify-ftw
description: Drive a local FTW Core with the Ferroamp and Sungrow simulators through its HTTP API, the same API the web UI and app use, and capture evidence. Use before claiming that a Core, API or control change works, or when asked to prove FTW behaviour on a running instance.
---

# Verify FTW

FTW Core is a Go service with a web UI and a JSON API on one port. This
skill starts Core with two simulated sites (Ferroamp over MQTT, Sungrow
over Modbus TCP) on isolated ports and an empty state directory, drives a
feature through the API, and keeps the evidence.

The helpers live in `scripts/`. Run them from the repository root.

## Launch

```bash
.cursor/skills/verify-ftw/scripts/launch.sh
```

It needs Python 3, `lsof` and the repo's Go toolchain on macOS or Linux.
It builds `ftw`, `sim-ferroamp` and `sim-sungrow` with the repo's Go
toolchain, writes a config from `config.local.example.yaml` with the
ports below, and starts all three. It fetches the bundled drivers first if
`drivers/` is empty. Ready means all three recorded processes are alive,
Core owns the API port, both simulator drivers report `status: ok`, and
`grid_w` is numeric. A cold start takes about 15 seconds after the build.

| What | Default | Override |
|---|---|---|
| API and web UI | `127.0.0.1:18080` | `FTW_VERIFY_API_PORT` |
| Ferroamp MQTT broker | `127.0.0.1:21883` | `FTW_VERIFY_MQTT_PORT` |
| Sungrow Modbus TCP | `127.0.0.1:25502` | `FTW_VERIFY_MODBUS_PORT` |
| OCPP 1.6 / 2.0.1 | `18887` / `18888` | `FTW_VERIFY_OCPP_PORT`, `FTW_VERIFY_OCPP201_PORT` |
| Scratch state, logs, pids | `$TMPDIR/ftw-verify` | `FTW_VERIFY_DIR` |
| Evidence | `.verify-evidence/` | `FTW_VERIFY_EVIDENCE` |

Two instances can run side by side when every port and `FTW_VERIFY_DIR`
differ. `launch.sh` refuses occupied ports or an existing scratch directory.
It stops its child processes if startup fails and keeps the logs for diagnosis.

## Doctor

```bash
.cursor/skills/verify-ftw/scripts/doctor.sh
```

Read-only. It checks that every process this skill started is alive, that
our `ftw` process owns the API port, and prints each driver's status, the
grid power and the mode. Process checks include start time and command, so
a reused PID fails the check. Run it first whenever anything looks off.

## Drive

The web UI calls the same endpoints, so drive the API with `curl`. Paths are
registered in `go/internal/api/api.go` (`s.handle("POST /api/mode", ...)`).

- `GET /api/status` is the live site: `drivers`, `dispatch`,
  `control_feedback`, `mode`, `grid_w`.
- `GET /api/modes` lists valid modes; `POST /api/mode {"mode": "..."}`
  changes it.

`scripts/drive-mode.sh idle` runs the supported mode journey end to end.
Other modes need their own dispatch assertions. The helper checks process
identity and API-port ownership before each request, including the POST. See
`features/` for what each feature's end state must show.

## Evidence

Each drive writes a directory under `.verify-evidence/` with the request,
its time and response, `/api/status` before and after, and the Core log.
It requires both batteries in dispatch and feedback, accepted 0 W commands,
fresh readings, and command and observation times after this action.
A proof must:

- use the public API path, never internal setters or test-only endpoints;
- capture the action and the resulting state, including what Core sent to
  the devices (`dispatch[].target_w`, `control_feedback[].sent_w`), not only
  the mode string it echoes back;
- read state after at least two control ticks (`control_interval_s: 2`).

The simulators are the only mocks. They stand in for hardware behind the
same MQTT and Modbus boundary that real devices use.

## Cleanup

```bash
.cursor/skills/verify-ftw/scripts/cleanup.sh
```

It checks recorded process identities before signalling them and deletes the scratch state.
It never kills by process name and keeps `.verify-evidence/`.
It refuses reused PIDs or a run directory without a valid instance record.
If startup failed before writing that record, the launcher already stopped
its children; inspect the retained logs and choose a new scratch directory.

## Gotchas

- Core logs `site dispatch inhibited ... site_meter_stale` for the first
  seconds after start. That is the stale-meter safety rule, not a failure.
  Wait for `site dispatch freshness recovered`.
- The UI still needs a human in a browser for visual changes (AGENTS.md).
  This skill proves behaviour, not appearance.
