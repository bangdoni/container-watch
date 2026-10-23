# ContainerWatch

Real-time, self-hosted monitoring for Docker and Podman containers — a dense,
dark dashboard with live metrics, streaming logs and multi-node support,
shipped as a single static binary.

<p align="center">
  <img src="web/static/img/favicon.svg" width="72" height="72" alt="ContainerWatch">
  <br>
  <sub>ContainerWatch</sub>
</p>

![Go](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white)
![License](https://img.shields.io/badge/license-MIT-6e9fff)

---

## Features

- **Dual engine support** — talks to the Docker and Podman Engine APIs over a
  local socket (`/var/run/docker.sock`, `/run/podman/podman.sock`) or a remote
  `tcp://` endpoint with TLS. The engine is detected on connect, so a Podman
  socket works even when configured as `docker:///var/run/docker.sock`.
- **Zero-config startup** — local sockets are probed automatically, the SQLite
  database is created on first run, and the first visit opens a setup wizard to
  create the admin account.
- **Live metrics over SSE** — CPU, memory, network and block I/O are sampled
  server-side every 2s and pushed through a single SSE heartbeat per client.
  Sparklines in the table and full charts in the metrics modal render on
  `<canvas>`/inline SVG without any charting dependency.
- **Streaming logs** — tail depth selector, pause/resume, client-side regex
  search with highlighting, ANSI escape handling and one-click export.
- **RBAC** — *User* accounts get read-only dashboards; *Admins* additionally
  manage container lifecycle, images, nodes, users and settings. Argon2id
  password hashing, opaque session tokens, CSRF double-submit protection and
  login rate limiting.
- **Multi-node** — add remote daemons with TLS, switch nodes from the sidebar,
  per-node health and engine-version reporting.
- **Dense, readable UI** — htmx + Alpine.js, self-hosted CSS, fonts and JS. No
  CDN, works fully air-gapped.

## Screens

| Page | Description |
| --- | --- |
| `/` | Dashboard: summary bar, node cards, full container table |
| `/containers` | Filterable container table (state, node, search) |
| `/containers/{id}` | Container detail: inspect data, metrics charts, live logs |
| `/images` | Image inventory with sizes and dangling detection |
| `/admin/nodes` | Add/edit/probe/delete engine nodes |
| `/admin/users` | Role management, account deletion |
| `/admin/settings` | Registration toggle, instance info |

## Install

ContainerWatch ships as a single static binary with no runtime dependencies.
Pick whichever method fits how you run containers.

### From source

Requires Go 1.26 or newer.

```sh
git clone https://github.com/bangdoni/container-watch.git
cd container-watch
go build -o containerwatch ./cmd/containerwatch
./containerwatch
# → http://localhost:8080  (setup wizard on first visit)
```

### With `go install`

Installs to `$GOBIN` (default `~/go/bin`):

```sh
go install github.com/bangdoni/container-watch/cmd/containerwatch@latest
containerwatch
```

Add `$GOBIN` to your `PATH` if it is not already there. To pin a version, use
`@v0.1.0` in place of `@latest`.

### Prebuilt binary

> **Not yet published.** `go install` works today (the module is tagged and
> installable), but prebuilt tarballs are not cut, so the
> [Releases](https://github.com/bangdoni/container-watch/releases) page has no
> archives yet. Until it does, use `go install`, build from source, or Docker.
>
> Release archives will be named
> `containerwatch_<version>_<goos>_<goarch>.tar.gz` for these targets, all of
> which cross-compile cleanly today:
>
> | Platform | Target |
> |---|---|
> | Linux x86-64 | `linux/amd64` |
> | Linux ARM64 | `linux/arm64` |
> | macOS (Apple silicon) | `darwin/arm64` |
> | macOS (Intel) | `darwin/amd64` |

### Cross-compiling

The binary is fully static (`CGO_ENABLED=0`); the SQLite driver is pure Go
(`modernc.org/sqlite`), so no libc or cross-toolchain is needed. To build for
another platform yourself:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -ldflags "-s -w" -o containerwatch ./cmd/containerwatch
```

Verified sizes for the four supported targets:

| Target | Binary |
|---|---|
| `linux/amd64` | ~14 MB |
| `linux/arm64` | ~13 MB |
| `darwin/arm64` | ~13 MB |
| `darwin/amd64` | ~14 MB |

### Docker

```sh
docker build -t containerwatch .

docker run -d --name containerwatch \
  -p 8080:8080 \
  -v containerwatch-data:/data \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  --group-add "$(stat -c '%g' /var/run/docker.sock)" \
  containerwatch
```

The image runs as UID/GID `1000`. `--group-add` with the socket's group lets the
unprivileged process read the socket; mounting it `:ro` is enough because the
Engine API is spoken over the unix socket itself.

For Podman:

```sh
podman run -d --name containerwatch \
  -p 8080:8080 \
  -v containerwatch-data:/data \
  -v /run/podman/podman.sock:/run/podman/podman.sock:ro \
  -e CW_LOCAL_SOCKETS=podman:///run/podman/podman.sock \
  containerwatch
```

## Configuration

All configuration is via environment variables; see
[`.env.example`](./.env.example) for the annotated list.

| Variable | Default | Description |
| --- | --- | --- |
| `CW_ADDR` | `:8080` | Listen address |
| `CW_PUBLIC_URL` | *(derived)* | Externally reachable base URL |
| `CW_DATA_DIR` | `./data` | Directory for the database and secret |
| `CW_DATABASE_PATH` | `$CW_DATA_DIR/containerwatch.db` | SQLite file path |
| `CW_COOKIE_SECRET` | *(generated)* | Stable secret for cookie namespaces |
| `CW_SESSION_TTL` | `24h` | Absolute session lifetime |
| `CW_SECURE_COOKIES` | *(inferred)* | Force the `Secure` cookie attribute |
| `CW_TRUST_PROXY` | `false` | Honour `X-Forwarded-*` headers |
| `CW_LOCAL_SOCKETS` | `docker:///var/run/docker.sock,podman:///run/podman/podman.sock` | Local sockets to probe (`none` disables) |
| `CW_METRICS_INTERVAL` | `2s` | Sampling interval (min `250ms`) |
| `CW_METRICS_WINDOW` | `60` | Samples retained per container |
| `CW_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `CW_OPEN_REGISTRATION` | `false` | Initial registration setting |
| `CW_HEALTHCHECK_URL` | *(derived)* | Target of `-healthcheck` |

## Remote nodes

Add a node from **Admin → Nodes** with one of:

- `unix:///var/run/docker.sock` — local socket
- `tcp://10.0.0.5:2376` — remote daemon over TLS (provide the CA and client
  certificate/key; server name verification is on by default)
- `tcp://10.0.0.5:2375` — remote daemon without TLS (**only on trusted networks**)

Credentials are stored encrypted at rest in SQLite and never rendered back to
the browser.

## CLI

```sh
containerwatch -version      # print version, commit and build date
containerwatch -healthcheck  # probe /healthz and exit (used by HEALTHCHECK)
```

`GET /healthz` returns JSON with uptime, version, node counts and a database
ping; it answers `503` when the database is unavailable.

## Development

```sh
go build ./...
go vet ./...
gofmt -l .

# Run against the local socket with a scratch database
CW_DATA_DIR=/tmp/cw-dev CW_ADDR=:8080 go run ./cmd/containerwatch
```

### Layout

```text
cmd/containerwatch/main.go   process wiring, startup, graceful shutdown
internal/config              environment configuration
internal/auth                Argon2id, sessions, RBAC middleware
internal/database            SQLite open + migrations + queries
internal/engine              Docker/Podman Engine API client, node manager
internal/hoststats           per-OS host CPU/memory sampling
internal/handlers            HTTP + htmx partial handlers, template renderer
internal/sse                 hub, metrics broker, event broker
web/embed.go                 //go:embed of static/ and templates/
web/static                   CSS, JS (htmx, Alpine), fonts, icons
web/templates/{layouts,pages,partials}
```

### Architecture notes

- **One poll, many readers.** `sse.MetricsBroker` samples every node on a
  fixed interval, keeps a ring buffer per container and publishes a compact
  JSON payload. The container table re-fetches `/containers/table` on the
  `containers` event, while sparklines/charts are updated from the same push, so
  N open browsers cost one engine poll regardless of N.
- **Reconnect-safe UI.** Because htmx swaps destroy Alpine state, chart and
  sparkline seeds travel as `<script type="application/json">` blocks and are
  rehydrated on each swap (`CW.rehydrate`).
- **No write timeout.** SSE responses are long-lived by design; read-header and
  idle timeouts still protect against slow clients, and shutdown drains via
  `http.Server.Shutdown` with a 15s grace period.
- **Template safety.** All dynamic values go through `html/template`
  contextual escaping; container actions validate ID and action name against a
  strict allow-list before touching the engine.

## Security

- Argon2id (64 MiB, t=3) password hashing with per-password salts.
- Opaque 32-byte session tokens; only SHA-256 hashes are stored server-side.
  Sessions are purged on boot and hourly.
- CSRF double-submit cookie (`cw_csrf`) plus `X-CSRF-Token` on mutating
  requests; missing or mismatched tokens yield `403`.
- Per-IP rate limiting on `/login`, `/signup` and `/setup`.
- Socket paths and endpoints are validated before use and permission errors are
  surfaced in the UI rather than crashing discovery.
- Container actions are restricted to an allow-list of verbs and are gated on
  the `admin` role server-side, not just hidden in the template.

Deploy behind TLS (a reverse proxy is fine) and set `CW_TRUST_PROXY=true` so the
`Secure` cookie and rate-limit keys use the real client address. Two things to
get right at the proxy:

- **Set `CW_TRUST_PROXY=true` only when a proxy is actually in front.** Without
  it, cookies served over HTTPS lack the `Secure` flag. With it set on a
  *directly exposed* server, callers can forge `X-Forwarded-For` and rotate
  fake addresses to sidestep the login rate limit.
- **Strip or overwrite `X-Forwarded-*` on the proxy** so clients cannot inject
  their own; nginx/Caddy set these itself for `proxy_pass`/`reverse_proxy`.

## License

MIT — see [LICENSE](LICENSE).

ContainerWatch is original code, written from scratch. It stands on the
shoulders of a few permissively-licensed third parties: Alpine.js and htmx
(MIT), the htmx SSE extension (0BSD), the Inter and JetBrains Mono fonts
(SIL OFL 1.1), and Go libraries that are MIT or BSD-3-Clause. Since there's
no copyleft anywhere in the mix, the whole thing is covered by the MIT
license. Full credits live in
[THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md), and the font license ships
with the fonts at `web/static/fonts/OFL.txt`.
