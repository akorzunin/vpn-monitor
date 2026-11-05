# VPN monitor

A small Go service that checks **workstation → VPN protocol → destination** connectivity.
Define servers and protocols in YAML; the same configuration drives scheduled checks,
the React/shadcn dashboard, Telegram commands, and labeled Prometheus metrics.

This is a new implementation, not a deployment of your existing VPN credentials.
No VPN server is allocated, no profiles are published, and no active client is switched.

## What it measures

- **HTTP:** repeated fresh HTTPS connections, expected status, time to response
  headers (mean/p95), failed-request percentage, and per-request results.
- **UDP ping / loss:** authenticated echo datagrams sent **through the selected
  tunnel**, with sent/received counts, packet loss, mean/p95 RTT. This is **not ICMP**.
- **Speed:** an optional byte/time-bounded download through the selected route,
  recording bytes, elapsed time and Mbps. Not an upload or sustained-capacity test.
- **Direct controls:** at least two independently reachable HTTPS destinations.
  A failed control makes the route result **inconclusive**, not proof of VPN blocking.
- States: `healthy`, `degraded`, `down`, `inconclusive`, `error`; the UI separately
  shows queued/running, unknown and stale results.

UDP tests are explicitly enabled per protocol. SSH, Naïve and HTTP CONNECT are
TCP-only here and reject `udp: true`. Other routes still need actual server-side
UDP support. Missing/unsupported tests are not reported as zero loss or zero speed.
HTTP latency includes DNS/tunnel setup/TLS as applicable; it is not network ping.
Latency percentiles describe successful samples only; failed samples remain visible.

## Requirements and quick start

- Go 1.25+, Node.js 22.12+, npm.
- A maintained **sing-box** binary with the outbound features you configure
  (Naïve requires a build including `with_naive_outbound`; QUIC protocols need QUIC).
  Local integration tests were run with sing-box 1.14.4. Only the configured
  protocol's native outbound is started, with a loopback mixed listener.

```sh
npm ci --prefix web
npm run build --prefix web
go build -o bin/vpn-monitor ./cmd/vpn-monitor
cp config.example.yaml config.yaml
chmod 600 config.yaml

# Generate secrets locally; never put real secrets in Git or shell history.
export VPN_MONITOR_API_TOKEN="$(openssl rand -hex 32)"
export EXAMPLE_HYSTERIA_PASSWORD='replace-with-your-private-credential'
# Edit config.yaml: the example addresses/credentials are placeholders.
bin/vpn-monitor --config config.yaml --check
bin/vpn-monitor --config config.yaml
```

Open <http://127.0.0.1:8080> and enter the API token. The token is kept only in tab
memory. All assets are served locally; there are no CDN, font or Telegram dependencies.
For Vite development, run `npm run dev --prefix web` alongside the backend.

Run the service **on the workstation/network you want to measure**. A checker on
another VPS measures a different path. Go orchestration and isolated sing-box
processes do not bypass an existing system TUN, nftables rules or proxy routing.
Verify the underlying routes/direct controls before interpreting results. No test
proves ISP attribution, DPI invisibility, streaming quality, or an IP ban.

## Shared configuration via Dropbox

Keep the real `.env` and `config.yaml` in Dropbox, with local symlinks in each
checkout. Once both files exist locally, run:

```sh
./install.sh
# Or specify a different Dropbox location:
./install.sh "$HOME/Dropbox/vpn-monitor"
```

The script only sets up shared configuration; it does not install dependencies
or build the app. It moves existing local files into the shared directory, or
links files already synced there on another machine. It is safe to rerun and
refuses conflicting copies rather than overwriting them. Sync Dropbox before
running it on another machine. Both project paths remain gitignored.

This stores credentials in Dropbox. Only source a trusted, shell-compatible
`.env`; the app does not load it automatically:

```sh
set -a
. ./.env
set +a
bin/vpn-monitor --config config.yaml
```

Keep `data/` local to each machine and review machine-specific paths in the
shared configuration. Configuration changes require a restart.

## Configuration

See [`config.example.yaml`](config.example.yaml). Add as many named servers and
protocol entries as needed (up to 64 routes). `server-name/protocol-name` is the
stable route ID used by the UI, API and bot. Names allow letters, digits, `_`, `-`.

`outbound` is a native sing-box outbound mapping, not a second protocol-specific
schema. Supported types: SSH, Naïve, VLESS, VMess, Trojan, Shadowsocks, Hysteria,
Hysteria 2, TUIC, SOCKS and HTTP. Feature support depends on your installed core;
unsupported core configuration is recorded as a startup error. TLS verification
cannot be disabled and SSH requires `host_key` pins. Retain the correct TLS/REALITY
server name when configuring a server IP. Routing overrides/detours and direct
outbounds are forbidden, so a failing route cannot silently fall back to direct.

Explicit `${ENVIRONMENT_VARIABLE}` references are expanded safely as scalar
values, including quotes/newlines; missing values are errors. Token settings name
environment variables (`api_token_env`, `telegram.token_env`, `udp.token_env`).
Never distribute admin SSH keys. Prefer dedicated restricted tunnel credentials.

Changes take effect on restart; the UI/metrics discover routes automatically.
The YAML can be generated by Ansible or any other configuration tool. `--check`
validates YAML, secrets and application bounds, **not server connectivity or all
native core fields**. Use `sing-box check` privately to diagnose core configuration.
Core stderr is intentionally not published because it may contain credentials.

