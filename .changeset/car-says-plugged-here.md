---
"ftw": minor
---

A car can say it was plugged in here. Vehicle drivers may report when their source read the car's cable go in (`plugged_since_ms`) and how far the car is from the site (`site_distance_m`; up to 300 m is here, over 5 km is elsewhere). `host.site_location()` gives the site's position to a signed vehicle driver, so the car's coordinates stay in the driver; Home Assistant's `vehicle_json` leaves out where the car is. The box names a car by its own word even when the report arrives late, but not when two chargers saw a plug-in in the same minute, and never a car that was read elsewhere after the plug-in. A reading from before the plug-in no longer makes the only car a visitor, and a tap over the box's visitor verdict stays. The driver card shows a car's level in percent when the driver sends a fraction.
