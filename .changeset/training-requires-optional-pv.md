---
"ftw": patch
---

Keep load-model training strict on hybrid inverters whose PV reading is optional. While such a PV source has not reported, Home use still shows the house, but the interval does not train the forecast; after dark the PV counts as 0 W and the night trains as before.
