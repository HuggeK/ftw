---
"ftw": minor
---

The box keeps cars apart from its chargers. A car's level is its own: a reading from the car stays exact until 1 kWh has reached it, however old the reading, so a nine-minute-old 14 % with nothing delivered since shows as "from the car". The box knows which car is on a charger from the charger's identifier, the car's own report, the only car, or one tap after it asks (`car.which`), and the plan takes that car's capacity, goal, surplus choice and power limit. On first start the box makes Car 1 from each charger's settings and a car from each vehicle profile, and binds each vehicle driver by VIN or to the only car. The charger routes and ops act on the session's car for this release, so apps already installed keep working.
