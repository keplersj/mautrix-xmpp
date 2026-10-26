# mautrix-xmpp

A Matrix bridge for puppeting your own XMPP accounts, built on
[mautrix-go's bridgev2](https://pkg.go.dev/maunium.net/go/mautrix/bridgev2) and meant to run
under [Beeper's bridge manager](https://github.com/beeper/bridge-manager) (`bbctl`) as a
self-hosted bridge.

You log in with your XMPP address (JID) and password. The bridge connects to your XMPP server
as an ordinary client, like Conversations or Gajim would, and your 1:1 chats and group chats
(MUCs) show up as rooms in Matrix/Beeper. You can log in to several XMPP accounts on any
servers.

## What it is not

- **Not a network gateway.** It doesn't run an XMPP server component, doesn't need admin access
  to any XMPP server, and doesn't let XMPP users talk to arbitrary Matrix users. It only
  bridges what your own account sees.
- **Not end-to-end encrypted.** Messages are plaintext between your XMPP server and the bridge.
  **OMEMO (and OpenPGP) chats are not bridged**: an encrypted message shows up as a notice
  telling you to read it in an XMPP client. Matrix-side encryption between your Beeper clients
  and the bridge (end-to-bridge encryption) works as with any bbctl bridge.
- No voice/video (Jingle), no PubSub feeds, no account registration, no relay mode.

## Running with bbctl

1. Generate a config (this registers the bridge on your Beeper account):

   ```sh
   bbctl config --type bridgev2 -o config.yaml sh-xmpp
   ```

   The XMPP settings go in the `network:` section. Missing settings get the defaults from
   [`pkg/connector/example-config.yaml`](pkg/connector/example-config.yaml), so the generated
   config works as is.

2. Run the bridge, either the binary (`mautrix-xmpp -c config.yaml`) or the container image:

   ```sh
   mkdir -p data && cp config.yaml data/
   docker run --rm --user 10001 --read-only -v "$PWD/data:/data" \
     ghcr.io/keplersj/mautrix-xmpp:0.2.0
   ```

   (The data directory must be writable by uid 10001.) The bridge connects to Beeper over a
   websocket, so it needs no inbound ports.

### Logging in

Open a chat with the bridge bot. bbctl names it after the bridge, so for `sh-xmpp` that is
**`@sh-xmppbot:beeper.local`**. Then:

1. Send `login`.
2. The bot asks for your **XMPP address**: send `you@example.org`.
3. The bot asks for your **password**: send it. The bot redacts that message right away.
4. The bot asks for a **server address (optional)**: send `skip`, unless your server has no
   DNS SRV records. In that case send `host:port` (STARTTLS) or `tls://host:port` (direct TLS).
5. The bot replies `Successfully logged in as you@example.org`.

Repeat with another address to add more accounts. `logout <JID>` removes one, and `list-logins`
shows them. Your group chats come from your bookmarks: every bookmarked room set to
**autojoin** is joined and gets a room. 1:1 chats appear when a message arrives, or start one
with `start-chat you@example.org contact@example.net`.

## Features

| Feature | XEP | XMPP → Matrix | Matrix → XMPP |
|---|---|:-:|:-:|
| 1:1 text messages | RFC 6121 | ✅ | ✅ |
| Group chats | XEP-0045 | ✅ | ✅ |
| Bookmarks (which rooms to join) | XEP-0402, XEP-0048 | ✅ | — |
| Messages from your other clients | XEP-0280 Carbons | ✅ | n/a |
| Stable message IDs | XEP-0359 | ✅ | ✅ |
| Reliable delivery across reconnects | XEP-0198 (acks, re-send) | ✅ | ✅ |
| Edits | XEP-0308 | ✅ | ✅ |
| Deletes | XEP-0424 | ✅ | ✅ (own messages) |
| Replies | XEP-0461 | ✅ | ✅ |
| Reactions | XEP-0444 | ✅ | ✅ |
| Read markers | XEP-0333 | ✅ | ✅ |
| Delivery receipts | XEP-0184 | ✅ | ✅ (1:1) |
| Typing notifications | XEP-0085 | ✅ | ✅ (1:1) |
| Files and images | XEP-0363 upload, XEP-0066 links | ✅ | ✅ |
| History backfill and catch-up | XEP-0313 MAM | ✅ | — |
| Contact names and avatars | roster, XEP-0054 vCard | ✅ | — |
| Capability detection | XEP-0030 | ✅ | — |
| Direct TLS, SRV lookup | XEP-0368, RFC 6120 | ✅ | |
| SASL SCRAM-SHA-256/1 (+PLUS), PLAIN over TLS | RFC 7677 | ✅ | |

TLS is mandatory and certificates are verified against your JID's domain. There is no
fallback to plaintext or unverified TLS. A private CA can be trusted with `SSL_CERT_FILE`.

### Known limitations (not yet)

- XEP-0198 session *resumption*: reconnects start a new session. Unacknowledged outgoing
  messages are re-sent, and missed incoming ones are fetched from MAM.
- MUC private messages are dropped. Joining, creating or leaving rooms from Matrix, invites,
  and room moderation aren't supported. The room subject is bridged to the topic one way only.
- Contact presence/status isn't bridged. Subscription requests aren't answered.
- No XEP-0084 PEP avatars (vCard avatars only), no message formatting (XEP-0393 styling is
  passed through as plain text), no stickers, polls or locations.
- File captions are sent as a separate message after the file.
- Without MAM on your server (or in a room), there's no history and no catch-up after
  downtime.

## Configuration

The `network:` section (see the [example](pkg/connector/example-config.yaml)):

| Key | Default | Meaning |
|---|---|---|
| `resource_prefix` | `mautrix-xmpp` | Resource prefix; each login gets a stable `prefix.<random>` |
| `connect_timeout` | `30s` | Dial and stream negotiation timeout |
| `reconnect.initial_delay` / `max_delay` | `2s` / `5m` | Exponential backoff with jitter |
| `ping_interval` | `60s` | XEP-0199 keepalive (0 disables) |
| `proxy` | `""` | Default `socks5://` / `socks5h://` proxy; per-login `proxy` in login metadata overrides it |
| `presence_priority` | `0` | Keep ≥ 0 so messages to your bare JID reach the bridge |
| `mam.page_size` / `catchup_max` | `50` / `500` | Archive paging |
| `media.max_download_size` / `max_upload_size` | 100 MiB | File limits (uploads also capped by the server) |
| `send_read_markers`, `send_typing` | `true` | Send XEP-0333 markers and XEP-0085 typing |
| `log_message_content` | `false` | Debug only: log XMPP stanzas and Matrix request bodies |
| `metrics.enabled` / `listen` | `false` / `127.0.0.1:8001` | Serve Prometheus metrics and health checks (see [Monitoring](#monitoring)) |
| `metrics.pprof` | `false` | Also serve Go profiles at `/debug/pprof/` |

Logs go to stdout through bridgev2's `logging` config. Passwords are never logged, and message
bodies aren't logged unless `log_message_content` is on, even at debug level.

## Deployment (container image contract)

The image `ghcr.io/keplersj/mautrix-xmpp` is published for **linux/amd64 and linux/arm64** on
version tags (`v0.1.0` → `0.1.0`, `0.1`), and each release is also tagged
`sha-<full commit SHA>`. There is no `latest` tag; pin a version or digest.

| | |
|---|---|
| Entrypoint | `["/usr/local/bin/mautrix-xmpp"]` |
| Command | `["-c", "/data/config.yaml"]` |
| User | runs as any non-root uid; default `10001:10001`; no home directory needed |
| Config | `/data/config.yaml`, may be **read-only** (detected; upgrades are applied in memory only) |
| Writable paths | only `/data` (SQLite DB `sh-xmpp.db` from bbctl's config, `logs/`); works with `--read-only` |
| Working directory | `/data` (bbctl's relative database and log paths resolve there) |
| Ports | none with bbctl (websocket to the homeserver); optional metrics port, see [Monitoring](#monitoring) |
| Base | `distroless/static`, static binary, no shell |

Example with the [beeper-bridges Helm chart](https://github.com/keplersj/helm-charts):

```yaml
externalBridges:
  xmpp:
    enabled: true
    image: {repository: ghcr.io/keplersj/mautrix-xmpp, tag: "0.2.0"}
    configSecret: {name: mautrix-xmpp-config}   # key config.yaml = bbctl output
```

If the config is missing or unreadable, the bridge exits with code 10 and a message explaining
how to generate one.

## Monitoring

With `network.metrics.enabled: true` the bridge serves plain HTTP on `network.metrics.listen`.
The endpoints have no authentication, so keep the listener on localhost or a private network.
Inside a container, set `listen: 0.0.0.0:8001` so it is reachable from outside the container.
If the address can't be bound, the bridge exits with code 1 at startup.

| Path | |
|---|---|
| `/metrics` | Prometheus metrics |
| `/healthz` | Liveness: `200` whenever the process is serving |
| `/readyz` | Readiness: `503` until startup finishes (including connecting to Matrix), then `200` |
| `/debug/pprof/` | Go profiles, only with `metrics.pprof: true` (for example for Pyroscope) |

Neither health check looks at XMPP connections, so one account with a wrong password doesn't make
the bridge unhealthy. Use the `mautrix_xmpp_logins` metric for that. The image has no shell or
curl, so use an HTTP probe, for example in Kubernetes:

```yaml
livenessProbe: {httpGet: {path: /healthz, port: 8001}}
readinessProbe: {httpGet: {path: /readyz, port: 8001}}
```

Metrics (all prefixed `mautrix_xmpp_`). Labels never contain JIDs, room or server names, or
error text:

| Metric | Type | Labels |
|---|---|---|
| `build_info` | gauge | `version`, `commit` |
| `logins` | gauge | `state`: `connecting`, `connected`, `disconnected`, `bad_credentials`, `stopped` |
| `connect_attempts_total` | counter | |
| `disconnects_total` | counter | `reason`: `bad_credentials`, `auth_temporary`, `tls`, `stream_conflict`, `stream_error`, `timeout`, `network`, `eof`, `other` |
| `stanzas_total` | counter | `direction` (`in`, `out`), `kind` (`message`, `presence`, `iq`) |
| `iq_duration_seconds` | histogram | `op` (`ping`, `mam`, `upload_slot`, `other`), `result` (`ok`, `error`, `timeout`, `failed`) |
| `unacked_stanzas` | gauge | Outbound stanzas waiting for a XEP-0198 ack, all sessions |
| `resent_messages_total` | counter | Unacknowledged messages re-sent after a reconnect |
| `matrix_send_duration_seconds` | histogram | `result` (`ok`, `ack_timeout`, `unconfirmed`, `not_connected`, `failed`); includes the up to 10 s ack wait. `unconfirmed` means the server has no stream management to ack with |
| `mam_messages_total` | counter | Messages fetched from archives |
| `media_transfers_total` | counter | `direction` (`download`, `upload`), `result` (`ok`, `too_large`, `failed`) |
| `media_bytes_total` | counter | `direction` |

The standard `go_*` and `process_*` metrics are included too. Scrape it with Prometheus:

```yaml
scrape_configs:
  - job_name: mautrix-xmpp
    static_configs:
      - targets: ["bridge:8001"]
```

### Grafana

[`contrib/grafana`](contrib/grafana) has:

- [`dashboard.json`](contrib/grafana/dashboard.json): import it in Grafana (Dashboards → New →
  Import) or provision it. It covers connection states and disconnect reasons, Matrix → XMPP
  send latency and results, stanza and IQ traffic, stream management acks, archive and media
  transfers, and process stats. It asks for a Prometheus data source and, for the collapsed
  log panel, an optional Loki one.
- [`alerts.yaml`](contrib/grafana/alerts.yaml): Prometheus alerting rules. Load them into
  Prometheus, the Mimir or Grafana Cloud ruler, or import them as Grafana-managed alerts. They
  cover the bridge being down, logins stuck disconnected, rejected passwords, flapping
  connections, a growing ack backlog and failing sends.
- [`config.alloy`](contrib/grafana/config.alloy): a Grafana Alloy config that scrapes metrics
  (to Prometheus or Mimir), profiles (to Pyroscope, needs `metrics.pprof: true`) and the
  container's JSON logs (to Loki).

The dashboard and alerts expect the scrape job to be named `mautrix-xmpp`. `make check-grafana`
parses every dashboard query and alert rule with promtool and runs the alert unit tests in
[`alerts_test.yaml`](contrib/grafana/alerts_test.yaml) (needs Docker). CI runs it too.

## Development

```sh
make build        # go build -tags goolm ./cmd/mautrix-xmpp
make test         # go vet + unit tests
make lint         # golangci-lint
make integration  # end-to-end test in Docker Compose (Prosody + Synapse + bridge image)
make image        # multi-arch image build
```

Builds need `-tags goolm` (pure-Go Olm) and cgo (bridgev2 links `mattn/go-sqlite3`).
`make integration` builds the image, runs it as uid 10001 with a read-only root filesystem and
config, and drives it from a test that is both a Matrix user and XMPP users. It covers login
through the bot, 1:1 and group messages both ways, edits, deletes, replies, reactions, typing,
markers, file transfer both ways, initial backfill, and reconnect with MAM catch-up. See
[`integration/`](integration) and [`docs/DESIGN.md`](docs/DESIGN.md) for the design.

## License

MIT, see [LICENSE](LICENSE).
