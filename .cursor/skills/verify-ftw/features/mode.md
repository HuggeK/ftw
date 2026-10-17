# Operating mode

The owner chooses how batteries behave: planner modes, self-consumption,
peak shaving, charge or stop.

## Sub-features

- `GET /api/modes` lists modes with label and tier.
- `POST /api/mode` changes the mode through `control.ApplyMode`.
- `idle` ("Stop batteries") holds every battery at 0 W.

## How to get to it (user POV)

Web UI mode selector, or the app.

## Driving it with curl

`.cursor/skills/verify-ftw/scripts/drive-mode.sh idle`. It passes when
`/api/status` reports `mode: idle`, every `dispatch[].target_w` is 0 and every
battery's `control_feedback[].sent_w` is 0 after three control ticks.
Both named batteries must appear exactly once in each list. The feedback
must show an accepted command and fresh readings, with command and
observation times after this request. Missing feedback is a failure.

## Gotchas

The response `{"mode":"idle","status":"ok"}` only says the request was
accepted. The proof is the dispatch and sent power afterwards.
