# Live site status

The overview shows grid, PV and battery power per device and whether each
device is reporting.

## Sub-features

- Per-driver status (`ok`, `degraded`, `offline`).
- Site grid power from the meter driver (`is_site_meter: true`).

## How to get to it (user POV)

Open the web UI. The overview card is the first screen.

## Driving it with curl

`launch.sh` waits for it. `doctor.sh` prints it. Directly:
`curl -s http://127.0.0.1:18080/api/status`. Proof is `drivers.<name>.status
== "ok"` for both simulators and a numeric `grid_w`.

## Gotchas

Readings are positive into the site and negative out of it. The simulators
export PV in daylight, so negative `pv_w` is normal.
