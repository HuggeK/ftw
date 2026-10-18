---
"ftw": patch
---

The box now takes the app's next API request as soon as the previous answer has ended. Before, it freed its one request slot only after it had sent the answer's last frame. An app that asked again at once could be told the box was busy and then wait out a backoff before the view filled in.
