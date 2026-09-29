# Deployment guide

## Server (public host)

The short way, if the host already runs containers:

```bash
git clone https://github.com/tidyinfo/muse-proxy && cd muse-proxy
cp deploy/server.env.example server.env
sed -i "s/PASTE_GENERATED_SECRET_HERE/$(openssl rand -hex 32)/" server.env
chmod 600 server.env
docker compose up -d
```

`docker-compose.yml` publishes both ports on loopback and reads the secret
from a read-only file mount rather than an environment variable. Steps 4 and
5 below still apply: TLS termination and the reachability of `:2222` are
yours to arrange, and the container does not do them for you.

By hand:

1. Build: `make build` → `bin/muse-proxy-server`.
2. Create the secret file (owner-only):
   ```bash
   openssl rand -hex 32 > /etc/muse-proxy/secret
   chmod 600 /etc/muse-proxy/secret
   ```
   The file may contain either the raw secret or a `FWD_SECRET=<secret>` line.
3. Install the systemd unit from `deploy/systemd/muse-proxy-server.service`,
   adjust paths, `systemctl enable --now muse-proxy-server`.
4. nginx: include `deploy/nginx/muse-proxy.conf` in the TLS server block.
   It proxies `/fwd/` to the server's loopback HTTP port with WebSocket
   upgrade headers and **disables access logging** for that location
   (the secret is in the URL path).
5. Firewall: the forward listener (`127.0.0.1:2222` by default) stays on
   loopback; users reach it via the server itself or a jump host:
   ```bash
   ssh -J user@server -p 2222 user@127.0.0.1
   ```

## Client (egress-only host)

1. Build: `make build` → `bin/muse-proxy-client`.
2. Write the URL file (owner-only):
   ```bash
   printf 'FWD_URL=wss://example.com/fwd/<secret>\n' > /etc/muse-proxy/fwd.env
   chmod 600 /etc/muse-proxy/fwd.env
   ```
   To keep the secret out of the URL entirely, drop it from `FWD_URL` and add
   a `FWD_SECRET=` line instead — the server accepts either, and the client
   sends it as `Authorization: Bearer` (see [`SECURITY.md`](SECURITY.md)):
   ```bash
   printf 'FWD_URL=wss://example.com/fwd/\nFWD_SECRET=<secret>\n' > /etc/muse-proxy/fwd.env
   ```
3. Install `deploy/systemd/muse-proxy-client.service`, or run directly:
   ```bash
   muse-proxy-client -url-file /etc/muse-proxy/fwd.env -allow 127.0.0.1:22
   ```
   Add `-secret-file /etc/muse-proxy/fwd.env` when using the `FWD_SECRET=`
   form above.
4. The client honors `HTTPS_PROXY`/`https_proxy`/`NO_PROXY` for the
   outbound WebSocket (`CONNECT`).

## Web shell (optional)

`muse-proxy-server` serves the terminal page at `/shell/` on the same
loopback HTTP port as `/fwd/`, so step 4 of the server section covers the
routing. Two things the shipped `deploy/nginx/muse-proxy.conf` leaves
commented out are **required** before you expose it:

- `auth_basic` (or your SSO / mTLS) on the `/shell/` location. The page's
  own `<secret>` field authenticates the *client tunnel*, not the human —
  without a second gate, anyone who can reach the page and knows the secret
  gets a shell. Same for `/fwd/`: the secret is the only credential.
- `limit_conn` on `/shell/`: a browser WebSocket is long-lived, so a leaked
  tab holds a pty for as long as the read deadline allows.

The page loads xterm.js from a public CDN (`cdn.jsdelivr.net`). If the
client's network cannot reach it, the page renders blank. Vendor the two
files into the deployment (or a local mirror) if that matters.

## Platform support

The **client builds on Linux only** — it allocates a pty through `/dev/ptmx`
with the Linux-specific `TIOCGPTN`/`TIOCSPTLCK` ioctls, which do not exist on
macOS or Windows. The server has no such dependency and cross-compiles to any
target.

| | linux/amd64 | linux/arm64 | linux/arm (v7) | darwin/* | windows/* |
|---|---|---|---|---|---|
| `muse-proxy-server` | yes | yes | yes | yes | yes |
| `muse-proxy-client` | yes | yes | yes | no | no |

If the egress-restricted host is a Mac or a Windows box, run the client in a
Linux container or VM on it and set `-allow` accordingly.

## Verification

- Server log shows `client connected from ...`.
- `muse-proxy-client -h` and `muse-proxy-server -h` both start on the target
  host; a client that fails to build is the usual cause of a silent tunnel.
- From the server: `ssh -p 2222 user@127.0.0.1` → lands on the client host.
- `scp -P 2222 file user@127.0.0.1:/tmp/` works (independent stream).
- With the web shell exposed: `GET /shell/` returns 401 without
  credentials and 200 with them; the `/shell/ws/` handshake returns 101.

## Notes for restricted sandboxes

If the client host rotates proxy credentials per process (as some sandboxes
do), a static systemd `Environment=` will go stale. Use a supervisor that
re-reads fresh credentials on each restart (e.g. a cron watchdog that
relaunches the client), rather than baking credentials into the unit.
