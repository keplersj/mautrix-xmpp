# mautrix-xmpp design

mautrix-xmpp is a [bridgev2](https://pkg.go.dev/maunium.net/go/mautrix/bridgev2) bridge that
logs into a user's own XMPP accounts as an ordinary client and bridges their 1:1 chats and
group chats (MUCs) into Matrix. It is built for Beeper's self-hosted bridge manager
(`bbctl config --type bridgev2 sh-xmpp`) and connects to the homeserver over the bbctl
websocket, so it needs no inbound ports.

It is a **puppeting** bridge, not a gateway: it never runs an XMPP component, needs no admin
access to any XMPP server, and only sees what the user's own account sees.

This document records the decisions made before writing the connector so they can be reviewed.

## 1. XMPP client library

Candidates evaluated (versions current on the Go module proxy, October 2026):

| Library | Latest | Typing | Notes |
|---|---|---|---|
| [`mellium.im/xmpp`](https://mellium.im/xmpp) | v0.23.0 (2026-05) | Strong: typed stanzas, `jid.JID`, IQ request/response matching, per-XEP packages | Pure Go, BSD-2. Pluggable stream features. Security fixes in every release (CVE-2024-46957, roster/carbon spoofing). |
| [`github.com/xmppo/go-xmpp`](https://github.com/xmppo/go-xmpp) | v0.4.1 (2026-10) | Weak: one `Recv()` returning a handful of structs, everything else is raw XML | Excellent SASL (SCRAM-SHA-512, -PLUS, SASL2, FAST), but a single 2.5k-line file with no IQ correlation and no MUC/MAM/bookmark helpers. |
| `gosrc.io/xmpp` (FluuxIO) | v0.5.1 (2020-03) | Medium | Unmaintained since 2020. Rejected. |
| `github.com/mattn/go-xmpp` | moved | — | Became `xmppo/go-xmpp`. |

**Decision: `mellium.im/xmpp`.** It is the only maintained Go library that is well typed and has
real extension support. For a bridge, correct IQ correlation, JID handling (PRECIS) and typed
MUC/MAM/disco helpers matter more than the extra SASL mechanisms `go-xmpp` has. Neither library
implements XEP-0198, so that is equal (see §5).

What mellium gives us natively, and what we use:

| Spec | mellium package | Used for |
|---|---|---|
| RFC 6120/6121 core, resource binding, roster | `xmpp`, `roster` | session, contacts |
| SASL SCRAM-SHA-256(-PLUS), SCRAM-SHA-1(-PLUS), PLAIN (TLS only) | `mellium.im/sasl` | login |
| STARTTLS, XEP-0368 direct TLS via SRV | `xmpp.StartTLS`, `dial` | transport security |
| XEP-0030 Service Discovery, XEP-0115 caps | `disco` | capability detection |
| XEP-0045 Multi-User Chat | `muc` (types), our own join logic | group chats |
| XEP-0402 PEP Native Bookmarks | `bookmarks` | which rooms to join |
| XEP-0313 MAM + XEP-0059 RSM | `history`, `paging` (types) | backfill and catch-up |
| XEP-0363 HTTP File Upload | `upload` | sending media |
| XEP-0066 Out of Band Data | `oob` (types) | media links |
| XEP-0184 Message Delivery Receipts | `receipts` (types) | delivery status |
| XEP-0280 Message Carbons | `carbons` | messages from the user's other clients |
| XEP-0199 Ping | `ping` | dead-connection detection |
| XEP-0203 Delayed Delivery | `delay` | timestamps |

What we implement ourselves on top of mellium (small, typed `encoding/xml` structs in
`pkg/xmppx`): XEP-0198 (§5), XEP-0048 bookmarks fallback, XEP-0054 vCard (names/avatars),
XEP-0085 chat states, XEP-0308 corrections, XEP-0333 chat markers, XEP-0359 stanza/origin IDs,
XEP-0421 occupant IDs, XEP-0424 retraction, XEP-0428 fallback indication, XEP-0444 reactions,
XEP-0461 replies.

Incoming messages are decoded once into a single Go struct with all the extension elements
above, which keeps parsing in one place and makes conversion unit-testable without a server.

## 2. Repository layout and build

- Module `github.com/keplersj/mautrix-xmpp`, binary `cmd/mautrix-xmpp`.
- `pkg/connector`: the bridgev2 `NetworkConnector`/`NetworkAPI`, login, ID mapping, message
  conversion, backfill.
- `pkg/xmppx`: the XMPP client session (dialing, TLS, SASL, stream management, reconnect) and
  the extension element types.
- `pkg/sqlitedrv`: registers bridgev2's `sqlite3-fk-wal` driver name on top of the pure-Go
  `modernc.org/sqlite`, so the binary builds with `CGO_ENABLED=0`.
- End-to-bridge encryption uses mautrix's pure-Go Olm (`-tags goolm`), again so no CGO is
  needed. The result is a static binary on a distroless base image, built for linux/amd64 and
  linux/arm64.

## 3. Database

bridgev2's own database layer, SQLite by default (`database.type: sqlite3-fk-wal`). The URI
comes from the config. bbctl generates `file:sh-xmpp.db?_txlock=immediate`, a path relative to
the working directory; the image sets `WORKDIR /data`, so it ends up at `/data/sh-xmpp.db`.
Postgres keeps working through the bridgev2 config as usual.

One small connector table (`xmpp_message_alias`) maps server-assigned stanza IDs to bridged
message IDs (see §4). It is migrated by its own version table, separate from bridgev2's.

## 4. ID scheme

All JIDs are normalized with `jid.Parse` (PRECIS) and stored in their string form. Each XMPP
account is its own login, so one Matrix user can have several.

| bridgev2 ID | Value | Example |
|---|---|---|
| `UserLoginID` | bare JID of the account | `alice@example.org` |
| `PortalKey` (1:1) | `ID = "dm:" + contact bare JID`, `Receiver = login ID` | `dm:bob@example.net` / `alice@example.org` |
| `PortalKey` (MUC) | `ID = "muc:" + room bare JID`, `Receiver = login ID` | `muc:dev@conference.example.net` / `alice@example.org` |
| `UserID` (contact, or occupant with a known real JID) | bare JID | `bob@example.net` |
| `UserID` (occupant of a semi-anonymous room) | `occ:` + room JID + `/` + nick (or `occ:` + room + `#` + occupant-id when XEP-0421 is present) | `occ:dev@conference.example.net/Bob` |
| `MessageID` | `chat bare JID + "|" + message ID` (see below) | `dm:bob@example.net|a1b2c3` |

Every portal has a receiver, so portals are always per login. Two of a user's accounts in the
same MUC get two rooms. That matches bbctl's default `split_portals: true` and avoids mixing
two identities in one room.

**Resources.** 1:1 chats are keyed by **bare** JID. Messages from any resource of a contact go
to the same portal, and outgoing messages go to the bare JID so the contact's server routes
them (with carbons, every client of the contact sees them). Other resources of our own account
are treated as "from me" via XEP-0280 carbons. The bridge binds its own resource
(`mautrix-xmpp.<random>`) and sends presence with priority 0, so it receives bare-JID messages
even when it is the user's only client.

**Message IDs.** XMPP has several IDs per message, and different XEPs reference different ones:

- The sender-assigned `<origin-id>` (XEP-0359), or the stanza `id` attribute if there is none.
  Corrections (XEP-0308) and all 1:1 references (replies, reactions, retractions, markers) use
  this one.
- The `<stanza-id by=...>` assigned by the archive (our own server for 1:1, the room for MUC).
  Groupchat references (XEP-0444/0461/0424/0333) and MAM paging use this one.

The canonical `MessageID` is built from the sender-assigned ID, so a MUC reflection of our own
message, a carbon, and a later MAM copy of the same message all dedupe to the same bridged
message. The archive's stanza-id is stored in message metadata and in `xmpp_message_alias`, so
groupchat references can be resolved. Messages with neither ID get a deterministic hash of
(sender, timestamp, body).

## 5. Connection lifecycle and XEP-0198

- **Dialing.** SRV lookup for `_xmpps-client._tcp` (direct TLS, XEP-0368) and
  `_xmpp-client._tcp` (STARTTLS), with `domain:5222` as the fallback. An optional per-login
  `host:port` override is allowed. Optional SOCKS5/HTTP proxy, either connector-wide or per
  login.
- **TLS.** Required. Certificates are verified against the JID's domain using the system roots
  (`SSL_CERT_FILE` is honored for private CAs). There is no plaintext or skip-verify fallback;
  STARTTLS is attempted even if the server doesn't advertise it, to prevent downgrades.
- **Auth.** SCRAM-SHA-256-PLUS, then SCRAM-SHA-256, SCRAM-SHA-1-PLUS, SCRAM-SHA-1, and PLAIN
  last. All of them run only over TLS.
- **Reconnect.** Exponential backoff with jitter, with configurable initial and maximum delay.
  An XMPP ping (XEP-0199) on an interval detects dead TCP connections.
- **Bridge state.** `CONNECTING` while dialing; `CONNECTED` after bind, roster and MUC joins;
  `TRANSIENT_DISCONNECT` on network errors, while retrying; `BAD_CREDENTIALS` on SASL
  `not-authorized`/`account-disabled`/`credentials-expired`, after which reconnecting stops
  until the user logs in again; `UNKNOWN_ERROR` on TLS verification failures and other errors
  the user has to act on, still retrying with backoff.
- **XEP-0198 Stream Management.** Neither Go library implements it. mellium's
  `StreamConfig.TeeIn/TeeOut` hands us the decrypted byte streams, and a small depth-tracking
  scanner over those streams counts top-level stanzas exactly (including IQ results that never
  reach a handler) and captures the raw bytes of each outgoing stanza. With that we implement:
  - `<enable/>` when the server advertises `urn:xmpp:sm:3`, then answering `<r/>` with the
    exact inbound count, and requesting acks after sends and periodically.
  - A queue of unacknowledged outgoing **messages**. On reconnect, after the new session is
    bound, unacknowledged messages are re-sent with their original IDs (receivers dedupe by ID).
  - `HandleMatrixMessage` waits briefly for the server ack, so Beeper's "sent" tick means the
    server has the message.

  **Session resumption (`<resume/>`) is not in v0.1.0.** Reconnects always bind a new session.
  Inbound messages missed while disconnected are recovered by MAM catch-up instead (§7).

## 6. Messaging features (v0.1.0)

| Feature | Spec | In | Out | Notes |
|---|---|---|---|---|
| Plain text 1:1 | RFC 6121 `type=chat` | ✅ | ✅ | `normal` messages with a body are treated as chat |
| Group chat | XEP-0045 | ✅ | ✅ | joins bookmarked rooms (`autojoin`) |
| Bookmarks | XEP-0402, fallback XEP-0048 (private XML storage) | ✅ | — | read only |
| Carbons | XEP-0280 | ✅ | n/a | sent carbons become "from me" messages |
| Stable IDs | XEP-0359 | ✅ | ✅ | `origin-id` on every outgoing message |
| Stream management | XEP-0198 | ✅ | ✅ | acks and re-send; resumption not yet |
| Corrections | XEP-0308 | ✅ | ✅ | → Matrix edits |
| Retractions | XEP-0424 (`:1`, and `:0` via fasten on receive) | ✅ | ✅ | → Matrix redactions |
| Replies | XEP-0461 + XEP-0428 fallback | ✅ | ✅ | the quoted fallback is stripped/added |
| Reactions | XEP-0444 | ✅ | ✅ | full-set semantics mapped to bridgev2 reaction sync |
| Read markers | XEP-0333 `displayed` | ✅ | ✅ | → Matrix read receipts |
| Delivery receipts | XEP-0184 | ✅ | ✅ (1:1) | → Beeper delivery status; we answer requests in 1:1 |
| Typing | XEP-0085 | ✅ | ✅ (1:1) | `composing`/`paused`/`active` |
| Media out | XEP-0363 + XEP-0066 | — | ✅ | upload, then send the URL with `jabber:x:oob`; a caption goes as its own message |
| Media in | XEP-0066 / bare HTTPS URL as body | ✅ | — | downloaded within size limits and reuploaded to Matrix |
| History | XEP-0313 MAM v2 | ✅ | — | backfill on portal creation, backwards backfill queue, catch-up after reconnect |
| Capability detection | XEP-0030 | ✅ | — | MAM, carbons, upload service, room features |
| Names/avatars | roster names, XEP-0054 vCard | ✅ | — | |

### Not in v0.1.0 (documented "not yet", not stubs)

- XEP-0198 session resumption (see §5).
- MUC private messages (`type=chat` from `room@service/nick`). They are logged and dropped.
- Joining or creating MUCs from Matrix, invites, MUC configuration and moderation, subjects as
  topics in both directions (incoming subject is bridged as the room topic).
- Presence and status bridging beyond what's needed to join MUCs, and handling of roster
  subscription requests (they are not auto-accepted).
- XEP-0084 PEP avatars (vCard avatars only), XEP-0172 nicknames.
- Stickers, polls, location, XEP-0393 styling to Matrix formatting (text is passed through as
  plain text).
- XEP-0447/0448 stateless file sharing and encrypted file sharing.

### Out of scope for v0.1.0

- **OMEMO / end-to-end encryption.** Messages are **not** end-to-end encrypted between Matrix
  and XMPP: the bridge, and the XMPP server, see plaintext. OMEMO-encrypted messages
  (`eu.siacs.conversations.axolotl`, `urn:xmpp:omemo:2`) are not decrypted. The bridge posts a
  notice saying an encrypted message could not be bridged. (End-to-bridge Matrix encryption
  between your Matrix clients and the bridge is unaffected and on by default under bbctl.)
- Voice/video (Jingle), PubSub feeds, in-band account registration, relay mode.

## 7. History (MAM)

Server support is detected with disco#info (`urn:xmpp:mam:2`) on the account's bare JID for
1:1 chats, and on each room for MUCs. Without MAM the bridge only bridges live messages.

- **Initial backfill** (portal creation): bridgev2 calls `FetchMessages(forward)`. We query the
  last `max_initial_messages` with RSM `<before/>` (newest page) and `with=<contact>` (1:1) or
  against the room JID (MUC).
- **Backwards backfill** (Beeper's backfill queue): `FetchMessages(backward)` pages with
  `<before>` = the oldest archive ID we've seen, which is kept as the cursor.
- **Catch-up after (re)connect**: one query against the account archive with
  `start = last seen timestamp` (in login metadata), paged up to `network.mam.catchup_max`.
  Results are queued as normal live events, so they create portals if needed and dedupe by
  message ID. Each joined MUC with MAM gets the same treatment, `after` the last stanza-id we
  saw in that room. MUC joins request `maxstanzas=0` history so join history doesn't duplicate
  MAM.

## 8. Configuration

bbctl generates everything except the `network:` section, which falls back to the defaults
from `pkg/connector/example-config.yaml` (no secrets in it):

```yaml
network:
  resource_prefix: mautrix-xmpp
  connect_timeout: 30s
  reconnect: { initial_delay: 2s, max_delay: 5m }
  ping_interval: 60s
  proxy: ""                # socks5:// or http(s):// default proxy; per-login override in metadata
  presence_priority: 0
  mam: { page_size: 50, catchup_max: 500 }
  media: { max_download_size: 104857600, max_upload_size: 104857600 }
  send_read_markers: true
  send_typing: true
  log_stanzas: false       # debug only: logs XML after authentication; may include message bodies
```

The binary exits with a clear message if the config is missing or unparsable. If the config
file isn't writable (a read-only Kubernetes Secret mount), config upgrades are applied in
memory only and nothing is written next to it.

## 9. Logging

zerolog through bridgev2's `logging` config, written to stdout. Passwords are never logged:
the login flow doesn't log input, the password lives only in login metadata in the database,
and stanza logging (`log_stanzas`) only starts after SASL completes. Message bodies are never
logged by default, not even at debug level. Only `log_stanzas: true` logs them.

## 10. Testing

- Unit tests: JID/portal/ghost/message-ID mapping, XMPP→Matrix and Matrix→XMPP conversion for
  every feature in §6, login input validation, the XEP-0198 stanza scanner.
- Integration: `integration/` runs Prosody (with MUC, MAM, carbons, HTTP file share and
  bookmarks) plus a Synapse homeserver in Docker Compose, the bridge binary, and a Go test
  driver that acts both as a Matrix user and as a second XMPP account. It covers login, 1:1 and
  MUC messages both ways, an edit, a retraction, a reaction, a file upload, and reconnect with
  MAM catch-up. Run it locally with `make integration`, and in CI.
