# FTW Cloud protocol

Proposed with [ADR 0008](adr/0008-ftw-cloud.md). Nothing here has shipped.
This document is normative: "must" is a requirement on the box, the service
or the phone. The bytes live in `go/protocol/cloudwire`, with golden vectors
in its `testdata`: `cloud-vectors.json` for the uplink and
`cloud-app-vectors.json` for one whole app session. `srcfl/ftw-cloud` and the
phone replay them. `cloudwire.View` is the phone's
[freshness](#freshness) rules in Go, and the cases listed there are its tests.

The app protocol, which runs inside Noise between a phone and its box, is
specified in
[srcfl/ftw-webapp docs/protocol.md](https://github.com/srcfl/ftw-webapp/blob/main/docs/protocol.md).
FTW Cloud reuses its frame layout, CBOR rules, field encoding and tile format.
It adds the `cloud.link` capability and one optional field to the app
protocol's `hello_ok`, `cloud` (see
[Session information](#session-information)), and changes nothing else. Lane 0 and the relay stay as they are. The Go types
for both live in `go/protocol`, under Apache-2.0
([#1588](https://github.com/srcfl/ftw/pull/1588)).

## Parties and connections

- **Box:** FTW Core with an FTW Cloud grant.
- **Service:** `ftwcloudd`, at `cloud.ftw.energy`.
- **Phone:** the iPhone app, paired with the box over Noise as today.
- **Extension:** a widget, notification service extension or Live Activity
  on that phone.

| Connection | Opened by | Transport | What protects it |
|---|---|---|---|
| Uplink | Box | `wss://cloud.ftw.energy/v1/box` | TLS 1.3, a handshake bound to the TLS session on both sides, every box message signed |
| App session | Phone | `wss://cloud.ftw.energy/v1/app?home=<id>` | TLS 1.3, then Noise IK to the service's static key |
| Extension read | Extension | `GET https://cloud.ftw.energy/v1/now` | TLS and an extension token; the data inside is box-signed |
| Push | Service | APNs | Box-signed payloads where they carry data |

**Uplink rules.**

- The endpoint is a compile-time constant in Core, as the relay's is.
- TLS 1.3 is the minimum. The dialer uses no proxy, whatever the environment
  says.
- No proxy may end TLS in front of the service, because that breaks the
  handshake binding. A plain TCP load balancer is fine.
- Reconnects back off with full jitter from 0.5 s to 60 s. The attempt
  counter resets after 60 s connected.
- The service sends a WebSocket ping every 15 s. The box's read deadline is
  45 s and its write deadline 10 s.
- WebSocket compression is off.
- A box keeps one uplink. A second one from the same home replaces the first.

## Frames

Each WebSocket binary message carries exactly one frame in the app
protocol's layout:

```
offset  field    type     value on FTW Cloud
0       ver      u8       1
1       lane     u8       1, always
2       flags    u8       0
3       rsvd     u8       0
4       len      u16 BE   payload bytes
6       payload  u8[len]  CBOR envelope
```

- **No padding.** In the app protocol, lane 1 pads to bulk buckets of 1, 4 or
  16 KiB. FTW Cloud uses lane 1 without the buckets: the message is exactly
  `6 + len` bytes. Lane 0 never appears on an FTW Cloud connection.
- A receiver closes the connection on a lane 0 frame, a nonzero flag, bytes
  after `len`, or a text message.
- Inside the app session the same frames ride as Noise transport messages,
  one frame per message, also without padding.

The app protocol pads lane 0 to a fixed size and cadence because the relay
must learn nothing from the stream. The service reads the content, so padding
would hide only size and timing from the network, at about three times the
data. The network sees when messages go and how large they are, which shows
which class changed and when. The privacy notice says so (ADR 0008,
decision 5).

## Envelope and signature

```
Envelope = { t: text, b: bstr, sig: bstr }
```

- CBOR, core deterministic encoding, text keys. A map with a repeated key is
  refused.
- An empty list or map is `[]` or `{}`, never null. A number the box does not
  know is `null`. Of the box's fields, only `wallMs`, `sources` and
  `fieldSources` are ever left out.
- `b` is the body, itself deterministic CBOR, carried as a byte string so that
  the signature covers exact bytes and the service can forward them unchanged.
- `sig` is ES256 (ECDSA on P-256 with SHA-256), 64 bytes, `r ‖ s`
  big-endian.
- **Signed input:** the bytes `ftw.cloud.v1\0` (12 ASCII bytes and a zero
  byte), then the deterministic CBOR encoding of the array `[home, t, b]`,
  where `home` is the text home id.
- The box signs with `cloud.key`. The service signs `hello_ok` and `purged`
  with its service key, over the box's home id. Every other envelope the
  service sends, and every envelope between phone and service, has an empty
  `sig`.
- A receiver verifies over the bytes it received, never over a re-encoding.
- The box's public key reaches phones over Noise, beside the tables of opaque
  ids, the home id and the box's current `boot`, in the
  [session information](#session-information) every role receives. It never
  comes from the service.
- Receivers ignore unknown keys in a body, as in the app protocol. The service
  drops and counts an unknown `t` from the box, so a newer box works with an
  older service. An unknown `t` from the service closes the uplink.
- The service forwards a home's messages in the order it received them.

## Keys and ids

| Name | What it is |
|---|---|
| `cloud.key` | ES256 (P-256) key the box makes when an owner grants FTW Cloud. The box never backs it up |
| `pk` | Its public key as a 65-byte uncompressed SEC1 point |
| `home` | Lowercase base32, no padding, of the first 16 bytes of SHA-256(`ftw.cloud.home.v1` ‖ `pk`). 26 characters |
| Service key | P-256. Core carries up to two public halves, so the key can rotate across a release |
| Service Noise key | X25519 static key. The app carries up to two |
| `grantKey` | 32 random bytes in the grant record. A new grant draws a new one |
| Opaque source id | First 8 bytes of HMAC-SHA256(`grantKey`, `src\0` ‖ driver name), base64url without padding |
| Opaque loadpoint id | First 8 bytes of HMAC-SHA256(`grantKey`, `lp\0` ‖ loadpoint id), base64url without padding |

A home id comes only from the key that signed `hello` on this connection,
never from a body field.

## Common body fields

Every body the box sends carries these four fields. The class tables below do
not repeat them.

| Field | Type | Meaning |
|---|---|---|
| `seq` | uint | Starts at 1 and rises by 1 per message, per boot, across reconnects |
| `boot` | bstr, 16 B | Random, drawn at each Core start |
| `uptimeMs` | uint | Box monotonic time since start, at send |
| `wallMs` | uint | Box wall time at send. Present only while NTP has synced the box clock |

Other wall-time fields, such as `readyByMs` and `t0Ms`, come from the box's
clock. While the box reports `clock: "none"`, the phone shows them as
unverified.

## Handshake

Both sides bind the handshake to the TLS session with its exporter value
(RFC 8446 §7.5), label `EXPERIMENTAL-ftw-cloud-v1`, empty context, 32 bytes,
called `ekm` below. A proxy that ends TLS in the middle holds two TLS sessions
with two different values, so neither side's check passes through it.

1. **Service → box: `challenge`** `{v: 1, nonce: bstr32}`, unsigned.
2. **Box → service: `hello`**, signed. The service computes `home` from `pk`,
   checks the signature, that `nonce` is its own and that `ekm` equals its own
   exporter value, or closes.

   | Field | Type | Meaning |
   |---|---|---|
   | `v` | uint | 1 |
   | `pk` | bstr, 65 B | The box's public key |
   | `nonce` | bstr, 32 B | Echo of the challenge |
   | `ekm` | bstr, 32 B | The box's exporter value for this TLS session |
   | `boxNonce` | bstr, 32 B | Random, for the service to sign |
   | `build` | text | Core version |
   | `clock` | text | `ntp` or `none` |
   | `caps` | [text] | The classes this box sends under the grant, `roster` included |
   | `rosterSeq` | uint | The box's current roster number |
   | `grantId` | bstr, 16 B | The grant this connection serves |

3. **Service → box: `hello_ok`**, signed by the service key over the box's
   home id.

   | Field | Type | Meaning |
   |---|---|---|
   | `boxNonce` | bstr, 32 B | Echo |
   | `ekm` | bstr, 32 B | The service's exporter value for this TLS session |
   | `have` | map text→text | Tile id to etag, for the tiles the service holds |
   | `rosterSeq` | uint | The roster number the service holds |

   The box checks the signature against a service key it carries, that
   `boxNonce` is its own and that `ekm` equals its own exporter value. If any
   check fails, the box closes and backs off. **It sends nothing but `hello`
   before this check passes.** A proxy that reads `hello` learns the box's
   public key and grant id, and cannot use them.
4. **Box → service:** a `roster`, at every connect (see [Roster](#roster));
   then one full message of each class the grant allows; then the tiles
   missing from `have`. While a purge is pending, the box sends only `bye`
   instead (see [Revocation](#revocation-and-deletion)).
5. After that the box sends on change, by the cadence rules.

## Classes

The box sends these. Each class belongs to one scope, and the service
forwards a class only to devices whose effective scope includes it. These
tables are the allowlist: a field the box sends must have a row here, and a
test fails on any field without one. `cloudwire.Fields` holds the same
tables, and `TestFieldsMatchTheProtocolDocument` compares the two. It reads
the rows under each class heading, each bold shape name and each numbered
handshake step, so keep that layout. Driver names and device capabilities
have no rows. Source kinds and counts, health counts and the statistics `has`
list still show what kinds of equipment a site has.

### `live`: `ftw.live.read`

| Field | Type | Meaning |
|---|---|---|
| `fields` | map text→int | Frozen field ids 1–6 and 10, keyed and encoded as the app protocol's `delta` does: field 1 is the mode index, power in W |
| `fieldSources` | map text→[text] | The sources each field's freshness follows. Sent only with `sources` |
| `sources` | map text→Source | The source table. Sent whenever `srcRev` changes, and in every full message |
| `srcRev` | uint | Rises by 1, per boot, each time the source table changes |
| `ages` | map text→uint | For each source in state `live` or `lagging`, the age of its newest reading at send, in ms of box uptime |
| `dispatchBlockedBy` | [text] | Sources whose staleness stops dispatch |

**Source**

| Field | Type | Meaning |
|---|---|---|
| `kind` | text | As in the app protocol |
| `state` | text | `live`, `lagging`, `stale`, `down` or `never`, as the box grades it |
| `lastOkMs` | int | Box uptime at the last good reading, when the table was sent |
| `staleAfterMs` | uint | How long a reading from this source stays current |

### `lp`: `ftw.live.read`

| Field | Type | Meaning |
|---|---|---|
| `controlRev` | uint | The box's control revision, as a `cmd` guard needs it |
| `loadpoints` | [Loadpoint] | One per loadpoint |
| `feedback` | [Feedback] | One per device function FTW commands |

**Loadpoint**

| Field | Type | Meaning |
|---|---|---|
| `id` | text | Opaque loadpoint id |
| `src` | text | Opaque source id of the charger, whose freshness the loadpoint follows |
| `plugged` | bool | A car is connected |
| `socPermille` | uint or null | Car charge level, null when unplugged |
| `socSource` | text | `vehicle`, `inferred` or `assumed`; empty when unplugged |
| `socAgeS` | uint or null | Seconds since the car last reported the level, at send. Null unless `socSource` is `vehicle` |
| `targetPermille` | uint or null | The level to reach |
| `readyByMs` | int or null | Wall time the car should be ready |
| `recurring` | bool | The target comes from a weekly schedule |
| `powerW` | int | Charging power, positive while charging |
| `sessionWh` | uint | Energy delivered this session |
| `session` | text | The number of plug-ins at this loadpoint since the box started, as text; empty when unplugged. With `boot` it names one plug-in. Core has no session id yet |
| `manual` | Manual or null | Manual charging state, null when none |
| `commandedW` | int or null | Power last commanded |
| `commandedReason` | text | Stable code for that command |
| `surplusOnly` | bool | Charging from surplus solar only |
| `gridDeferred` | bool | Grid charging deferred by the plan |
| `planPending` | bool | A replan is due |
| `boost` | bool | Battery boost is on |
| `nextStartMs` | int or null | Wall time the next planned charge starts |
| `nextEndMs` | int or null | Wall time it ends |
| `nextWh` | uint or null | Energy it plans to deliver |

**Manual**

| Field | Type | Meaning |
|---|---|---|
| `state` | text | The manual state |
| `sinceAgeMs` | uint | Time in that state, at send |
| `requestedW` | int or null | Power the user asked for |
| `commandedW` | int or null | Power commanded for it; null when none or zero |
| `reason` | text | Why the hold is limited, as a stable code |

**Feedback**

| Field | Type | Meaning |
|---|---|---|
| `kind` | text | The device function, such as battery or charger |
| `src` | text | Opaque source id |
| `status` | text | `following`, `waiting`, `limited`, `not_following`, `no_contact` or `not_controlled` |
| `reason` | text | Stable code |
| `severity` | text | As on the box |
| `evidence` | text | `none`, `accepted`, `measured` or `confirmed` |
| `fresh` | bool | The readings behind the status are fresh |
| `actualW` | int or null | Measured power |
| `ageMs` | uint or null | Age of that reading by box uptime, at send |

The change test ignores `socAgeS`, `sinceAgeMs` and `ageMs`; the phone adds
the time since the message. It compares measured values with a deadband,
because a charging car moves its power at every reading:

- `powerW` and `actualW` count as changed when they move by more than
  max(100 W, 5 % of the larger value), or `actualW` appears or goes;
- `sessionWh` when it moves by 100 Wh or more;
- `socPermille` when it moves by 10 or more, or appears or goes.

Any other difference counts as a change. The full `lp` every 15 minutes
carries exact values, and live field 10 carries the site's charging power at
every period. Without the deadband, a simulated house with a car charging at
11 kW sent 12.7 MB a day.

### `prices`: `ftw.live.read`

| Field | Type | Meaning |
|---|---|---|
| `zone` | text | Price zone |
| `currency` | text | ISO 4217 code |
| `t0Ms` | int | Wall time the first slot starts: the first stored slot from the start of today on the box's clock |
| `slotMs` | uint | Slot length. Where stored slots differ in length, the box repeats a longer slot's price across `slotMs` slots |
| `spotMinor` | [int or null] | Market price, minor units per kWh |
| `totalMinor` | [int or null] | What the household pays, tariff and VAT included |

`totalMinor` shows the household's tariff, and the privacy notice says so. The
phone sees how far the slots reach. The window runs from the start of today to
the end of tomorrow. Because it starts at midnight and not now, its hash moves
only when new prices land. The box looks at prices once a minute and sends
`prices` only when a hash of `t0Ms`, `slotMs`, `spotMinor` and `totalMinor`
changes. A gap between stored slots is `null`.

### `plan`: `ftw.plan.read`

| Field | Type | Meaning |
|---|---|---|
| `decisionId` | text | The plan's id. The box sends `plan` only when it changes |
| `genMs` | int | Wall time the plan was made |
| `stale` | bool | The planner could not run |
| `fallback` | bool | Core's Go fallback made this plan, not the solver |
| `t0Ms` | int | Wall time the first slot starts |
| `slotMs` | uint | Slot length |
| `batteryW` | [int or null] | Battery power per slot, site convention |
| `gridW` | [int or null] | Grid power per slot |
| `socPermille` | [uint or null] | Planned battery level at slot end |
| `evW` | [int or null] | Planned charging power |
| `pvLimitW` | [int or null] | Planned solar limit, null where none |
| `priceMinor` | [int or null] | Import price, minor units per kWh |
| `reason` | [text] | Stable code per slot |

The grid ceiling the plan defends is not sent.

### `health`: `ftw.health.read`

| Field | Type | Meaning |
|---|---|---|
| `ok` | uint | Drivers working |
| `degraded` | uint | Drivers working with faults |
| `faulted` | uint | Drivers failing |
| `offline` | uint | Drivers not answering |

### `event`: `ftw.events.read`

| Field | Type | Meaning |
|---|---|---|
| `eventId` | text | Unique per event |
| `kind` | text | A kind from `contract/push-catalogue.yaml` |
| `severity` | text | As on the box |
| `args` | map text→(text or int) | The catalogue's placeholders, except names |
| `src` | text | Opaque source id, when the event concerns a device |
| `lp` | text | Opaque loadpoint id, when the event concerns a loadpoint |

No argument carries a driver or device name. Where the catalogue text has a
name placeholder, such as `{name}` in `driver.offline`, the box sends `src`
and the phone fills in the name from its table. The box sends an event only
when its own rules (enabled, cooldown) allow. Delivery is best effort.

### `tiles`: `ftw.history.read`

The body is the app protocol's `hist.chunk` body: `tileId`, `etag`, `res`,
`startMs`, `stepMs`, `series`, `data` and `partial`. A closed tile has the
same tile id, etag and bytes as the box serves over Noise.

- `res` is `5m`. The service keeps 48 hours.
- `series` is drawn from `grid_w`, `pv_w`, `battery_w` and `load_w`, which the
  box serves today, and `ev_w` and `battery_soc`, which C2 adds.
- When a 5-minute bucket closes, the box appends to the trailing tile. Only
  then does `startMs` differ from the Noise meaning: it is the first new
  bucket, and `partial` is true.

### `hb`: every device in the roster

| Field | Type | Meaning |
|---|---|---|
| `clock` | text | `ntp` or `none` |
| `srcRev` | uint | The source table revision now in force |
| `liveSeq` | uint | `seq` of the newest `live` the box has sent |
| `lpSeq` | uint | `seq` of the newest `lp` the box has sent, 0 if none |
| `ages` | map text→uint | As in `live`, at the time of this `hb` |

### Service-only classes

The service never forwards these to a phone.

**`roster`**

| Field | Type | Meaning |
|---|---|---|
| `rosterSeq` | uint | Persisted on the box. Rises by 1 at each pair, role change, revoke or grant change |
| `ttlS` | uint | 86,400. Counted from when the service received the roster |
| `devices` | [Device] | Every device paired with the box |
| `grant` | Grant | What the owner granted |

**Device**

| Field | Type | Meaning |
|---|---|---|
| `id` | text | The box's device id |
| `key` | bstr, 32 B | The device's X25519 Noise static key |
| `role` | text | `owner` or `viewer` |
| `scopes` | [text] | Scopes the box derives from the role. The service holds no role table |
| `epoch` | uint | The device's epoch on the box |

**Grant**

| Field | Type | Meaning |
|---|---|---|
| `id` | bstr, 16 B | Grant id |
| `scopes` | [text] | A subset of `cloud_scopes` |
| `purposes` | [text] | A subset of `cloud_purposes`: `serve` always, and `stats` while the owner contributes to statistics |
| `controller` | text | The data controller: `sourceful` unless a partner pairing flow sets another. No such flow exists yet |

**`stats`**, only while `grant.purposes` contains `stats`.

| Field | Type | Meaning |
|---|---|---|
| `statsId` | bstr, 16 B | Random, drawn by the box at consent |
| `policy` | text | Version of the consent text the owner accepted |
| `zone` | text | Price zone |
| `has` | [text] | Which of `battery`, `pv` and `ev_charger` the site has |
| `hours` | [Hour] | At most 168 per message |

**Hour.** `startMs`, then Wh totals for the site, rounded to 10 Wh, under the
energy ledger's flow names: `grid_import`, `grid_export`, `pv_generation`,
`battery_charge`, `battery_discharge`, `consumer_use`, `vehicle_charge` and
`vehicle_discharge`. A flow the site lacks is absent.

The service refuses `stats` for a `statsId` it has deleted, so an old grant
record cannot turn a withdrawn consent back on.

**`stats.withdraw`** `{statsId}`. The service deletes that id's rows within
24 hours.

**`bye`** `{why, purge}`. `why` is `revoked`, `shutdown` or `update`.
`purge: true` asks the service to delete the home. `why: "update"` holds
back `box.unreachable` until the box returns or an hour passes.

## Cadence

- The box keeps the latest value per class and sends a class only when its
  content changed. The change test ignores `lastOkMs`, as the app protocol's
  source comparison does, and the age fields named under `live` and `lp`.
  `lp` adds the deadband described there.
- Each class goes at most once per period: 5 s by default, 1 s while a
  `watch` is in force. `lp` goes at most every 2 s until a Pi 4 bench shows
  what composing it costs. The box reads its state no faster than a message
  could go.
- Two messages skip the period. The box looks at its source table every
  second, and a `live` whose table changed goes then, with `sources` and
  `fieldSources`. An `hb` goes when it is due.
- `plan` goes only when `decisionId` changes, `prices` only when its hash
  changes, `tiles` when a bucket closes, and `event` at once.
- Every 15 minutes, and after a `resync`, the box sends a full `live` with
  `sources` and `fieldSources`, and a full `lp`.
- **Heartbeat.** The box sends `hb` whenever neither `live` nor `hb` has left
  for H, where H = clamp(the smallest `staleAfterMs` among sources ÷ 2,
  2 s, 15 s), and 15 s when there are no sources. It sends no `hb` while a
  changed `live` or `lp` waits for its period, so an `hb` never vouches for
  values the box has not sent.

## Service to box

The service may send the box only these. Anything else, a frame over 4 KiB,
a text message or a lane 0 frame closes the uplink. A fuzz test covers it.

| `t` | Body | Rule |
|---|---|---|
| `challenge` | `{v, nonce}` | First message. Unsigned |
| `hello_ok` | See Handshake | Signed by the service key |
| `watch` | `{hz: 1}` | Raises the period to 1 s for 60 s. While any phone of the home looks, the service sends it at once, every 50 s after that, and again when the box connects. Without it the box returns to 5 s. The box ignores any other `hz` |
| `resync` | `{what, tiles}` | `what` holds `full`, `tiles` or `roster`; `tiles` lists tile ids. The box honours at most one per 5 minutes and ignores the rest |
| `purged` | `{atMs, what, grantId, statsId}` | Signed by the service key. `what` is `home` or `stats`. Sent once every deletion it names is done |

The box acts on none of these beyond what the table says. It takes no time,
config, update or instruction from the service.

## Backpressure and limits

- One writer per socket. While a write blocks, the newest value per class
  replaces the one waiting. Events wait in a ring of 32; when it is full the
  oldest drops and the box counts the drop.
- A write blocked for 10 s closes the socket.
- A reconnect sends full state and the missing tiles. It replays no old
  `live`, `lp` or `event`.
- **Byte budget.** The box counts the bytes it sent over a rolling 24 hours
  of uptime. Past 20 MB, four times the measured 5 MB, it sends only `event`,
  `roster`, `stats.withdraw` and `bye` until the count falls. It sends no
  `hb` then, so data on phones ages to stale rather than looking current.
  `GET /api/cloud` shows the count and whether the budget is reached.

## Roster

- The box sends a roster at every connect, on every change within one control
  tick, and every 6 hours. Each one renews `ttlS`, so a box that returns after
  20 hours offline does not have 4 hours left.
- If `hello_ok.rosterSeq` is above the box's own, the box moves its number past
  the service's and sends a new roster. A box whose state went back in time
  therefore cannot undo revokes the service already holds.
- The service accepts a roster whose `rosterSeq` is at least the one it
  holds. An equal number renews `ttlS`. A lower one is ignored.
- When `ttlS` runs out with no newer roster, the service closes the home's
  sessions with 4403 and refuses new sessions and token reads until a roster
  arrives.
- **Effective scope** of a device is its `scopes` intersected with
  `grant.scopes`.
- A device that leaves the roster loses its sessions (4403), tokens and APNs
  registrations within a second of the service receiving the roster, which
  needs the uplink to be up. A device whose `epoch`, `role` or effective scope
  changes has its sessions closed with 4409, so it reconnects and learns its
  new access. A new `epoch` also drops its tokens and APNs registrations.
- The service learns devices only from the roster. It never adds one.

## App session

1. The phone opens `wss://cloud.ftw.energy/v1/app?home=<id>` over TLS 1.3,
   with no `Origin` header, and runs `Noise_IK_25519_ChaChaPoly_SHA256` as
   initiator. The responder's static key is a service Noise key built into
   the app. The prologue is `ftw.cloud.v1:` followed by the home id, which the
   phone learned from the box (see [Session information](#session-information)).
   The query only says which home to look in: the prologue binds it, so a
   query that names another home fails the handshake.
2. Handshake message 1 carries an unsigned `now` envelope, `{hz}`: 1 while the
   phone is looking at the home, 0 while it is not.
3. The service admits the phone only if the static key that message 1 proved
   is in the home's roster, the roster has not run out, and no owner has
   suspended the device. Every refusal closes with 4403 before message 2.
4. Message 2 carries an unsigned `snapshot` envelope, below.
5. After that the service forwards each new box envelope the device may read,
   unchanged and in the order the box sent them, one unpadded lane 1 frame per
   Noise transport message. It takes the snapshot and starts the stream under
   one lock, so no envelope falls between them.

**`snapshot`**

| Field | Type | Meaning |
|---|---|---|
| `envelopes` | [Envelope] | Box envelopes in rising `seq`, each the box's own CBOR map, unchanged: for each class the device may read, the newest one the service holds; the newest `live` that carried `sources`; and the newest `hb`. Empty after an hour without the box |
| `lastSeenMs` | int | Service wall time when it last heard from the box |
| `online` | bool | The box's uplink is up now |
| `more` | uint | How many envelopes did not fit in message 2. They follow it as ordinary frames, in rising `seq` |

The phone accepts the envelopes that follow as it accepts any forwarded
envelope. An envelope too large for one Noise message is not forwarded; the
phone sees the gap through the next `hb`.

**Watch.** While at least one phone of a home looks, the service keeps the
box's `watch` in force: at once, every 50 s, inside the box's 60 s, and when
the box connects. When no phone looks, it stops, and the box returns to 5 s
within 60 s.

**Phone to service.** These use the envelope with an empty `sig`. The
service acts on these and ignores other types; a reply has the type named in
the table.

| `t` | Who | Effect |
|---|---|---|
| `watch` | Any device | `{hz}`: 1 when the phone looks at the home, 0 when it looks away. It holds until the next `watch` or the end of the session |
| `tiles.get` | Devices with `ftw.history.read` | `{have}`, tile id to etag. The service answers with the `tiles` envelopes the phone lacks |
| `token.get` | Any device | Answered with `token {token, expiresInS}`. A new token replaces the device's old one |
| `push.register` | Any device | `{apnsToken, environment, kind, activityId}`. One registration per APNs token; a Live Activity and its push-to-start each register their own |
| `push.unregister` | Any device | `{apnsToken}` |
| `suspend` | Owner | `{deviceId}`. Suspends a device |
| `unsuspend` | Owner | `{deviceId}`. Lifts a suspension |

A suspension can only narrow access, never beyond what the roster allows. The
service ignores one from a viewer and one that names a device outside the
roster. Suspending a device closes its sessions with 4403. A suspension holds
until an owner lifts it, or the roster drops the device or changes its
`epoch`, and it works while the box is offline, until the roster runs out.
Anything the service says itself, such as `lastSeenMs`, carries no box
signature, and the phone never shows it as a reading.

**Close codes.** The service ends a session with one of these. A refusal says
nothing about which check failed.

| Code | Meaning | The phone |
|---|---|---|
| 4400 | The phone broke the protocol | Retries with backoff |
| 4403 | Refused: no such home, a device not in the roster, a suspended device, a roster that ran out, a grant that is off, a purged home, or a handshake to another service key or home | Before message 2, tries the app's other service key once, at once. Otherwise asks again slowly |
| 4408 | A newer session from the same device took over | Does not retry |
| 4409 | The roster changed what this device may read | Reconnects at once, once; then retries with backoff |
| 4429 | The home has as many sessions as the service allows, as at the relay | Retries with backoff |
| 1013 | The phone fell too far behind | Retries with backoff and gets a fresh snapshot |

- **Backoff** is full jitter from 0.5 s to 60 s, as on the uplink. It also
  covers a lost socket.
- **Slowly** is a random delay between half and all of min(15 s × 2ⁿ, 5 min),
  where n counts the refusals in a row: 7.5 to 15 s at first, at most 5 min.
  A refusal changes only when the roster or grant does, or when a service
  that restarted has heard from the box again.
- The phone dials at once, whatever it waits for, when the app comes to the
  front or the network changes.

**Limits.** Per home, 16 sessions. Per device, one session; a newer one
replaces the older with 4408. Per phone, 256 envelopes queued, past which the
session closes with 1013. A phone message larger than 4 KiB, a text message,
or more than 20 messages in a burst and then 2 a second closes the session
with 4400. Message 1 must arrive within 10 s. Pings and deadlines are the
uplink's. A restarted service refuses a home's phones with 4403 until that
home's box has connected again (see [Service state](#service-state)).

### Session information

Every role learns what it needs to read FTW Cloud from the box, over its own
Noise session, in the app protocol's `hello_ok.cloud`.

| Field | Type | Meaning |
|---|---|---|
| `pk` | bstr, 65 B | The box's `cloud.key` public key. Every box envelope from the service must verify with it |
| `home` | text | The home id, cut from `pk` |
| `boot` | bstr, 16 B | The uplink's current `boot`. It counts from the same Core start as `hello_ok.clock.uptimeMs`, which anchors rule U |
| `sources` | map text→text | Opaque source id to driver name |
| `loadpoints` | map text→text | Opaque loadpoint id to loadpoint id |

- The box sends `cloud` only while the grant is on. It leaves it out while
  the grant is off or a revoke waits for its receipt, when it holds no
  `cloud.key`, and in floor mode. Without it the phone reads through the
  relay only.
- The tables name the drivers and loadpoints the box knows at `hello`. One
  added later has no name on the phone until its next session with the box.
- The box lists `cloud.link` in `hello_ok.caps` when its build can run the
  uplink. A build that names no service does not.

## Extension token

- 32 random bytes, base64url. The service stores its SHA-256, never the token.
- Bound to one roster entry and valid for 7 days. The app renews it at each
  foreground and background refresh.
- It dies when the roster drops the device, the device's epoch changes, an
  owner suspends the device, or the grant ends.
- `GET /v1/now` with `Authorization: Bearer <token>` returns CBOR
  `{envelopes, lastSeenMs}`: the newest `live`, the newest `live` with
  `sources`, `prices` and `hb`, as the box signed them, in rising `seq`.
- A token never reads `lp`, `plan`, `event`, `tiles` or `health`. A test
  proves it.
- An extension checks signatures with the box key, and ages data with the
  anchor, that the app stored in its App Group.

## Push

| Type | Payload | Phone behaviour |
|---|---|---|
| `alert` | `aps` with the generic title "FTW" and `mutable-content: 1`, plus `home` and the `event` envelope in base64 | The extension checks the signature and drops an already seen `(boot, seq)`. If the event is anchored and at most 10 minutes old, it renders text from the app's catalogue copy and picks the category, and so the buttons, from the verified `kind`. Otherwise it shows a generic line with no buttons |
| `widgets` | Content changed, no data | Widgets fetch with their token |
| `liveactivity` | `home`, the `lp` envelope and the newest `hb` envelope, in base64 | The view shows numbers only if both signatures check, the `hb` covers that `lp`, and the charger source grades `live` by the freshness rules; otherwise "Open FTW". `staleDate` is set to when that grade would lapse |
| `box.unreachable` | Generic, from the service | Sent after 600 s without the box's socket. No box signature, no arguments, no buttons |

Each push goes only to devices whose effective scope covers what it carries:
`alert` needs `ftw.events.read`, and `widgets` and `liveactivity` need
`ftw.live.read`. Viewers therefore get no alerts. `box.unreachable` goes to
devices registered for `alert`. The service sends `widgets` when tomorrow's
prices arrive and when a car is plugged in. It derives no category, collapse
id or priority from an event's kind. How often a Live Activity can refresh
within Apple's budget is the iOS plan's M7 spike.

**What Apple sees.** Every alert payload carries the home id, the event kind,
its arguments and the box's `boot`, `seq` and send time. A Live Activity
payload carries the whole `lp` envelope: charge level, power, ready time and
energy delivered. ADR 0008 lists this as a risk.

## Freshness

Carrier state `cloud` labels data read through the service. Source states
come from the box, and the rules below can only make them worse.

**Assumption.** The phone's own clock is close to true time. A phone clock
that runs behind lets a delaying service hide that much delay.

**Accepting a message.** The phone accepts a box envelope only if its
signature verifies with the box key for that home. It keeps the highest `seq`
accepted per class and per boot, and drops a message of that class whose
`seq` is not higher. It processes the envelopes of a snapshot in rising
`seq`. A message from another boot replaces the current boot only if it is
anchored and its send time is later than the newest message the phone holds.
When the boot changes, the phone drops the source table, field values and
loadpoints it held, and shows none until that boot's full `live` arrives. The
phone checks the signature, decodes the body and checks `seq` before it
changes anything, so a message it drops leaves what it holds as it was.

**Anchors.** The phone trusts a send time only by one of two rules.

- **Rule N.** The message has `wallMs`. Send time = the earlier of `wallMs`
  and the phone's time of arrival, so a fast box clock cannot make data look
  fresher. If the phone accepts a message of a boot whose `wallMs` is more
  than 2 s ahead of its arrival, it stops using rule N for that boot.
- **Rule U.** During a Noise session with the box, the phone records phone
  time minus box uptime for each message from the box that carries
  `uptimeMs`, `hello_ok.clock` among them, under the boot that
  `hello_ok.cloud.boot` names. It keeps the smallest offset per boot, and
  keeps it across app starts. Send time = offset + `uptimeMs`. Box clocks
  drift, so when rule U gives the send time, the phone adds 0.01 % of the time
  since the offset was last learned to every age. It drops the anchor
  24 hours after it was last learned.
- When both apply, the phone uses the earlier send time. When neither does,
  the data is unanchored: it shows as "cloud, age unverified", at best
  `stale`, and never `live`.

**State.** The phone holds, for the current boot, the source table with its
`srcRev`, the newest `live` and `lp` with their `seq`, and the newest anchored
`live` or `hb`, called M below. Let t = phone time − M's send time, at least
zero.

1. If M names a `srcRev` newer than the table the phone holds, every source is
   `stale` until that table arrives.
2. If M is an `hb` whose `liveSeq` is above the `seq` of the `live` the phone
   holds, every field is `stale` until that `live` arrives. The same holds
   for `lpSeq` and the loadpoint and feedback values in `lp`.
3. Otherwise, for each source with box state B, `staleAfterMs` S and age A in
   M's `ages`, the reading's age now is A + t. Graded as the box grades it,
   that is `live` up to S, `lagging` up to 2S, `stale` up to 6S and `down`
   beyond. A source missing from `ages` keeps B.
4. The effective state is the worse of B and that grade, in the order `live`,
   `lagging`, `stale`, `down`, `never`. A state the phone does not know ranks
   as `never`.
5. A field follows the worst of its sources in `fieldSources`. A field with no
   source, such as the mode, is `live` while t ≤ 2H and `stale` after. A
   loadpoint follows its charger source in `src`. A source the table lacks
   counts as `stale`.

The service cannot sign, so it cannot make a source look fresher. It can only
delay, drop or replay, and each of those ages the data or marks it stale.

**Cases.** `cloudwire.View` runs each case below as a test, except case 15,
which belongs to the notification extension. The phone's port replays them.
In all of them the phone uses rule N with 100 ms in transit unless stated.

1. **A quiet live source stays live.** One source, S = 10 s, read every
   4.5 s, so H = 5 s. The box sends a full `live` with `srcRev` 1, then only
   `hb` every 5 s for 120 s, each with an age of at most 4.5 s. The age now
   never passes 9.6 s, and the source shows `live` throughout.
2. **A dead uplink ages on time.** As case 1, but the socket dies after an
   `hb` whose age for the source is 2 s. The source shows `live` until t = 8 s,
   `lagging` until 18 s, `stale` until 58 s and `down` after.
3. **A replay ages out.** The service replays case 1's first `live` and
   `hb` an hour later to a phone that has seen nothing from that boot. Their
   send time is an hour old, so the source shows `down`. A phone that holds a
   higher `seq` for those classes drops them.
4. **A withheld table goes stale.** The box moves the source to `lagging` with
   `srcRev` 2, and the service drops that `live`. The next `hb` names
   `srcRev` 2. The phone holds revision 1, so the source shows `stale` until
   revision 2 arrives.
5. **No anchor, never live.** The box reports `clock: "none"`, and the phone
   holds no rule U offset for that boot, or none at all. The data shows as
   "cloud, age unverified" and never `live`.
6. **A fast box clock loses rule N.** A message arrives with `wallMs` 3 s
   ahead of the phone clock. Rule N stops for that boot, and without rule U
   the data is unanchored, as in case 5.
7. **A small clock lead gains nothing.** A message arrives with `wallMs` 1.5 s
   ahead of the phone clock. Its send time is the arrival time, not `wallMs`.
8. **A changed byte is dropped.** The service flips one byte of `b`. The
   signature fails and the phone drops the message.
9. **A dropped `live` goes stale.** The box sends `live` with `seq` 40, the
   service drops it, and the next `hb` names `liveSeq` 40. The phone holds
   `seq` 31, so every field shows `stale` until `seq` 40 arrives.
10. **A held-back `lp` goes stale.** As case 9, for `lp` and `lpSeq`. The car
    view shows `stale` and no charge level as current.
11. **The byte budget runs out.** The box stops sending `live` and `hb`. The
    data ages as in case 2.
12. **A reboot clears the old table.** The phone holds boot X's table with
    `srcRev` 9. Boot Y's `hb` arrives, anchored and later, with `srcRev` 3,
    and boot Y's full `live` is dropped. The phone drops boot X's table and
    values, and shows no sources and no fields until boot Y's full `live`
    arrives.
13. **Rule U drift counts against freshness.** The offset was learned 10 hours
    ago. Every age gains 3.6 s, and at 24 hours the anchor lapses.
14. **A snapshot with mixed `seq`.** Message 2 holds `prices` with `seq` 5 and
    `live` with `seq` 3000. The phone, which held `live` 2990 and no `prices`,
    accepts both.
15. **An old alert shows the generic line.** The service replays a signed
    event from 2 hours ago to a phone that never saw it. The extension shows
    the generic line with no buttons.
16. **The box's state wins.** The box reports a source as `down`, and t = 1 s.
    The source shows `down`.

## Grant on the box

The grant lives in the box's state store under `cloud/grant_v1`, not in the
config document, so a whole-document config write cannot reset it. It holds
the grant id, scopes, purposes, a fingerprint of `cloud.key`, `grantKey`, the
granting device or door, the version of the consent text the owner accepted,
the time or 0 when the clock was not set, and the data controller. With
statistics on, it also holds the `statsId` and the version of the statistics
consent text.

- A missing record, or a fingerprint that does not match `cloud.key`, reads
  as **off**, and `GET /api/cloud` says why. This is the opposite of
  `app_link` and `fleet_ping`, where a missing section reads as on.
- The box never backs up `cloud.key`, and a restore never brings it back:
  restore moves the whole data directory aside. A restored box, in place or
  on new hardware, has FTW Cloud off until an owner turns it on again, under
  a new home id and a new `grantKey`. The old home at the service ages out as
  under [Silence](#revocation-and-deletion).
- A grant works with the relay off.

| Route | Who may call it |
|---|---|
| `GET /api/cloud` | An owner's session, or a client on the LAN. State, scopes, purposes, bytes sent in the last 24 hours of uptime, whether the byte budget is reached |
| `GET /api/cloud/preview` | The same. The exact bytes that leave the house |
| `POST /api/cloud/grant {scopes, policy}` | An owner's session. On the LAN, only from loopback or with the house password. On a grant already on, it changes the scopes and keeps the key, home id and `grantKey` |
| `POST /api/cloud/stats {policy}` | The same gate, as a separate consent. 409 while the grant is off or a revoke waits for its receipt |
| `DELETE /api/cloud/grant`, `DELETE /api/cloud/stats` | An owner's session, or any client on the LAN, because narrowing access is safe |

A viewer's session gets 403 on every route, and so does a request with
forwarding headers. No route asks for a step-up: the box cannot verify the
app's, so the grant screen claims no more than an owner's session.

- Turning FTW Cloud on pushes `cloud.granted` to all owners, and turning it
  off pushes `cloud.revoked`. A change of scopes, and turning statistics on
  or off, push nothing. Every change writes an audit line on the box. A grant,
  a change of scopes and a statistics change each send a new roster.
- `POST /api/cloud/stats` draws a `statsId` the first time and keeps it while
  statistics stay on. Turning statistics on again after a withdrawal draws a
  new one.
- `DELETE /api/cloud/stats` removes `stats` from the purposes at once. The
  box keeps the `statsId` until `purged {what: "stats"}` arrives, so that
  `stats.withdraw` can name it.

## Revocation and deletion

- **Device revoke** on the box: a roster without the device. See Roster.
- **Grant revoke:** the box sends `bye {why: "revoked", purge: true}`. The
  service deletes the home: registry row, tokens, APNs registrations, live
  copy, events, tiles and statistics rows. It keeps a tombstone of the home
  id until it has delivered `purged {what: "home"}`, which it sends once all
  of that is gone, statistics rows included. Until then the box keeps
  `cloud.key`, sends only `hello` and `bye` on each connect, and shows the
  revoke as pending. With a valid `purged` it deletes the key and the grant
  and shows the receipt time.
- **Statistics withdrawal:** `stats.withdraw`. The service deletes the rows
  within 24 hours and sends `purged {what: "stats"}` when done, on the same or
  a later connection.
- **Silence:** after an hour without the box's socket, the service serves only
  `lastSeenMs` and no readings. When the roster runs out, it closes sessions
  and refuses reads. After 30 days it drops tokens and APNs registrations.
  After 90 days without a valid roster it deletes the home as in a purge.
- **A new home** that sends no roster within 24 hours is deleted.

## Service state

Per home, the service keeps in memory the newest envelope of each class, the
newest `live` that carried `sources`, 48 hours of tiles and a one-hour ring of
events. It writes no live readings to disk; the statistics files hold the
only readings it stores. Its registry holds the home id, the box key, the
roster, suspensions, the current and deleted `statsId`s, APNs registrations
and token hashes.

A restarted service holds the registry but no readings and no rosters in
memory. It admits a home's phones again once that home's box has connected
and sent its roster.

A rebuilt host starts with an empty registry. Boxes send their rosters again
and phones register again at their next start. Suspensions are lost, so an
owner must suspend again.

## Registry names

The contract registry does not hold these names yet.
[srcfl/ftw-webapp#81](https://github.com/srcfl/ftw-webapp/pull/81) adds them
there first; [#1595](https://github.com/srcfl/ftw/pull/1595) and
[srcfl/ftw-app#15](https://github.com/srcfl/ftw-app/pull/15) copy them here and
to the app.

| Name | Kind | Goes into |
|---|---|---|
| `ftw.events.read` | Scope: events behind notifications | `registry.yaml`, `scopes` |
| `cloud_scopes` | List: `ftw.live.read`, `ftw.history.read`, `ftw.plan.read`, `ftw.health.read`, `ftw.events.read` | `registry.yaml`, new block |
| `cloud_purposes` | List: `serve`, `stats` | `registry.yaml`, new block |
| `cloud.link` | Capability: the box can run the FTW Cloud uplink. Absent means the app uses the relay only and offers no FTW Cloud | `registry.yaml`, `capabilities` |
| `cloud` | Carrier state, between `relay` and `cache` | `registry.yaml`, `carrier_states` |
| `cloud.granted`, `cloud.revoked` | Push kinds, sent to every owner when FTW Cloud is turned on or off | `push-catalogue.yaml` |

Class names, field names, close codes and the signing tags are not registry
names. They live here and in `go/protocol`, held by golden vectors in both
repositories.

Role names, `owner` and `viewer`, are registry names, but `go/protocol` does
not generate them yet, so `srcfl/ftw-cloud` spells `owner` by hand. Generating
the registry's roles would remove that copy.

## Numbers in one place

| What | Value |
|---|---|
| Reconnect backoff, box and phone | 0.5 s to 60 s, full jitter |
| Phone retry after 4403 | Half to all of min(15 s × 2ⁿ, 5 min) |
| Ping, read deadline, write deadline | 15 s, 45 s, 10 s, on both sockets |
| Period per class | 5 s; 1 s while watched; `lp` at least 2 s |
| Source table change sent within | 1 s |
| `lp` deadband | Power more than max(100 W, 5 %); `sessionWh` 100 Wh; `socPermille` 10 |
| `prices` looked at | Every minute |
| Box's `watch` lasts | 60 s |
| Service renews the box's `watch` | Every 50 s while a phone looks |
| Phone's `watch` lasts | Until the next `watch` or the end of the session |
| Heartbeat H | clamp(smallest `staleAfterMs` ÷ 2, 2 s, 15 s) |
| Full state | Every 15 minutes |
| `resync` honoured | At most one per 5 minutes |
| Event ring on the box | 32 |
| Byte budget | 20 MB per 24 hours of uptime |
| Largest frame from the service to the box | 4 KiB |
| Largest message from a phone | 4 KiB |
| Sessions | 16 per home; one per device |
| Envelopes queued per phone | 256 |
| Phone messages | 20 in a burst, then 2 a second |
| App session message 1 | Within 10 s |
| Roster `ttlS`, renewal | 24 hours, every 6 hours |
| Extension token | 7 days |
| Alert age for buttons | 10 minutes |
| `box.unreachable` after | 600 s |
| Readings served after silence | 1 hour |
| Sessions closed | When the roster runs out |
| Tokens and APNs dropped after | 30 days |
| Home deleted after | 90 days without a roster; 24 hours for a new home without one |
| Statistics rows kept | 13 months from the hour they describe |
| Rule N clock lead allowed | 2 s |
| Rule U drift margin, lifetime | 0.01 % of the time since learned; 24 hours |
