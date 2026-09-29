# Maintaining this project

Written for whoever ends up holding the baton. If that is you: welcome, and
please fix anything on this page that has gone stale.

## Scope

`muse-proxy` is deliberately small. It exists because port forwarding, reverse
SSH and L3 tunnels all require inbound connectivity, and some environments
deny inbound at a layer below anything you can configure. See
[`RESEARCH.md`](RESEARCH.md) for the measurements that motivated it.

If a change does not serve "carry inbound requests down an already-open
outbound connection", it probably does not belong here. In particular, do not
add an L3 tunnelling mode: it was built once, it works at the IP layer, and it
still cannot reach a service on a host that discards packets destined to a
listening socket.

## Ground rules

1. `go test -race ./...` and `go vet ./...` must pass. That is the whole gate.
2. `PROTOCOL.md` is a contract between client and server, which may be
   deployed at different versions. Any protocol change updates it **in the
   same commit**, and adds a test in `internal/proto`.
3. v1 frame types are frozen. New behaviour uses reserved type bytes, and
   receivers ignore unknown types — that is what lets an old peer talk to a
   new one. `TestUnknownFrameTypesAreDecodable` guards it.
4. No secrets, hostnames, or internal addresses in the repository. This is
   checked before every push; see below.
5. `CHANGELOG.md` gets an entry under `[Unreleased]` with every user-visible
   change.

## Before you push

```bash
make fmt vet test          # or: gofmt -l . ; go vet ./... ; go test -race ./...
git grep -InE '/home/[a-z]+|192\.168\.|10\.[0-9]+\.[0-9]+\.[0-9]+' -- \
  ':!go.sum' ':!LICENSE' ':!RESEARCH.md'
```

The second command is the leak check. It is crude on purpose: a pattern that
catches a real hostname is worth more than one that reads nicely. It has
already caught a deleted script that was still recoverable from git history,
which is why the check runs against history too when you rewrite:

```bash
git rev-list --all | while read c; do
  git grep -IlE '/home/[a-z]+|\b10\.[0-9]+\.[0-9]+\.[0-9]+' "$c" \
    -- ':!RESEARCH.md' 2>/dev/null
done
```

Documentation examples use addresses from `198.18.0.0/15` (RFC 2544
benchmarking), never `10.x`, never a real public IP. That range is reserved
and never routable, so an example cannot be mistaken for a live host — and it
keeps the leak check above from crying wolf on every doc.

The check deliberately does **not** match `127.0.0.1:<port>`. Every default
in this project is a loopback address with a port (`127.0.0.1:18080`,
`127.0.0.1:2222`, `127.0.0.1:22`), and they appear in the README, the
nginx snippet, the systemd units, `docker-compose.yml` and the flag
definitions. Matching them made the check report eleven findings on a clean
tree, which is the fastest way to teach people to ignore it.

**If something sensitive is ever committed, rewriting history is the only fix.**
GitHub keeps unreachable objects reachable through the fork network and cached
views. A `git commit --amend` is not a fix. Rewrite from the first bad commit,
force-push, and ask people who cloned to re-clone.

## Releasing

- `git tag -a vX.Y.Z -m "..."` then push the tag; the release workflow builds
  and attaches the artifacts.
- The workflow runs goreleaser **twice on purpose**: once with `--skip=publish`
  to prove the whole build works, then again to publish. If a release fails,
  the step that failed tells you which half broke — worth keeping, because
  reading the log needs repo admin or a PAT, and step conclusions are visible
  to anyone.
- The release is **not** a draft. An unattended tag push cannot publish a
  draft (that needs a browser), so a draft means a release nobody can see
  while the container image is already public.
- Never re-point a published tag. If a release is broken, cut `vX.Y.Z+1`; the
  artifacts people already downloaded keep the old name, and rewriting the tag
  silently invalidates every signature anyone has checked.
- The client only builds for Linux. Do not add a Windows or macOS client job
  until `openPty` uses `posix_openpt` — see the platform table in `README.md`.
- Artifacts are stripped on purpose. Go keeps its line table (`pclntab`) in the
  data section, so panic traces are fully symbolised with file and line either
  way; `-s -w` only drops DWARF, which matters solely to an interactive
  debugger attached to a running process. Verified, not assumed.
- Images are built from a `$BUILDPLATFORM` stage so no target-architecture code
  is ever executed. Keep it that way: putting `apk add` in the final stage
  makes the arm64 build depend on QEMU being registered on whatever machine
  runs the release.

## Version skew between the two binaries

The client and server are upgraded independently, and **the wire protocol has
no version handshake**. A mismatched pair does not fail cleanly — streams open
and die seconds later. Both binaries log their version on connect; if an issue
looks like unexplained stream churn, compare those two lines first:

```
client connected from 198.18.0.7:54321 (muse-proxy wire protocol v2)
websocket connected (muse-proxy wire protocol v2)
```

v1 and v2 were additive, so skew costs features rather than the connection.
**Any future breaking framing change must bump `proto.Version`, be called out
in the changelog as breaking, and tell operators to upgrade the server last.**

## Answering issues

Most reports will be one of these, and all of them are known:

- **"It hangs, no error."** That is the behaviour this project exists to route
  around. If it is the *client* hanging rather than the tunnel, the client
  host is probably the restricted one. See `RESEARCH.md` §7.
- **"Ping works but SSH doesn't."** Expected. Ping does not go to a socket.
  See `RESEARCH.md` §3 — closed ports answering with RST is what makes this
  so confusing.
- **"Doesn't run on my Mac/Windows."** The client is Linux-only. The table in
  `README.md` says so; point them at it rather than re-explaining.
- **"Streams open then die after a few seconds."** Compare the wire-protocol
  version both ends log on connect. See "Version skew" above.
- **"Antivirus quarantined it."** A binary that opens `/dev/ptmx`, spawns a
  shell and bridges loopback TCP over a WebSocket matches the heuristic
  signature of a C2 agent. It is a false positive. `checksums.txt` and the
  signed provenance are there so users can verify rather than take your word.

## If you are picking this up from someone else

The three files that carry the institutional memory, in order:

1. [`RESEARCH.md`](RESEARCH.md) — why the design is shaped this way, including
   everything that was tried and did not work. Do not delete the dead ends;
   they are the most expensive part of the knowledge to recreate.
2. [`docs/SECURITY.md`](docs/SECURITY.md) — the threat model, and what the
   secret does and does not authenticate.
3. [`CHANGELOG.md`](CHANGELOG.md) — including removals. The WireGuard/UDP
   forwarding that was taken out is recorded there with the reason, so nobody
   re-adds it.
