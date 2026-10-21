---
"ftw": patch
---

Show Home use again when one device is offline. Live Home use is the energy balance as is: the site meter minus every device that is read, as before 0.141. A device that is offline, or a setup problem, no longer blanks it; the dashboard marks it "estimated" with "≈". A negative value is shown, since it points at production FTW cannot see. After sunset at the site's map pin an offline inverter counts as 0 W. Home use is blank only when the site meter is missing or stale. `/api/status` adds `load_quality` and `load_reason` as hints.

Home use now has two series. The live display value is logged as the `load_display_w` metric. The energy-based Home use from closed history intervals, with its quality flags, stays what load models train on. Home Assistant gets a second sensor, "Load Power (energy average)", next to "Load Power".
