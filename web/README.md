# Web shell frontend (protocol v2)

`shell.html` is a minimal [xterm.js](https://xtermjs.org/) terminal page for
the web-shell path. It is intentionally small: authentication, theming and
reconnect UX live at the deployment layer (nginx), not here.

## Protocol (v2, implemented)

The page opens `wss://host/shell/ws/<secret>` and speaks the multiplexed
frame protocol with a single stream (id 1):

- `OPEN_SHELL (0x06)` payload `{"cols":N,"rows":M}` — request a pty
- `DATA (0x02)` — keystrokes up / pty output down
- `WINCH (0x07)` payload `"COLSxROWS"` — terminal resize
- `CLOSE (0x03)` — end the session

The secret is typed into a password field on the page and kept in a JS
variable; it never appears in the page URL (no `?secret=`), so it stays out
of browser history and Referer headers.

The server side (`/shell/` page + `/shell/ws/` bridging) is implemented in
`internal/server`; the pty side in `internal/client`. Details in
`PROTOCOL.md` ("Web shell (v2)").

## Security

Do not serve this page without authentication in front of it. See
`../docs/SECURITY.md`: put it behind SSO, basic auth, or client
certificates at the reverse proxy.
