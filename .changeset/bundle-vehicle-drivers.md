---
"ftw": minor
---

Bundle the vehicle drivers that say where a car is plugged in: `home_assistant_vehicle` (new: a car's level, cable and place from a Home Assistant automation over MQTT), `tesla_cloud`, `teslamate_vehicle` and `vag_vehicle`, and the updated `tesla_vehicle`. They report the charge cable, and where they can, whether the car is at this site, so the box can name the plugged-in car by its own word. A car at 1 % no longer reads as full. They do nothing until configured, and none wakes a car.
