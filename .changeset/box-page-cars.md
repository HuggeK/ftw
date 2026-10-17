---
"ftw": minor
---

The box's own page shows the owner's cars. The overview has a card for each car, plugged in or not, with its level, where the level came from and how old it is ("72% · From the car · 1 h ago", or "≈" once it is an estimate), its charge limit, its goal and the charger it is on. When a charger is in use and the box cannot tell which car it is, the charger's notice asks "Which car is on garage?" with one button per car and "None of these"; a car the box named itself can be corrected the same way. Settings has a Cars tab that lists each car with its VIN, its level sources and the tags a charger knows it by, and lets the owner rename a car, set its battery size, add one and remove one.

New routes carry the car ops for the page: `POST /api/cars`, `PATCH` and `DELETE /api/cars/{id}` (owner only), and `POST /api/loadpoints/{id}/car`, the tap (`session.car.set`).
