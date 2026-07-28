---
"ftw": patch
---

Tell the header badge's two states apart at a glance.

A waiting update and a degraded optimizer both rendered as the same pulsing
amber mark, so a field tester read the optimizer warning as an update icon.
The warning now draws a static ringed `!` in the palette's dimmer amber,
while the update dot keeps its filled amber pulse. Shape, motion and weight
all differ, so the two stay apart without relying on colour.
