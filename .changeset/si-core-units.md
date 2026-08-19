---
"ftw": minor
---

Core stores power in watts, energy in watt-hours, SoC as 0–1, and PV arrays as rated watts. kWp and 0–100 percents remain only at UI, Home Assistant, appproto, and the forecast.solar URL. Pasting watts into the old kWp field is converted on config load. Heat-pump diagnostics emit W/Wh.
