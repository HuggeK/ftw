---
"ftw": patch
---

Keep Home use visible when an energy average is unavailable but Core has a valid power balance. Label that estimate as the latest power readings. Keep missing or invalid load unknown.

Let a confirmed disconnected charger contribute a separate zero-load estimate between polls, even when it retains old power and session timestamps. Break that interval on reconnect, missing evidence, changed state or a gap.
