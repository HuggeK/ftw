---
"ftw": patch
---

FTW Cloud tells every owner's phone when someone gains access to the box, asks for a change, is about to lose access, can now approve access, or pairs as an owner, with the readable copy on or off. The box's `notice` names the client on `agent.added` only while the copy is on and carries events, and these kinds no longer also go to FTW Cloud as events, so an owner gets one alert and a viewer none. The `agent.added` push now says access was given, not who gave it.
