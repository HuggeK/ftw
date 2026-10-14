---
"ftw": minor
---

The plan an app reads (`plan.get` and the plan pushed after a command) now carries the battery's planned level at the end of each slot, `socPermille`, in thousandths: the same name and unit as the FTW Cloud plan class. A phone can then say where the battery ends up, not only what it does. A level the planner did not give as a number is left out. Older apps ignore the field.
