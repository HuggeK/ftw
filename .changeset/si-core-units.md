---
"ftw": minor
---

Core stores power in watts, energy in watt-hours, SoC as 0–1, and PV arrays as rated watts. kWp and 0–100 percents remain only at UI, Home Assistant, appproto, calendar titles, and the forecast.solar URL. Loadpoint, calendar, vehicle telemetry, and V2X envelopes use 0–1 without `_pct` names. Pasting watts into the old kWp field is converted on config load. Heat-pump diagnostics emit W/Wh.
