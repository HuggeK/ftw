---
"ftw": patch
---

Home use is no longer marked estimated because an OCPP charger has no car. When the charger reports Available, no car is connected and no current flows, so its power counts as a measured 0 W in live Home use and in history. Each heartbeat or other message from the charger keeps that reading current, and it ends as soon as a car arrives. Unavailable and Faulted still leave the charger's power unknown. The Available a charger sends for itself (connector 0) at boot or reconnect no longer reads as an unplugged car. Core now asks OCPP chargers for a heartbeat every 30 s instead of every 60 s, so one late heartbeat no longer marks an idle charger offline: one more small request and reply a minute per charger. A heartbeat_interval_s set in the config is kept.
