---
"ftw": patch
---

FTW Cloud's copy carries each car's level and goal as the `car` class, by an opaque id and with no name, VIN or identifier, when it changes and in full every 15 minutes. A charger's level and goal fields in the copy now read from its session's car, so phones that know cars and phones that know only chargers see the same numbers.
