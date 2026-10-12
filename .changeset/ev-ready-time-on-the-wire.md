---
"ftw": patch
---

Show and save the EV ready time on the household clock, even from a browser in another time zone. `GET /api/loadpoints` now sends `time_of_day_min`, minutes after midnight in the box's zone, and the schedule routes accept it. It wins over `time_of_day_min_utc`, which Core still sends and accepts for older clients.
