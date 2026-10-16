---
"ftw": patch
---

The FTW Cloud uplink tries QUIC on UDP 443 first and falls back to the WebSocket when UDP does not get through within 250 ms, remembering per network which one opened. A QUIC session that ends before `hello_ok`, or that the path drops within a minute of it, puts the WebSocket first on that network for a day. Over QUIC, `hb` and a `live` without its source table go as datagrams, and the tunnel's bulk on a stream of its own, so history never holds up a command. Noise sessions accept frames up to 1024 out of order, since bulk can overtake lane 0. `GET /api/cloud` says which transport carries the link. The registry names them in `cloud_transports`.
