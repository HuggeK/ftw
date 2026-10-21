---
"ftw": patch
---

Home use is marked "≈" as soon as a device stops answering. Before, Core kept the device's last power for up to 90 s and still called Home use measured, so the page could show a wrong value and "% self-powered now" while every watt came from the grid. The last value is still shown until the age limit, but as an estimate that names the device. A device the watchdog has taken offline now stays offline until it delivers data again. Each failed read used to bring it back for a moment, so Home use flipped every few seconds and the watchdog reported the device lost again each time.
