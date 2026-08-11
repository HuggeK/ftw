---
"ftw": patch
---

Make the box-to-app link faster and cheaper. The first telemetry subscription can travel with the hello, hidden apps receive the promised 0.2 Hz cadence, and the box dashboard stops its two-second status poll while hidden. History work no longer blocks other app messages and reads each tile from SQLite once. Large API answers stream through a fixed chunk buffer instead of repeatedly copying the remaining body.
