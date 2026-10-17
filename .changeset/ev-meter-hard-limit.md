---
"ftw": patch
---

The EV's hard limit now reads only the site meter and FTW's own commands, never the charger's power readings. A charge is cut by the overage the meter measures on the worst phase or in total import, holds while the site is inside its limits, and rises one step at a time once the meter shows the last one. After a stop it restarts only when the meter has shown room for 35 seconds, and a Core restart keeps FTW's last offer while the same charging session is still plugged in. A late, missing or lone 0 W reading from the charger can no longer stop a charge, and an import ceiling no longer makes the charger start and stop every half minute.
