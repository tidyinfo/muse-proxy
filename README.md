# muse-proxy

Outbound-only hosts, reachable anyway.

Some sandboxes, NAT boxes and locked-down VMs can open outbound WebSocket
connections but cannot accept inbound traffic at all: inbound TCP answers are
filtered, raw sockets are neutered. `muse-proxy`
turns a single outbound WebSocket into a multiplexed tunnel that carries real
TCP streams and interactive shells back into
the host.

## Architecture

```
  your laptop / phone (ssh, scp, browser)
        │  plain TCP / HTTPS
        ▼
  ┌──────────────┐   WSS (always dialed OUT by the client)  ┌──────────────┐
  │    server    │◄────────────────────────────────────────│    client    │
  │ (public host)│   one persistent connection,             │ (egress-only │
  │  :2222 TCP   │   many independent streams               │  host)       │
  │  /shell web  │                                          │  → 127.0.0.1 │
  └──────────────┘                                          └──────────────┘
```

The client always dials out; the server never dials in. Each inbound TCP
connection on the server becomes one stream; the client forwards it to an
allowlisted local target (e.g. `127.0.0.1:22`).

## Components

- `cmd/muse-proxy-client` — runs on the egress-only host. Dials
  `wss://server/fwd/<secret>`, holds the multiplexed connection, forwards
  each stream to the allowlist. Auto-reconnect with backoff, keepalive pings.
- `cmd/muse-proxy-server` — runs on the public host. Validates the secret,
  accepts TCP on a loopback port, bridges each connection to a stream; serves
  the web-shell UI.
- `internal/proto` — the frame protocol both sides share. See `PROTOCOL.md`.
- `web/` — browser terminal for the web-shell path.
- `deploy/` — systemd units, nginx snippets, env examples.
- `docs/` — `SECURITY.md`, `DEPLOYMENT.md`, `CONTRIBUTING.md`.

## Protocol

Versioned, framed binary messages over one WebSocket. v1 carries TCP
streams; v2 adds the shell/pty frames. Full spec:
[`PROTOCOL.md`](PROTOCOL.md).

## Quickstart (forwarding path)

```bash
# 1. one secret per deployment, never commit it
openssl rand -hex 24

# 2. public host
muse-proxy-server -secret <secret> -tcp 127.0.0.1:2222 -http 127.0.0.1:18080

# 3. egress-only host (behind nginx: wss://example.com/fwd/<secret>)
muse-proxy-client -url wss://example.com/fwd/<secret> -allow 127.0.0.1:22

# 4. from anywhere that can reach the server
ssh -p 2222 user@127.0.0.1        # on the server itself
ssh -J user@server -p 2222 user@127.0.0.1   # via jump host
```

## Security model (summary)

- The secret is a bearer token in the URL path: treat it like a password,
  never commit it, rotate on suspicion.
- The client allowlists forwarding targets — it can never be steered into
  becoming an open proxy.
- The server should listen on loopback only; expose further access via your
  own jump host / firewall rules.
- **`/shell/` needs a second authentication layer.** The `<secret>` on that
  page authenticates the *client tunnel*, not the human in front of the
  browser. Reverse-proxy it behind basic auth, mTLS or SSO before exposing
  it, or anyone who reaches the page and knows the secret gets a shell.
  Details and threat model: `docs/SECURITY.md`.

## Roadmap

- v1: TCP forwarding (implemented, tested)
- v2: web shell — pty sessions over the same multiplexed connection
  (implemented, tested; page at `/shell/`)
- later: per-stream ACLs, audit logging

## License

MIT — see [LICENSE](LICENSE).
