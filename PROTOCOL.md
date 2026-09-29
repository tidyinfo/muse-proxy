# muse-proxy wire protocol

Version: 2 (TCP forwarding + web shell).

## Transport

- One WebSocket connection, always dialed **client → server**:
  `wss://host/fwd/<secret>`.
- The web terminal page is served at `https://host/shell/`; the page opens
  `wss://host/shell/ws/<secret>` back to the server, which bridges it to the
  same client connection.
- Only binary messages are significant; text messages MUST be ignored.
- The server authenticates by the `<secret>` path segment and MUST reject
  the handshake (HTTP 403, no upgrade) on mismatch. Secrets are compared in
  constant time.
- Production deployments MUST use WSS (TLS). The outer TLS is the only
  encryption on the wire.

## Framing

Every binary message is exactly:

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|      type     |                    stream id                    |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                          payload ...                            |
```

- `type`: 1 byte. `stream id`: 4 bytes, big-endian. `payload`: the rest.
- Messages shorter than 5 bytes MUST be ignored.
- DATA payloads SHOULD be ≤ 32 KiB per frame; receivers MUST accept
  arbitrarily split/coalesced DATA.

## Frame types

| Type | Name  | Stream id | Payload              |
|------|-------|-----------|----------------------|
| 0x01 | OPEN  | S         | `"host:port"` UTF-8  |
| 0x02 | DATA  | S         | opaque bytes         |
| 0x03 | CLOSE | S         | empty                |
| 0x04 | PING  | 0         | empty                |
| 0x05 | PONG  | 0         | empty                |
| 0x06 | OPEN_SHELL | S | `{"cols":N,"rows":M}` UTF-8 JSON |
| 0x07 | WINCH      | S | `"COLSxROWS"` UTF-8, e.g. `120x30` |
| 0x08-0x09 | — | — | removed (were OPEN_UDP / DATAGRAM) |

Implementations MUST ignore unknown frame types.

## Stream lifecycle (v1)

- Only the **server** opens streams; the client never initiates.
- The server allocates stream ids: SHOULD start at 1 and use odd numbers,
  MUST NOT reuse an id while its stream is live.
- On OPEN, the client checks the target against its allowlist:
  - allowed → dials it (TCP, 10 s timeout) and starts piping;
  - refused or dial failed → replies CLOSE for that id.
- CLOSE is advisory: after sending/receiving CLOSE for S, a side MUST
  release all state for S. A side MAY skip CLOSE if the WebSocket itself
  is being torn down.
- Either side may close its local TCP half at any time; the peer observes
  EOF via a final DATA (possibly empty) followed by CLOSE.

## Keepalive

- The client sends PING every 25 s (configurable).
- The server replies PONG to every PING.
- If the client sees no PONG for 3 consecutive intervals, it MUST drop the
  connection and reconnect with exponential backoff (5 s → 60 s max, jitter
  optional).
- The server SHOULD close idle connections after 5 minutes without PING.

## Web shell (v2)

A browser page (`/shell/`) opens `wss://host/shell/ws/<secret>`; the server
bridges that WebSocket to one pty stream on the client connection:

- The browser's first binary frame MUST be OPEN_SHELL with
  `{"cols":N,"rows":M}`; otherwise the server closes the session.
- The server allocates an odd stream id S and forwards OPEN_SHELL(S) to the
  client. Browser frame ids are ignored — the bridge is 1:1.
- On OPEN_SHELL the client spawns a login shell on a fresh pty
  (`/bin/bash -i`, falling back to `/bin/sh`), sized to cols×rows:
  - shell disabled (`-disable-shell`) or spawn failure → replies CLOSE(S).
  - duplicate stream id → replies CLOSE(S), keeps the existing stream.
- DATA is bidirectional: keystrokes browser → pty, terminal output pty →
  browser (server rewrites the stream id to the browser's id).
- WINCH (`"COLSxROWS"`) resizes the pty; malformed values are ignored.
- When the shell exits, the client sends CLOSE(S); the server then closes
  the browser WebSocket. Browser CLOSE ends the session the same way.
- The server applies its idle read deadline to browser sessions, so a dead
  tab cannot hold a pty forever.
- Access control for `/shell/` (who may open a terminal) is the deployer's
  job: put authentication in front of it at the reverse proxy
  (see `docs/SECURITY.md`). The `<secret>` alone is a shared bearer token.
- The page itself carries no secret: the user types it into a password field
  and the page keeps it in a JS variable. Never put the secret in the page
  URL — it would leak into browser history and Referer headers.

## Session takeover (single-client)

The server holds **one** client session at a time. A new `/fwd/<secret>`
handshake is an explicit takeover, not a rejection:

- The server MUST close the previous client WebSocket and every stream it
  owned, then adopt the new connection with an empty stream table.
- The outgoing session's teardown MUST NOT touch the incoming session's
  streams. Implementations tag each session with a generation counter and
  gate teardown on it; otherwise a stream opened just after a reconnect gets
  killed by the old session's cleanup, and the peer sees a timeout rather
  than a reset.
- Stream ids are allocated from a counter that is **not** reset by a
  takeover, so ids stay unique across reconnects.
- A client that sees its WebSocket closed with no preceding CLOSE for a live
  stream should treat the stream as dead and reconnect; it must not attempt
  to resume it.

## Version skew

The client and the server are deployed independently and are often upgraded at
different times. **There is no version negotiation on the wire** — a v0.1.0
client and a v0.2.0 server that changed the framing will not detect the
mismatch, and the symptom is streams that open and then die seconds later with
no useful error.

What makes it survivable: v1 and v2 were both additive. New frame types use
previously reserved bytes and receivers ignore unknown types, so a v2 peer
talking to a v1 peer loses only the features v2 added — the connection and
TCP forwarding keep working. A future *breaking* change would break that, and
the release notes must say so loudly when one happens.

The practical check, and the reason both binaries print their version on
connect:

```bash
# server log
client connected from 198.18.0.7:54321 (muse-proxy wire protocol v2)
# client log
websocket connected (muse-proxy wire protocol v2)
```

Mismatched numbers there explain a class of bug reports that has no other
symptom. Keep the two ends in step when upgrading, and upgrade the server
last so an old client keeps working throughout.

## Operational notes

- The secret appears in the request path: disable access logging for the
  `/fwd/` location (see `deploy/`).
- Proxies: the client honors `HTTPS_PROXY`/`https_proxy` (`CONNECT`) and
  `NO_PROXY`, mapping `ws`/`wss` schemes for the lookup.
- Concurrency: streams are independent; one slow/dead stream MUST NOT
  block others. Implementations MUST serialize WebSocket writes.
