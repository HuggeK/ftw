---
"ftw": patch
---

When the planning style asks for forecast caution and the plan has to use the point forecast instead, the plan now says so: the box page shows "Caution off" and the app's plan message carries `cautionOff`. It clears once caution works again. A cautious plan is no longer refused just because the car charges while the battery discharges: dispatch already keeps the battery off a protected car. With a car plugged in, an accepted cautious plan now takes one Energyplan solve instead of two; a refused one still takes two. The forecast archive no longer credits the live meter's current load to Core's recovery, and a lasting caution fault no longer rewrites its failure record on every replan.
