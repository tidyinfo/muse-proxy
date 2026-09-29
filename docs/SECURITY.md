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

- The secret is a **bearer token**: anyone holding it can impersonate the
  client and receive forwarded streams. There is nothing else authenticating
  either end.
- Generate with `openssl rand -hex 32`. Never commit it, never log it.
- Compare secrets in constant time (the server does, for both carriers).
- Rotate on suspicion: change the secret server-side and in the client's
  url-file, then restart both. During rotation, invalidate the old secret
  first: with two valid secrets in flight, either one opens the tunnel, so
  the compromised value stays live until both ends agree on the new one.
  Keep a short overlap window only if you must avoid downtime, and log the
  cutover.

### Two carriers, either is sufficient

The server accepts the secret in **either** place, and does not care which
one arrives:

| Carrier | Client | Where it leaks |
|---|---|---|
| URL path — `wss://host/fwd/<secret>` | default | reverse-proxy access logs, any `Referer`, browser history |
| `Authorization: Bearer <secret>` | `-secret-file` | essentially nowhere; it is a header, not a URL |

Either one matching opens the tunnel, and both comparisons always run. There
is no precedence: if you use the header form, whatever the path segment
happens to be is ignored.

**Which to use.** The path form is the default because a reverse proxy can
switch access logging off per-location without inspecting a single header —
one line of nginx, no log parser, no risk of missing a field. That is a real
operational advantage and it is why the path is not going away.

Use the header form when your reverse proxy cannot be configured that way, or
when you would simply rather the secret never be part of a URL:

```bash
# client: FWD_URL without the secret, plus a FWD_SECRET= line
#   in the file passed to -url-file
FWD_URL=wss://host/fwd/
FWD_SECRET=8f14e45fceea167a5a36dedd4bea2543
muse-proxy-client -url-file /etc/muse-proxy/fwd.env
```

If you use the path form, **disable access logging for the `/fwd/` and
`/shell/` locations** (see `deploy/nginx/muse-proxy.conf`, which also scrubs
the request URI for the error log). TLS does not help here: the point at
which the path is written to a log is usually *after* termination.

The browser web shell always uses the path form. That is not a leak: the
page builds the WebSocket URL in JavaScript from a value the operator just
typed, so the secret never reaches a navigation, a history entry, or a
`Referer` header. It is still subject to access logging, so the same nginx
rule applies.

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
