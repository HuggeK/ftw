---
"ftw": patch
---

Train both load models on the closed quarter-hour household energy that daily Home use is built from. The built-in model no longer learns from one instant reading per minute, and the Energyplan forecast no longer keeps its own load collector, so forecasts learn the Home use shown in History.

Keep cold-day counters in their stored minute units when training moves to quarter-hours, preserving learned profiles and partial-day evidence.
