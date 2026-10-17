---
"ftw": patch
---

Stop the active Cloud session as soon as its off flag is saved, even if a
later storage write fails. Live data, hourly statistics and remote access
stop; the saved flag also keeps them off after a restart.
