---
"ftw": minor
---

A phone whose session runs through FTW Cloud now gets a frame as each driver poll lands, instead of on the box's next one-second pulse, with a beat each second when no poll comes. A frame goes on every poll whether or not a value changed, so its timing shows the poll rhythm and never which reading moved. While a phone watches, the readable copy's `live` also goes when a reading lands, still at most once a second. Sessions over the relay keep their fixed cadence. Lane 0's invariant changes from "fixed length and cadence" to "fixed length; fixed cadence over the relay".
