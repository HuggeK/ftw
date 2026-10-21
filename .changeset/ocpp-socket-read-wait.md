---
"ftw": patch
---

An idle OCPP charger no longer loses its connection about once a minute. The OCPP library closed a charger socket that had sent nothing for 60 s, the same as a 60 s heartbeat, so a charger that only sends heartbeats could be dropped as each one arrived, reconnect, and briefly show Home use as "≈". A charger socket may now stay silent for five heartbeat intervals (150 s with the default 30 s heartbeat). FTW's own watchdog still decides when a quiet charger is offline.
