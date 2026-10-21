---
"ftw": minor
---

Add a Home Assistant ingress mode. The HA app can use the owner's HA login for setup and settings, including installs with a saved FTW password. The ingress listener accepts Supervisor requests and local health checks; other clients cannot open the UI. Ordinary native installs keep their existing access rules.
