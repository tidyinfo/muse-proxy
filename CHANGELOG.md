# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Removed
- WireGuard-over-WSS and UDP datagram forwarding (never released; removed
  before any release cut). The tunnel could not be used to reach services on
  the remote cell: that host's sandbox drops any inbound packet destined to
  an existing listening socket, for TCP *and* UDP alike. Changing port,
  protocol or address does not help — only cell-initiated (outbound)
  connections work, which is what the WebSocket + TCP forward already
  provides. Removed: `0x08`/`0x09` frames, the `internal/wgbind` package,
  the `-wg-*` client flags, `-udp-allow` / `-udp-idle-timeout` server flags,
  and `docs/UDP_FORWARDING.md`.

### Added
- **Bearer header authentication.** The server accepts the client secret as
  `Authorization: Bearer <secret>` in addition to the URL path, and the
  client sends it when `-secret-file` names a file containing `FWD_SECRET=`.
  Either carrier is sufficient and both comparisons always run. This is
  additive: the path form is unchanged and remains the default, because a
  reverse proxy can disable access logging per-location without inspecting
  headers. It exists for deployments where the secret should never be part
  of a URL. See `docs/SECURITY.md`.
- `docker-compose.yml` for the server, plus `deploy/server.env.example`.
  Loopback-bound published ports, read-only root filesystem, all capabilities
  dropped, secret mounted read-only rather than passed as an environment
  variable.
- `muse-proxy-client`: dials `wss://host/fwd/<secret>`, multiplexes TCP
  streams to allowlisted local targets, auto-reconnect with backoff,
  keepalive PING/PONG.
- `muse-proxy-server`: validates the client secret, bridges a loopback TCP
  listener to client streams, answers PINGs.
- `internal/proto`: shared frame codec for the v1 wire protocol.
- `PROTOCOL.md`: wire-protocol specification (TCP framing; the web shell was specified as a v2 reservation and has since shipped).
- `deploy/`: systemd units, nginx snippet, env example.
- `docs/`: `SECURITY.md`, `DEPLOYMENT.md`, `CONTRIBUTING.md`.
- CI workflow (gofmt, vet, tests with `-race`, multi-version build).
- Web shell (v2): `internal/client` spawns a pty login shell on
  `OPEN_SHELL` (native `/dev/ptmx` via `golang.org/x/sys`, no extra
  dependencies), handles `WINCH` resizes, tears down on `CLOSE`/exit;
  `internal/server` serves the embedded xterm.js page at `/shell/` and
  bridges `wss://host/shell/ws/<secret>` to the client pty stream;
  `-disable-shell` client flag refuses shell streams.
- `web/web.go`: embeds `shell.html` into the server binary.
- `PROTOCOL.md`: v2 web-shell lifecycle section.
- The `/shell/` page takes the secret from a password field, never from the
  URL; the client has a `-disable-shell` flag independent of the TCP
  allowlist.

### Fixed
- A mistyped `KEY=value` line in a `-secret-file` was silently accepted as the
  raw secret, producing a server that answers 403 to everything with nothing in
  the log to explain why. It is now rejected at startup with the offending line
  quoted. (Found by writing the key wrong myself.)
- Docs/infra no longer reference removed features. The systemd units and the
- Docs/infra no longer reference removed features. The systemd units and the
  README quickstart told operators to pass flags that no longer exist
  (`-udp-allow`, `-wg-*`, `-listen`), so following them verbatim produced a
  server that refused to start; the README used `-listen` where the flag is
  `-tcp`. CI also pinned Go 1.21/1.22 while `go.mod` requires 1.23.1, so the
  matrix could not have passed. `CONTRIBUTING.md` referred to branch `main`
  (the repo's branch is `master`) and to the `DisableShell` Go field where
  the CLI flag is `-disable-shell`.
- `PROTOCOL.md` now specifies session takeover. It was the one protocol-
  visible behaviour left undocumented: a second client handshake evicts the
  first, and an outgoing session's teardown must not touch the incoming
  session's streams. Deployments that mix versions have no written contract
  for it.
- `docs/DEPLOYMENT.md` now covers the web shell, which it did not mention at
  all, including the two nginx directives the shipped snippet leaves
  commented out and that are required before exposing `/shell/`.
- `internal/proto`: added `TestUnknownFrameTypesAreDecodable`. Both
  `PROTOCOL.md` and `CONTRIBUTING.md` claimed forward compatibility was
  tested; no such test existed. Type bytes 0x08/0x09 are a live example —
  a peer built from the unreleased UDP draft still emits them.
- Removed site-specific content that leaked deployment details (a real
  hostname, a counterparty's home directory, internal tunnel addresses) and
  deleted `run-client.sh`, which duplicated `docs/DEPLOYMENT.md` and was the
  only place those paths appeared.
- `Makefile` listed a `lint` target in `.PHONY` that did not exist.
- `internal/server`: the doc comment for `attach()` had drifted onto
  `closeStreamsLocked`, so neither function was documented correctly.
- `internal/server`: reconnect race in `attach()` — the outgoing session's
  teardown walked the shared stream map after a takeover and killed streams
  opened by the incoming session. The server now tags each session with a
  generation counter: takeovers explicitly close the previous generation's
  streams, and a teardown only cleans up when its generation is still
  current. Regression test `TestReconnectDoesNotKillNewSessionStreams`
  pins the interleaving with a test-only hook (nil in production).
- `internal/client`: race between async `OPEN` handling and the first `DATA`
  frame — a banner sent immediately after connect (e.g. an SSH client
  banner) could be silently dropped when it arrived before the local dial
  finished, making sshd answer "Invalid SSH identification string".
  `OPEN`/`OPEN_SHELL` are now processed synchronously in the reader loop,
  so the stream is always registered before later frames for that id are
  handled. Regression test `TestOpenDataOrdering` pins the ordering with a
  blocking test dial (fails on the old code, passes on the new).
