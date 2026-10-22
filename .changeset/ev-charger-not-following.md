---
"ftw": patch
---

A charger that keeps charging after FTW tells it to stop or slow down now shows as Not following, not Limited. In 0.155.0 an Easee Cloud charger that went on drawing 3.1 kW after FTW cut it to 0 W for the main fuse showed "Limited by the main fuse" for as long as it drew. A limit explains why FTW ordered less; it does not explain a charger that takes more. FTW now warns once the charger has drawn more than its order for four minutes, and raises an alarm ten minutes later. A car that takes less than a limited order is judged as it is under a plan order. Sending the same order on every tick no longer pushes the earlier order out of the two-minute response window, so a slow cloud charger is not judged on an order it could not yet have applied.
