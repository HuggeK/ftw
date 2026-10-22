---
"ftw": patch
---

Charging starts again promptly after a pause. In 0.155.0, an Easee Cloud charger that was paused and then asked to start waited up to three minutes, shown as limited by the main fuse, while the phases had plenty of room: FTW still counted the higher current it had ordered before the pause as on its way. A pause now ends that reservation, because the charger follows its latest order and the site meter shows its draw until the pause lands. In the reported case the restart now comes after 12 s instead of 2 min 47 s.
