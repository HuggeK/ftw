---
"ftw": patch
---

Show Core's current power balance as Home use in Power now. Show the energy mean separately with its own period and age, so a missing or old mean cannot hide current load or replace it with a past zero. Keep missing or invalid current load unknown.

Use measured AC power for the live site balance when a driver such as Pixii reports both battery DC and site-side AC power. Do not substitute DC when the AC reading is unavailable.

Let a confirmed disconnected charger contribute a separate zero-load estimate between polls, even when it retains old power and session timestamps. Break that interval on reconnect, missing evidence, changed state or a gap.
