---
"ftw": patch
---

Home use on the energy flow no longer shows "≈" when every device is read, and "% self-powered now" is back. The page marked Home use as an estimate whenever /api/status carried a second, energy-averaged value, which it always did. It now follows Core's `load_quality` alone, and /api/status no longer sends `load_estimate`.

Home use has one value. 0.154.3 was never published as a beta; its change also ships here: Home use is the site meter minus every device that is read, shown when one device is offline (marked "≈"), and blank only when the site meter is missing or stale. The Home Assistant sensor "Load Power (energy average)" announced for 0.154.3 is removed before release; "Load Power" stays.
