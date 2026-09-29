// Package web embeds the web shell frontend so the server can serve it
// from a single binary.
package web

import _ "embed"

// ShellHTML is the xterm.js terminal page (protocol v2, see PROTOCOL.md).
//
//go:embed shell.html
var ShellHTML []byte