Checks run at startup and at `interval`. A **single worker** serializes full tests
so simultaneous speed tests cannot compete. Manual runs share the queue, and an
already queued/running route returns `409`. Choose an interval that fits the
number of routes and worst-case timeouts. Results older than three intervals are
stale, even if their last measurement was healthy. A test can contain up to
`targets × samples` HTTP requests, bounded speed traffic, and finite UDP probes.
At 16 MiB per route every five minutes, speed tests alone can consume about
4.5 GiB/day **per route**. Enable them deliberately.

History retains the most recent 1,000 runs globally, atomically replaced in a
mode-0600 JSON file; the API returns the last 100 per route. Use one service
instance per history file. Retention is bounded, not a long-term analytics database.
A persistence failure leaves results in memory and exposes an error metric.

## UDP endpoint

Install this same binary on an **already available** remote machine, preferably
a host distinct from the VPN exit so the test also exercises Internet egress:

```sh
export VPN_MONITOR_UDP_TOKEN='a-long-random-shared-secret-at-least-24-characters'
bin/vpn-monitor --echo --echo-listen 0.0.0.0:9444
```

Set `udp.endpoint` to its **IP:port** and provide the same secret through
`udp.token_env` on the checker. The endpoint replies only to fixed-size,
HMAC-SHA256-authenticated 64-byte packets and never amplifies them. Restrict its
firewall to your VPN exit IPs and rate-limit it; authentication is not encryption
or replay prevention. No ports are opened automatically. UDP tests use SOCKS5 UDP
ASSOCIATE against the private local sing-box listener and never send probes
straight to the endpoint as a fallback. Without an endpoint they are `disabled`.
No ICMP privileges, TUN device, or `NET_ADMIN` are needed.

## Telegram and independent web access

Create a bot using BotFather. Set `telegram.token_env` and an explicit allowlist
of private `chat_ids`; then restart. No webhook/public inbound bot port is needed.

- `/status` — all routes and freshness.
- `/test server/protocol` — queue a full test.
- `/stats server/protocol` — latest detailed result.
- `/help` — command help.

State changes, including recovery, generate best-effort notifications. The initial
healthy result is silent. Notifications are not a durable delivery queue; repeated
unchanged failures do not spam the chat. If Telegram is blocked, results remain
available in the web UI, JSON history and metrics. Telegram polling/delivery never
runs in the test worker. An external monitor is still needed to detect workstation
outages, and remote web access itself needs a reachable path.

The dashboard shows all protocols, manual test buttons, direct and tunnel samples,
HTTP/UDP statistics, speed and recent history. The adapted shadcn/ui components
are locally owned source under `web/src/components/ui`, with `components.json`
configured for the shadcn CLI. It works without Telegram or external web services.

## API and Grafana

All `/api/*` endpoints and `/metrics` require `Authorization: Bearer <API token>`.
`GET /healthz` and static UI assets are public; healthz means the process is alive,
not that VPN routes work. Bind to loopback by default. For remote access, use a
trusted HTTPS reverse proxy, firewall and access control. Never expose the bearer
token over public plaintext HTTP. There are no config-editing or arbitrary-command
API endpoints, no cross-origin API access, and raw outbounds are never returned.
Result details use target aliases, not URLs, passwords or server credentials.

| Endpoint | Purpose |
| --- | --- |
| `GET /api/routes` | Configured routes, queued/running flag, freshness, latest result |
| `GET /api/history?route=server%2Fprotocol` | Last 100 completed runs, newest first |
| `POST /api/test` | JSON body `{"route":"server/protocol"}`; returns `202` |
| `GET /metrics` | Prometheus text exposition, labeled by `server`, `protocol` |

[`examples/prometheus.yaml`](examples/prometheus.yaml) shows authenticated scraping
and alert expressions. Import [`examples/grafana-dashboard.json`](examples/grafana-dashboard.json)
or provision it using your existing Grafana workflow; server/protocol variables
come from metrics rather than hard-coded panels. Existing Ansible/Grafana instances
are **not changed automatically**.

Key metrics: `vpn_monitor_healthy`, `vpn_monitor_state`,
`vpn_monitor_last_run_timestamp_seconds`, `vpn_monitor_http_failure_ratio`,
`vpn_monitor_http_latency_seconds`, `vpn_monitor_http_p95_seconds`,
`vpn_monitor_udp_loss_ratio`, `vpn_monitor_udp_rtt_seconds`,
`vpn_monitor_speed_bits_per_second`, `vpn_monitor_busy`,
`vpn_monitor_persistence_error`. Unsupported/disabled UDP/speed series are absent.
Alert on freshness, scrape availability and inconclusive results as well as failures.

## Development checks

```sh
go test -race ./...
go vet ./...
npm ci --prefix web
npm run build --prefix web
```

Tests use local HTTPS servers, a mock SOCKS UDP relay and authenticated echo.
The real sing-box integration test is local-only and skips if the binary is absent.
No live VPN credentials, Telegram token or Internet test targets are required.
Production protocol/server acceptance and real Telegram delivery must be verified
separately with your private configuration.

## Removing the old Ansible installation

The former Python probes, reference endpoint/DNS diagnostics, workstation Compose
project and special Grafana/Telegraf integration have been removed from the Ansible
source. VPN host provisioning remains there; this service does not replace it.
Source removal alone does not uninstall a running deployment. On an old installation,
stop its dedicated Compose project, remove its probe textfile input, disable/delete
its three VPN Grafana rules, and remove its old provisioning file before redeploying
Grafana. Keep unrelated monitoring and active VPN clients intact. Do not copy old
private configurations, cloud state, historical experiment logs or credentials
into this public repository.

MIT licensed. UI primitives follow the MIT-licensed shadcn/ui patterns.
