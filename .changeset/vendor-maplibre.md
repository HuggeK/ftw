---
"ftw": patch
---

The weather map's MapLibre GL JS is vendored on the box instead of loaded
from unpkg, so the location picker works without internet access and the UI
executes no third-party CDN JavaScript — the same policy that vendored
Leaflet, whose now-unused copy is removed.
