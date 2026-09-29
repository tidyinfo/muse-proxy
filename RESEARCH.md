# When a host silently drops every inbound connection

A field report from building this project. Everything here was measured on a
live, hardened host; the numbers are real, the reproduction steps are the ones
actually used.

**Summary:** on at least one hardened environment, every inbound TCP *and* UDP
packet is discarded **if and only if a socket is listening on the destination
port**. Ports with no listener behave normally — the kernel answers with RST or
ICMP port-unreachable. ICMP echo works throughout. The result is a host that
looks reachable to every naive reachability test you would run, and is
completely unreachable for the thing you actually want to do.

---

## 1. The symptom

The obvious approaches to reaching a host that sits behind a restrictive
firewall all failed on the same machine, in the same way: no error, no RST, no
log entry, just a hang until the client timed out.

```
$ ssh -p 2222 root@198.18.0.9
^C
$ ping -c2 198.18.0.9
2 packets transmitted, 2 received, 0% packet loss, time 1001ms
rtt min/avg/max/mdev = 315.639/550.168/784.698/234.529 ms
```

Ping works. SSH does not. The tunnel, the WireGuard interface, the reverse
socket, the port number — none of it worked. Meanwhile the same code, the same
keys and the same network path worked perfectly on an ordinary machine, which
is what made this worth chasing rather than dismissing as misconfiguration.

## 2. What ruled things out first

Before theorising, four hypotheses were tested and killed:

| Hypothesis | How it died |
|---|---|
| Firewall rules | `iptables -S INPUT` → `-P INPUT ACCEPT` with **zero rules**; `nft list ruleset` → empty |
| Address/routing mismatch | A single network namespace for all processes; the interfaces carried the expected addresses |
| Application not listening | `ss -ltnp` showed the daemon bound to `0.0.0.0:PORT` and `[::]:PORT`; `sshd -T` confirmed `listenaddress 0.0.0.0` |
| Kernel module / cgroup shenanigans | `/proc/self/cgroup` → `0::/`; `bpftool prog show` → nothing loaded |

The environment was *not* quietly filtering with netfilter. That is the finding
that killed the easy explanations and forced the measurements below.

## 3. The measurement that mattered

Packet counting, not packet capture. A WireGuard interface's transfer counters
give a clean byte-level differential for every probe, without needing capture
privileges inside the restricted host.

`recv`/`sent` below are the interface's own counters across a single
`connect()` attempt. `recv delta = 0` means literally nothing came back.

| Probe (via the tunnel) | listener? | sent Δ | recv Δ | reading |
|---|---|---|---|---|
| `198.19.1.9:22` | yes (sshd) | 192 | **0** | dropped |
| `198.19.1.9:443` | no | 96 | 80 | RST |
| `198.19.1.9:2222` | no | 96 | 80 | RST |
| `198.19.1.9:80` | no | 96 | 80 | RST |
| `198.19.1.9:9998` | no | 188 | 260 | RST |
| `198.19.1.9:9999` | **yes** (`python3 -m http.server`) | 288 | **0** | dropped |
| `198.19.1.9:9997` | **yes** (UDP echo) | — | **0** | dropped |
| `198.19.1.9:9996` | no (UDP) | — | ICMP unreach | ICMP returned |

Two rows do the whole argument:

- **Row 6 kills "port 22 is blocked."** A stock Python HTTP server on port
  9999 was dropped exactly like sshd on 22. Nothing about sshd, nothing about
  port 22 — it is the *existence of a listening socket*.
- **Row 8 kills "TCP only."** UDP behaves the same way, and the ICMP
  port-unreachable that came back for the closed UDP port proves the kernel's
  UDP path was alive and answering the whole time.

And the mirror image of this is the thing that wastes your afternoon: for
every port with **no** listener, the host answers promptly and correctly. A
port scan of the restricted host looks perfectly healthy. So does `ping`. So
does every "is it up?" check you would reach for first.

## 4. The control that pins it down

The one test that made this airtight. From **inside** the restricted host:

```
198.19.1.9:22    -> Connection refused
198.19.1.9:80    -> Connection refused
198.19.1.9:9999  -> Connection refused
127.0.0.1:22   -> OK
```

The host refuses to connect to **its own** address on every port, including
ports where a process is demonstrably listening on `0.0.0.0`. A local
`connect()` to `198.19.1.9:22` should be matched by a socket bound to
`0.0.0.0:22` and succeed. It returns `ECONNREFUSED` instead.

netfilter cannot produce that behaviour, and nothing was listening on a
non-loopback address to collide with. The `connect()` is being intercepted
before it reaches the protocol stack.

## 5. The rule

> Inbound traffic is discarded when, and only when, it would be delivered to
> an existing listening socket.

TCP and UDP alike. Unaffected: ICMP, the outgoing direction, and the return
path for connections the host initiates itself.

The most economical model that fits every observation is a hook at the socket
delivery layer — `BPF_CGROUP_INET{4,6}_INGRESS` has exactly these semantics,
because it fires on packets matched to a real receiving socket. Traffic to a
closed port never gets that far: it is answered from the earlier no-listener
branch, which is why those look healthy.

The exact mechanism was not confirmed — the host is not ours to instrument.
The rule above is what the data supports; the mechanism is the simplest model
consistent with it.

## 6. Why the obvious fixes do not work

Everything below was tried, and every one of them is a dead end:

- **A different port.** `sshd` was given a second port; the second port was
  dropped identically. The rule is about *having* a listener, not about which
  port it is.
- **A different protocol.** UDP failed the same way as TCP (§3).
- **A different source address.** Irrelevant: the discriminator is on the
  receiving side.
- **Firewall rules on the host.** There are none, and adding some cannot
  un-drop traffic the host is discarding below netfilter.
- **A L3 tunnel (WireGuard over WebSocket).** The tunnel itself was perfect —
  handshakes, keepalives, bidirectional byte counters, and working ICMP
  through it. It carried the SYN all the way to the receiving socket, and the
  SYN was dropped at the last step. **A perfectly working tunnel that delivers
  packets into a host which refuses to accept them is still useless for
  reaching services on that host.**

## 7. What does work

Only one thing: **connections the restricted host initiates itself.**

The host can open outbound TLS/WebSocket just fine. So put the listener on the
*other* side and carry the inbound request back down the already-open outbound
connection. That is the entire design of this project, and §6 is the list of
alternatives that measurement eliminated.

The port scan result deserves a second mention for anyone writing tooling: a
naive "is this host up?" check will report this host as healthy. The correct
test is a completed TCP handshake against a port that is *actually* served —
not a ping, not a closed-port RST, not a port scan.

## Reproducing

On any host, the observation reduces to two probes:

```bash
# a) with something listening
python3 -m http.server 9999 --bind 0.0.0.0 &
nc -vz -w3 <host> 9999     # or: timeout 3 bash -c 'cat </dev/null >/dev/tcp/<host>/9999'

# b) with nothing listening
nc -vz -w3 <host> 9998     # and nothing on 9998
```

On an ordinary network both return promptly. On a host exhibiting the
behaviour, (a) hangs and (b) is refused or resets immediately — that
divergence *is* the signature. `nc -z` on the closed port is the control that
rules out "everything is being dropped."
