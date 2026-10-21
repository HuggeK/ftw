---
"ftw": patch
---

EV charging no longer pushes a phase past the fuse when a charger is slow to apply a new current, when a charge moves between three phases and one, or when one car is raised while another is cut. Each raise now counts as current on the phases its command names until the charger can have applied it: the time its driver declares, or 3 minutes when it declares none. A car takes only room that no other charger's raise holds. After a rise, a charger that declares no response time may wait up to 3 minutes before it climbs further.
