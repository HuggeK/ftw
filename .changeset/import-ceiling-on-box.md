---
"ftw": patch
---

Switching the peak import limit on now applies the last limit the box had on, not a value one browser kept from earlier. The Grid dialog shows that value while the limit is off, and `/api/status` serves it as `peak_import_ceiling_last_w`. If the box has never had a limit on, switching on applies nothing until you pick a value and press Save, and Save now looks off while there is nothing to apply.
