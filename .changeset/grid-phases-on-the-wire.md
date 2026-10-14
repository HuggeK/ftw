---
"ftw": patch
---

The box streams the grid's three phase currents to the app and to FTW Cloud, so the phone can show each phase against the main fuse. They go as extra fields 11–13 in tenths of an ampere, signed by the site convention, with the main fuse as field 14. A meter that gives only the size of each current, such as Sungrow, gets its sign from the power on the same phase. The box sends only the phases the site has, and only while the site meter is online. When a field stops coming, the box resends the whole snapshot once, so a phone never keeps a phase at its last value. Lane 0 keeps its 512-byte frame, and these fields are the first to give way when a delta does not fit.
