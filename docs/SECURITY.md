# Security model

## What muse-proxy is

A reverse-access tunnel: an egress-only host dials **out** one WebSocket to
a public server, and the server bridges inbound TCP connections back through
it. The threat model follows from that shape.

## Trust boundaries

| Actor | Trust |
|---|---|
| The **server** operator | Fully trusted by the client deployer: they choose where streams go only via the client's own allowlist, but a malicious server sees all forwarded plaintext and can open unlimited streams. Run the server on infrastructure you control. |
| The **client** host | Trusted to enforce its allowlist. A compromised client can be steered only to its allowlisted targets — keep that list minimal (`127.0.0.1:22`, nothing else, until you need more). |
| The **network** | Untrusted. All traffic runs inside TLS (WSS). Never deploy WS without TLS. |

## The secret

- The `<secret>` in `/fwd/<secret>` is a **bearer token**: anyone holding it
  can impersonate the client and receive forwarded streams.
- Generate with `openssl rand -hex 24`. Never commit it, never log it.
- The secret appears in the request path: **disable access logging** for the
  `/fwd/` location (see `deploy/nginx/muse-proxy.conf`).
- Compare secrets in constant time (the server does).
- Rotate on suspicion: change the secret server-side and in the client's
  url-file, then restart both. During rotation, invalidate the old secret
  first: with two valid secrets in flight, either one opens the tunnel, so
  the compromised value stays live until both ends agree on the new one.
  Keep a short overlap window only if you must avoid downtime, and log the
  cutover.

## Client hardening

- **Allowlist is the whole policy.** The client refuses every target not on
  `-allow`. It cannot be turned into an open proxy, even by a malicious
  server.
- Prefer `-url-file` over `-url` so the secret never appears in `ps` output.
- Run as an unprivileged user with a systemd unit (`deploy/systemd/`).

## Server hardening

- Bind the TCP forward listener to **loopback only** (`127.0.0.1:2222`).
  Further access (jump hosts, firewall rules) is the operator's job.
- Bind the `/fwd/` HTTP endpoint to loopback behind nginx; terminate TLS at
  nginx.
- Without a connected client, inbound TCP is refused immediately (fail
  fast); nothing is queued.
- Rate-limit `/fwd/` at nginx if you expect secret-guessing attempts.

## Web shell (v2)

- Each shell is a separate pty, one per stream; streams are isolated.
- Do not expose the web UI without authentication: put it behind your SSO,
  basic auth, or client certificates at the nginx layer
  (see `deploy/nginx/muse-proxy.conf`).
- The `<secret>` in `/shell/ws/<secret>` is the same bearer token as
  `/fwd/`; disable access logging for `/shell/` too.
- Idle browser sessions are dropped by the server read deadline; a dead tab
  cannot hold a pty forever.
- Operators who only need TCP forwarding can start the client with
  `-disable-shell`, which makes it refuse every `OPEN_SHELL`.
- Consider command audit logging per your compliance needs (not built in).

## Reporting vulnerabilities

Do not open public issues for security bugs. Contact the maintainers
privately (address TBD before first release).
