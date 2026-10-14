---
"ftw": patch
---

Honour the interval a driver returns from `driver_poll`, as the driver contract says. Core ignored it before and polled every five seconds. The returned wait now sets the next poll, between one second and thirty seconds. A driver that calls `host.set_poll_interval` keeps that interval, as before, whatever it returns.

Bundled drivers that return one second now read once a second: ambibox_v2x, ferroamp, ferroamp_dc2_v2x, pixii_pv, sdm630, sonnen and zap. myuplink reads every thirty seconds instead of every five. easee_cloud, tesla_wall_connector and zaptec_cloud wait ten seconds after a failed read, and solaredge_legacy thirty seconds after a failed SunSpec probe.
