---
"ftw": patch
---

The EV's hard limit now treats the main fuse as heat, as a fuse or breaker does. A short house spike, such as a heat pump or an oven starting, no longer cuts the charge. A phase above the fuse less 1 A cuts it only once that phase's current, weighted over about the last minute, passes the same limit, and at once above 1.2 times the fuse. The charge rises again only once that weighted current has room too, so repeated peaks cut it once instead of every minute. Where the meter reports phase currents, they replace total import as the fuse check, so a short spike on all three phases no longer cuts either. A tariff import ceiling still applies.
