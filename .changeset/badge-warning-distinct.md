---
"ftw": patch
---

Give the header's status corner three distinct marks.

A waiting update and a degraded optimizer both rendered as the same pulsing
amber mark, so a field tester read the optimizer warning as an update icon.
An update now draws a blue download-to-drive icon and a degraded optimizer
an orange warning triangle, each fading gently; when neither is pending the
slot shows a steady green dot. Silhouette, colour and motion all differ, so
the marks stay apart without relying on colour alone.

An update and a degraded optimizer pending at once now show both marks —
previously the warning suppressed the update entirely. The separate
connection dot moves into the same slot, and a lost connection replaces the
other marks rather than sitting beside readings we may no longer be able to
refresh.
