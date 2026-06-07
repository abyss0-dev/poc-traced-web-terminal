# Design Doc: Traced Web Terminal

## 1. Background and Objective

The `poc/web-terminal` PoC established a platform-agnostic, multi-target Web
Terminal: a browser-facing **BFF** and an infrastructure-facing **Gateway (GW)**
bridge a browser to selectable QEMU guests, with the transport (SSH) hidden
behind a swappable **Runtime** abstraction.

This PoC extends that pipeline with a second, read-only data path: while the
operator drives a guest interactively from the browser, the kernel-level
behavior of that same guest — captured by the **bloodhound** eBPF tracer running
inside the VM — is streamed back to the browser and displayed live alongside the
terminal.

The question under test is narrow and concrete: **can in-VM eBPF observation be
surfaced to the browser in real time, on the same screen as interactive
operation, without disturbing the existing terminal pipeline?**

**Objectives:**
- Reuse the `web-terminal` BFF + GW + Runtime pipeline unchanged in spirit, adding
  the trace path as an additive, independent channel.
- Run bloodhound inside each guest, filtered to the interactive operator's
  `auid`, and tail its NDJSON output live.
- Relay the raw `BehaviorEvent` NDJSON stream end-to-end (VM → GW → BFF → browser)
  and render it in a live log pane next to the terminal.
- Confirm End-to-End operation against local QEMU guests: typing in the right
  pane produces observable trace lines in the left pane within sub-second
  latency.

**Explicit non-objective:** this PoC does **not** reconstruct commands, correlate
events into a process tree, or reimplement the `bloodhound-tui` viewer logic. The
left pane is a live, line-formatted view of the raw event stream. Correlation and
tree rendering are deferred (see §18).

## 2. Relationship to `web-terminal`

This PoC is a superset of `poc/web-terminal`, not a fork of its intent. The
terminal half (target picker, PTY session, resize, concurrent sessions,
credential confinement) is carried over verbatim in design. The traced variant
adds exactly three things:

- A new, independent, read-only **trace channel** (`/ws/trace`) that is parallel
  to — and decoupled from — the interactive PTY channel (`/ws`).
- A new Runtime capability, `TraceStream(id)`, that yields the guest's live
  `BehaviorEvent` lines. The existing `Targets` / `EnsureStarted` / `Attach` /
  `Shutdown` contract is unchanged.
- bloodhound provisioned into and launched inside each guest.

Everything the original PoC validated remains in scope and keeps working; the
trace path is purely additive and may be absent (a guest without bloodhound
simply yields an empty trace stream) without breaking the terminal.

## 3. Architecture Overview

```
[ Browser ]  (left: live trace log | right: xterm.js)
    │
    │  /ws        WebSocket — binary = terminal I/O, text = JSON control
    │  /ws/trace  WebSocket — text = raw BehaviorEvent NDJSON lines (backend → browser only)
    ▼
[ BFF ] (Go)
    │   - Serves static frontend assets
    │   - Proxies the target list from the GW
    │   - Relays both WebSockets 1:1 (near-transparent; never interprets payloads)
    │
    │  /ws         relayed end-to-end (bidirectional)
    │  /ws/trace   relayed end-to-end (one-directional, backend → browser)
    ▼
[ GW ] (Go / Runtime owner)
    │   - Existing: launch fleet, target status, Attach(id) → interactive PTY session
    │   - New: TraceStream(id) → opens a read-only exec channel to the guest that
    │          tails bloodhound's NDJSON output and streams each line outward
    │   - Holds all credentials (never sent toward the browser)
    │
    │  Runtime abstraction (QEMU implementation → SSH today)
    ▼
[ Guest VM ]  Ubuntu, running:
    - sshd                              (interactive PTY, as before)
    - bloodhound --uid <operator-uid>   (eBPF tracer; NDJSON → /var/log/bloodhound.ndjson)
```

The two channels are independent. The trace channel can be opened as soon as a
target is `ready`, whether or not a PTY session is attached, and carries no
credentials and no control frames toward the backend.

## 4. Technology Stack

Inherited from `web-terminal`:

- **Frontend:** Vanilla JS; `xterm.js` + fit addon for the terminal pane.
- **BFF / GW:** Go (single module, two commands: `cmd/bff`, `cmd/gw`).
- **WebSocket:** `github.com/gorilla/websocket`.
- **SSH:** `golang.org/x/crypto/ssh` (PTY session and trace tail share the SSH
  transport, on separate channels).
- **Targets:** QEMU guests from an Ubuntu cloud image, cloud-init provisioned,
  reachable on host loopback via user-mode port forwards.

Added:

- **Tracer:** [bloodhound](https://github.com/abyss0-dev/bloodhound), built as a
  static musl binary, deployed into each guest and run under systemd. Consumed as
  a built artifact only — no source modification. The build must include bloodhound's
  runtime BTF offset resolution (abyss0-dev/bloodhound#37); earlier builds hardcode
  `task_struct` offsets to one kernel and silently drop every task-scoped event on
  any other kernel build (see §10).
- **Trace pane:** plain DOM scrolling log with a bounded ring-buffer for display,
  plus an optional **Web Worker** that persists the full stream to OPFS (§13).

## 5. Component Responsibilities

**BFF (browser-facing):**
- Serves the static frontend and proxies the target list (status, no credentials).
- Relays the `/ws` PTY channel bidirectionally and the `/ws/trace` channel
  outbound, both without interpreting payloads.

**GW (infrastructure-facing):**
- Owns the Runtime; launches the fleet on startup and tears it down on exit.
- Exposes target status (`booting` / `ready` / `error`).
- On `/ws` attach: bridges an interactive Session to the WebSocket (unchanged).
- On `/ws/trace`: calls `TraceStream(id)`, line-buffers the guest's output, and
  pushes each complete NDJSON line outward as a text frame; applies backpressure /
  drop policy when the consumer is slow (§11).
- Holds all credentials; none cross toward the BFF or browser.

**Runtime abstraction (inside the GW):** the `web-terminal` contract plus one
read-only method.

| Method | Responsibility |
|---|---|
| `Targets()` | Return configured targets with current status. |
| `EnsureStarted()` | Launch all targets; begin readiness checks. |
| `Attach(id)` | Open an interactive PTY session; return a `Session`. |
| `Shutdown()` | Stop every launched target and release resources. |
| `TraceStream(id)` | Open a read-only stream of the guest's live `BehaviorEvent` NDJSON lines. |

The QEMU implementation realizes `TraceStream` as a **dedicated SSH connection**
(independent of any PTY session, so the trace pane works without an attached
terminal) that runs `tail -F -n0` of bloodhound's output file **without
allocating a PTY**. It is returned to the GW as a bare `io.ReadCloser` — the
read-only analogue of the existing `Session` (`io.ReadWriteCloser`); closing it
tears down the SSH connection, which reaps the remote `tail`. The trace command is
run so that SSH session close delivers `SIGHUP` to its process group, rather than
leaving an idle `tail` (one with no new line to write) lingering until its next
blocked write trips `SIGPIPE` — otherwise repeated subscribe/unsubscribe cycles
would accumulate orphaned `tail` processes in the guest. Another runtime can
realize it differently (a log API, a side-car socket) while presenting the same
read-closable line-stream contract.

This trace connection authenticates as a **separate, non-operator account** whose
login uid differs from `operator_uid`, not as the interactive user. That is the
fix for the observer feedback loop described in §9: a `tail` running under the
operator's own `auid` would have its own reads traced and amplified without bound.

## 6. Wire Protocol

Two channels, each with a single framing convention so frames pass end-to-end
unmodified through the BFF.

**`/ws` (interactive PTY)** — unchanged from `web-terminal`:

| Frame type | Meaning | Direction |
|---|---|---|
| Binary | Raw terminal bytes | Both |
| Text | JSON control (`resize` with `cols` / `rows`) | Browser → backend |

**`/ws/trace` (observation)** — new, one-directional:

| Frame type | Meaning | Direction |
|---|---|---|
| Text | One raw `BehaviorEvent` NDJSON line (UTF-8) | Backend → browser |
| Text | JSON control envelope for stream meta (e.g. `{"type":"trace_meta","dropped":N}`) | Backend → browser |

The browser sends nothing on `/ws/trace` other than the close handshake. Event
lines are forwarded as emitted by bloodhound; the relay path treats them as
opaque text, and the browser parses them only for display. Because the browser is
otherwise silent on this channel, the GW and BFF still run a read loop on their
`/ws/trace` sockets purely to observe that close handshake (and to service
ping/pong); without it a browser-side close goes unnoticed and the watcher SSH
connection — and the remote `tail` behind it — leaks.

## 7. BehaviorEvent Format and Parsing

The trace stream is **NDJSON / JSONL**: one UTF-8 JSON object per line. The
canonical schema is `docs/spec/behavior_event/behavior_event.schema.json`. A
representative line:

```json
{"header":{"timestamp":6.879845928,"auid":1000,"sessionid":5,"pid":291,"ppid":229,"comm":"sshd"},
 "event":{"type":"TRACEPOINT","name":"openat"},
 "proc":{"main_executable":"/usr/sbin/sshd","cwd":"/"},
 "args":{"filename":"/etc/passwd","flags":["O_RDONLY","O_CLOEXEC"]},
 "return_code":3}
```

Anatomy the viewer relies on:

| Field | Meaning | Viewer use |
|---|---|---|
| `header.timestamp` | float seconds (see clock caveat below) | not used as a wall clock |
| `header.{pid, ppid?, comm}` | process identity | line label |
| `header.{auid, sessionid}` | correlation keys | (scoping done in-kernel) |
| `event.type` | `SYSCALL` / `TRACEPOINT` / `TTY` / `PACKET` / `LSM` / `LIFECYCLE` / `HEARTBEAT` / `META` | color + filter axis |
| `event.name` | hook or event name (may be a numeric syscall id) | primary label |
| `proc.{main_executable, cwd, tty}` | best-effort context | optional |
| `args` | type-specific, producer-defined | extract one or two representative keys |
| `return_code` | optional | rendered as `→ rc` |

Parsing rules and pitfalls (all confirmed against real captures):

- **Permissive parsing.** `args` is producer-defined with `additionalProperties`,
  and older producers emit an `event.layer` field that the canonical schema has
  since dropped. The viewer reads only the keys it needs and ignores the rest; it
  never validates incoming lines.
- **Two timestamp clocks.** Kernel-derived events (SYSCALL / TRACEPOINT / TTY /
  LIFECYCLE / LSM) carry a **boot-relative monotonic** `header.timestamp`, while
  HEARTBEAT carries **wall-clock epoch** seconds. The viewer therefore does **not**
  render `header.timestamp` as a clock; the displayed time is the browser's
  **receive time**, and the raw value is shown verbatim only as supplementary
  text. (This is the known wall-clock-unification gap; arrival-order display makes
  it irrelevant here.)
- **Numeric event names.** Tier-1 raw syscalls arrive as `event.name = "3"` with
  `args.syscall_nr` / `args.raw_args`; richly-extracted hooks arrive with decoded
  names (e.g. `openat`). Numeric names render as `syscall #N`.
- **Base64 payloads.** `args.data` on TTY and PACKET events is Base64. It is
  **not** decoded by default (TTY content would duplicate the right pane); the
  formatter shows a size hint and truncates.

### Command-grouped rendering (default display)

The default left-pane rendering presents the stream as a sequence of command
stories rather than a flat log: an `execve`/`execveat` event is rendered as a
prominent **command anchor** (`▶ argv`, with the process label and receive
time), and the events that follow are rendered as indented **child rows** under
it (a per-type icon, the event name, one representative `args` value, and a
`✓`/`✗` return-code badge), each tinted by a stable per-`pid` colour so a burst
from one process reads as one group. This grouping is **best-effort and
display-only** — it anchors on the most recent `execve` and the matching `pid`,
not a real correlator (true command-unit / process-tree correlation is deferred,
§18). It composes with the §11 filters: with TTY hidden and shell housekeeping
dropped by default, the pane reads as "each command the operator ran, and what it
did". The raw NDJSON line is retained (raw toggle, and persisted to OPFS, §13).

Underlying each row is a compact one-liner derived from `(type, name)` plus one
or two representative `args` keys, the return code, and the process label; it is
what the substring filter (§11) matches against and what the raw-off fallback
shows for unknown shapes:

```
12:03:01  TRACEPOINT  openat   /etc/passwd          → 3   (pid 291 sshd)
12:03:01  TTY         tty_write  "…" (b64 24B)             (pid 314 bash)
12:03:02  LIFECYCLE   process_start  /usr/bin/cat          (pid 402 cat)
```

The formatter is a thin per-type function (a small fixed set of type → key
mappings); unknown types fall back to `type name → rc`.

### HEARTBEAT handling

HEARTBEAT events arrive roughly once per second with `auid`/`pid` zeroed and
carry `args.{drop_count_delta, drop_count_total, events_emitted_delta}`. They are
**excluded from the main log stream** and instead drive a persistent status row:
a *tracer-alive* indicator, the cumulative emitted count, and a drop counter.
This keeps per-second noise out of the log while making tracer liveness and drops
continuously visible.

These arrive with `auid` zeroed even though bloodhound runs with
`--uid <operator-uid>`, so the design relies on bloodhound emitting HEARTBEAT
independent of the `auid` filter. Because bloodhound is consumed unmodified (§19),
this is confirmed at bringup (criterion 7) rather than assumed — both liveness
(§9) and the non-idle trace channel depend on it; if heartbeats were filtered out,
an idle operator would leave the channel completely silent and the GW keepalive
would become the sole liveness signal.

## 8. Data Flow Design

- **Target listing:** unchanged — browser → BFF → GW; non-`ready` targets are
  non-selectable.
- **Terminal session:** unchanged — selecting a target opens `/ws`, GW calls
  `Attach(id)`, keystrokes and output relay as binary frames, `resize` as text.
- **Trace subscription:** when a target is selected (or its trace pane opened),
  the browser opens `/ws/trace?id=<id>`. The BFF opens a corresponding
  `/ws/trace` to the GW. The GW calls `TraceStream(id)`; the QEMU runtime tails
  bloodhound's NDJSON from that guest.
- **Trace flow:** each complete NDJSON line read on the guest is pushed as a text
  frame, relayed by the BFF, and handed to the browser's trace worker (§13).
- **Scoping:** bloodhound is launched with `--uid <operator-uid>`, so its
  in-kernel `auid` filter restricts the stream to the interactive operator's own
  behavior; unrelated system activity does not appear. The filter keys on `auid`
  (login uid), which `sudo` does not change — so anything sharing the operator's
  login uid is in scope, including the trace `tail` itself if it logs in as the
  operator. The trace connection therefore uses a distinct login uid (§5, §9).
- **Teardown:** closing the tab or deselecting the target closes `/ws/trace`,
  which terminates the tail channel on the GW. The bloodhound daemon keeps
  running in the guest for the next subscriber.

## 9. Trace Pipeline

```
bloodhound (eBPF, in guest)
  │  BehaviorEvent NDJSON to stdout
  ▼
/var/log/bloodhound.ndjson      (systemd StandardOutput=append:)
  │  tail -F -n0  (follow new lines only, no PTY)
  ▼
GW dedicated SSH conn  ──▶  line-buffer  ──▶  /ws/trace frame  ──▶  BFF relay  ──▶  browser
```

- bloodhound runs as a resident daemon under systemd, started at guest boot,
  writing NDJSON to a known path. The GW does not start or stop bloodhound; it
  only tails its output.
- `tail -F -n0` follows the file from its current end, so a new subscriber sees
  events from subscription time forward rather than replaying the whole session.
- A guest without bloodhound (or with the daemon stopped) yields an empty stream;
  the terminal pane is unaffected.

**Line framing.** The SSH stream delivers arbitrary byte chunks; the GW
reassembles complete lines (split on `\n`) so that one frame carries exactly one
complete NDJSON line. NDJSON guarantees no raw newline inside a record (JSON
escapes them), so newline framing is sufficient — no JSON-aware boundary parsing
is needed. Two concrete cautions:

- **Oversized lines.** A `BehaviorEvent` with a full Base64 packet/TTY payload can
  exceed 64 KB, so the naive `bufio.Scanner` default token limit must not be
  relied on. The GW reads with `ReadBytes('\n')` (or a raised buffer) and applies
  an oversized-line policy (truncate or drop with a counter) consistent with the
  backpressure model (§11).
- **No PTY on the trace exec.** Allocating a PTY would inject `\r\n` translation
  and echo; the trace command runs as a plain (non-PTY) exec so lines terminate
  with a clean `\n`. (Because WebSocket preserves message boundaries, framing one
  line per text frame means the browser never has to re-split; only if the GW
  batches multiple lines per frame does the browser split on `\n` again.)

**Observer feedback loop.** The reader of the trace must not be a process that
bloodhound itself traces, or observation becomes self-sustaining. bloodhound
filters on `auid` (login uid), and `sudo` does not change it. If the GW's trace
connection logs in as the operator (`poc`, the same credentials the PTY session
uses), the remote `tail` runs under the operator's `auid` — so the `tail`'s own
`read`/`write` syscalls are traced, emitted as events, appended to the file,
which wakes the `tail` (via `inotify`), which reads again. The loop is
self-driving and amplifies: measured at roughly **12× the idle event rate** on a
guest where a plain operator session was otherwise quiet, with the trace pane
filling with the reader's own `openat`/`read` activity and the backpressure drop
policy (§11) tripping continuously. The ring-buffer drop bounds it (no unbounded
growth), but the real signal is buried.

The fix is to give the trace path an identity bloodhound does not trace: the
`TraceStream` SSH connection authenticates as a **dedicated watcher account**
whose login uid differs from `operator_uid` (§14), so the kernel `auid` filter
excludes the reader entirely. This also matches the production posture (§19),
where the observation surface is separate from the operator. Two alternatives
exist but are not the baseline here: bloodhound excluding reads of its own output
file, or exposing the stream over a side-channel (socket) that is not a traced
syscall path. Note that `--exclude-ports 22` (§11) does **not** help — it scopes
the packet layer, not the `tail`'s file-read syscalls.

**Connection longevity.** The trace connection is long-lived (it persists for the
duration of a browser subscription, which can be hours). The dial timeout applies
only to handshake establishment, not to the established connection, and `tail -F`
never self-terminates. The risks are idle reaping by QEMU user-mode networking
(SLIRP) or an intermediate NAT/firewall, and the fact that
`golang.org/x/crypto/ssh` sends no keepalive automatically — a dropped connection
could otherwise go undetected (half-open). Two mitigations:

- **GW-side SSH keepalive.** A periodic `keepalive@openssh.com` request (≈30 s)
  keeps NAT/SLIRP state warm and actively detects a dead connection; on error the
  GW closes the stream and surfaces it upward.
- **Browser auto-reconnect.** When `/ws/trace` closes, the browser reconnects with
  backoff; `tail -F -n0` resumes from the current file end, so the only loss is a
  brief reconnection window (acceptable for the PoC).

bloodhound's once-per-second HEARTBEAT also keeps the channel non-idle while the
tracer is alive (heartbeats still traverse SSH/WS even though they are excluded
from the display, §7), but this is treated as a convenient side effect, not the
primary liveness mechanism — the explicit keepalive stands on its own.

## 10. VM Provisioning

Inherited: one Ubuntu cloud image downloaded once as a read-only base; per-target
copy-on-write overlays; cloud-init sets distinct hostnames and a shared PoC
user/password; user-mode networking forwards host loopback ports to each guest's
sshd.

Added for tracing:

- The bloodhound static binary is placed into each guest (baked into the overlay
  or delivered via cloud-init) along with a systemd unit that runs
  `bloodhound --uid <operator-uid> --exclude-ports 22` with stdout appended to
  `/var/log/bloodhound.ndjson`. The unit sets `RUST_LOG=info`: bloodhound logs via
  `env_logger`, which defaults to `error` when `RUST_LOG` is unset and would mute
  both the resolved-offset line and the offset-resolution fallback warning that is
  the guardrail against silent breakage (below).
- Kernel requirements: kernel ≥ 6.8, BTF (`CONFIG_DEBUG_INFO_BTF`), and
  `CONFIG_AUDIT` for `loginuid` / `auid` tracking — all present on the Ubuntu 24.04
  cloud image (confirmed on `6.8.0-117-generic`). BPF LSM is the exception: it is
  compiled in but **not in the active LSM stack by default**, so bloodhound's seven
  LSM tamper-resistance hooks do not attach unless the kernel command line adds
  `lsm=...,bpf` (a grub change plus one reboot). Their attach is non-fatal — the
  SYSCALL / TRACEPOINT / TTY capture this PoC relies on is unaffected — so enabling
  BPF LSM is optional for the trace pane.
- bloodhound resolves the `task_struct` field offsets it needs at daemon start from
  the running kernel's BTF (manual CO-RE, abyss0-dev/bloodhound#37), so no
  per-kernel offset verification or matched-kernel pinning is required; it is
  portable across BTF-bearing kernels. Verification caveat: a build with stale
  hardcoded offsets does not error — it reads a garbage `auid`, drops every
  task-scoped event, and leaves only HEARTBEAT/PACKET flowing, so the daemon looks
  healthy while observing nothing. Confirm capture by the `Resolved task_struct
  offsets ...` log line and by actual `execve`/`openat` events, not by file growth.
  To turn this from a manual check into an automatic guard, the GW gates
  trace-readiness on it: over the watcher connection it confirms the guest journal
  shows the `Resolved task_struct offsets` line before presenting the trace pane as
  live, so the garbage-auid silent-failure mode surfaces as a non-ready target
  rather than a healthy-looking but blind stream.
- The interactive SSH login sets `loginuid` (via `pam_loginuid`), which is exactly
  the interactive-session case bloodhound's `auid` filter targets. The trace
  connection logs in as a separate watcher account so its `loginuid` falls outside
  that filter (§9).

## 11. Backpressure and Volume

A raw `BehaviorEvent` stream is high-volume (per-syscall). Even scoped to one
`auid`, a single command such as a recursive `find` can emit thousands of lines.
The pipeline degrades gracefully rather than stalling, at three points:

- **GW (producer side, the backstop):** if the WebSocket consumer is slower than
  the tail, the GW drops oldest queued lines rather than blocking, and surfaces a
  cumulative `dropped` count via a `trace_meta` control frame. This is the
  ultimate guard and remains in force regardless of any client-side persistence.
- **Browser display (ring buffer):** the visible log keeps only the most recent N
  lines (e.g. 2000) in the DOM; older lines leave the DOM. Auto-scroll follows the
  tail; a pause control freezes the view without dropping the connection (§13).
- **Filtering:** bloodhound's `auid` filter is the primary volume control;
  `--exclude-ports 22` keeps the operator's own SSH traffic out of the packet
  layer (bloodhound's only source-side levers are `--uid`, `--exclude-ports`,
  `--ring-buffer-size`, `--enable-rich-sendfile`, `--heartbeat-interval`; the
  high-frequency `futex`/`poll`/`clock_gettime` class is already excluded in the
  daemon). Because `auid` scope necessarily includes the operator's own
  interactive shell, the raw stream is dominated by shell housekeeping and
  keystroke echo, so the client adds display-only filters for readability: TTY
  events are hidden by default (keystroke echo / prompt redraw, and they
  duplicate the right pane, §7); a *hide-shell* toggle (on by default) drops
  housekeeping syscalls whose `comm` is a shell while always keeping
  `execve`/`execveat` so launched commands stay visible (mirroring
  `bloodhound-tui`'s tty_write exclusion); per-`event.type` toggles; and a
  substring filter over the formatted line (path / pid / comm). These never
  touch the durable capture (§13) — only what is shown.
- **Reader out of scope:** keeping the trace reader off the operator's `auid`
  (§9) removes the single largest volume source — the self-feedback loop — before
  backpressure ever sees it. This is a correctness property, not just a volume
  optimization; the drop policy is the backstop, not the cure.

Durable capture (§13) does not replace these guards: OPFS write throughput is
high but finite, so the GW drop policy is still the last line of defense.

## 12. Screen Layout

A horizontal split occupies the body below the existing header (which keeps the
target picker and status from `web-terminal`):

```
┌ header: Web Terminal   [vm1][vm2][vm3]                         status ┐
├──────────────────────────────────────┬──────────────────────────────┤
│ Trace (live)                          │ Terminal                      │
│ 12:03:01 TRACEPOINT openat /etc/pas…  │ xterm.js                      │
│ 12:03:01 LIFECYCLE  process_start …   │ (interactive PTY)             │
│ (one-line formatted, auto-scroll,     │                               │
│  ring-buffered to N lines)            │                               │
│ [filter…][hide shell][pause][clear]   │                               │
│ [raw][types▾][export]                 │                               │
│ ● tracer alive  emitted 15575  drops 0│                               │
└──────────────────────────────────────┴──────────────────────────────┘
```

- Left: live trace log. The default rendering is the one-line format of §7; a
  toggle reveals the raw NDJSON line. A persistent status row at the bottom
  reflects HEARTBEAT (tracer-alive, emitted, drops).
- Right: the existing `xterm.js` terminal, behaviorally identical to
  `web-terminal`.
- The split ratio is fixed (≈50/50) for the PoC.

## 13. Trace Pane: Ring Buffer and Durable Capture

The display and the durable record are separate concerns, split across the main
thread and a dedicated Web Worker so that a high-volume stream never blocks the
UI.

**Display (main thread):** a bounded ring buffer of the most recent N formatted
lines is rendered to the DOM. This is ephemeral and exists only to keep the DOM
bounded while showing the live tail.

**Durable capture (Web Worker, optional capability):** the full, unabridged
stream is persisted to **OPFS** (Origin Private File System). High-throughput
OPFS writes require `createSyncAccessHandle()`, which is available only inside a
Worker, so the Worker owns persistence:

```
/ws/trace  ──▶  [ Trace Worker ]
                  ├─ append to OPFS via sync access handle, batched
                  │     (flush every ~200 ms or ~64 KB, not per line)
                  └─ forward a throttled tail to the main thread
                         (coalesced to ≤ ~30 fps) for the ring-buffer DOM
                                  ▼
                          [ main thread ] render ring buffer
```

Opening `/ws/trace` inside the Worker keeps every line off the main thread; the
main thread receives only the downsampled tail it needs to render.

What durable capture enables: scroll-back beyond the ring buffer, end-of-session
**export** of the full `.ndjson`, and later offline replay / correlation (the
deferred tree feature, §18).

Constraints and policy:

- **Quota / size.** Per-syscall NDJSON can reach GiB scale over a long session.
  The Worker watches `navigator.storage.estimate()` and enforces a size cap with
  rotation (oldest bytes dropped) once a configured ceiling is reached; PoC
  sessions are otherwise expected to be short.
- **Single writer.** One sync access handle per file; the Worker mediates both
  writes and any scroll-back reads.
- **Optional layer.** Durable capture is additive. With it disabled the pane
  still works as a live, ring-buffered display — which alone satisfies the core
  verification (§14). Scroll-back rendering from OPFS (windowed reads) is itself
  deferred; the minimum durable feature is capture + export.

## 14. Configuration

The GW configuration extends the `web-terminal` config. The runtime and target
entries (`id`, `label`, endpoint, credentials) are unchanged; credentials remain
GW-only. Added per-runtime (or per-target) trace settings:

- `trace.enabled` — whether to expose `TraceStream` for the runtime.
- `trace.log_path` — the guest path bloodhound writes to (default
  `/var/log/bloodhound.ndjson`).
- `trace.operator_uid` — the uid bloodhound filters on; matches the PoC login
  user. This is the single source of truth for the operator uid: provisioning
  (§10) derives both the guest user's uid and the systemd unit's `--uid` argument
  from this one value, so the filter and the launched daemon cannot drift apart and
  silently empty the stream.
- `trace.watcher` — the credentials the `TraceStream` connection logs in with: a
  separate guest account (e.g. a `watcher` user) whose login uid differs from
  `trace.operator_uid`, so the trace reader is outside bloodhound's `auid` filter
  (§9). These are GW-only credentials, like the target credentials. Provisioning
  (§10) creates this account alongside the operator user.

The BFF is configured only with the GW address, as before.

## 15. Scope Definition

**In-Scope:**
- Everything `web-terminal` validated (two-process pipeline, Runtime + Session,
  multi-target selection, PTY fidelity, resize, concurrent sessions, credential
  confinement, clean fleet lifecycle).
- A second, read-only `/ws/trace` channel relayed end-to-end through the BFF.
- bloodhound running inside each guest, `auid`-scoped to the operator, tailed live
  by the GW.
- A live left-pane log: one-line formatted, ring-buffered, auto-scrolling, with a
  HEARTBEAT-driven status row and a drop counter.
- Optional OPFS durable capture with end-of-session export.

**Out-of-Scope:**
- **Command reconstruction, event correlation, and process-tree rendering** —
  deferred; the pane shows the stream only.
- **Reuse or modification of `bloodhound-tui`** — its batch viewer logic is not
  ported; bloodhound itself is consumed as a built artifact only.
- **Scroll-back rendering from OPFS** (windowed reads of older lines) — capture
  and export only for this PoC.
- **Persisting or auditing the trace stream on the GW** (live relay only).
- **Browser-side authentication / authorization** (inherited non-goal).
- **Non-QEMU Runtime implementations** of `TraceStream`.
- **Wall-clock timestamp normalization across event types** — irrelevant here, as
  lines are displayed in arrival order.
- **Guest-side rotation of `/var/log/bloodhound.ndjson`** — deferred *because this
  is a PoC*: sessions are short, so the file is bounded by session length and the
  append-only sink is accepted. A long-running deployment would add logrotate or a
  size cap (the browser-side OPFS capture already carries its own cap, §13).
- **More than one concurrent trace subscriber per target** — deferred *because this
  is a PoC*: one trace subscriber per target is assumed, so per-subscriber `tail`
  fan-out and its differing `-n0` join points are not designed for.
- **Exact reconciliation of exported line count against emitted events** — deferred
  *because this is a PoC*: the GW `dropped` counter accounts for backpressure drops
  only, and the brief reconnect-window loss (§9) is accepted, so the export (§13)
  reconciles approximately rather than exactly (see criterion 8).

## 16. Success Criteria

Each criterion is a concrete, observable check. Criteria 1–10 of the
`web-terminal` PoC continue to apply unchanged; the following are added.

1. **bloodhound is live and not blind.** After a target reaches `ready`,
   `/var/log/bloodhound.ndjson` exists, the journal shows
   `Resolved task_struct offsets ...` (not a fallback warning), and a command the
   operator runs produces matching `execve`/`openat` lines. Mere file growth is
   not sufficient — HEARTBEAT/PACKET grow even when offset drift has silenced all
   task-scoped capture (§10). The GW automates the journal-line portion of this as
   a trace-readiness gate (§10), so offset drift surfaces as a non-ready target
   rather than a healthy-looking blind stream.
2. **Trace channel is independent.** Opening `/ws/trace` for a `ready` target
   succeeds whether or not a PTY session (`/ws`) is attached, and closing one
   channel does not disturb the other.
3. **Live observation of operation.** A command typed in the right pane (e.g.
   `cat /etc/hostname`) produces corresponding `BehaviorEvent` lines (execve,
   openat, read, …) in the left pane within about one second.
4. **Operator-scoped stream.** The left pane shows the operator's own behavior;
   unrelated background-user activity does not appear (validates the `auid`
   filter end-to-end).
5. **Correct per-target routing of trace.** The trace pane for `vm1` reflects
   activity on `vm1`, not `vm3`, matching the terminal routing.
6. **Backpressure without stall.** A high-volume command (e.g. recursive
   `find /`) does not freeze the terminal or browser; the ring buffer caps DOM
   growth and the drop counter increments if lines are shed.
7. **HEARTBEAT status.** The status row shows the tracer alive with a rising
   emitted count; if drops occur the drop counter advances.
8. **Durable capture (when enabled).** After a session, the captured `.ndjson`
   exports and its line count is consistent with what was emitted minus the
   GW-reported drops and any brief reconnect-window loss. Exact reconciliation is
   out of scope for the PoC (§15).
9. **Credential confinement preserved.** No credentials appear on either
   WebSocket or in the BFF process; the trace channel carries only event lines
   and meta.
10. **Graceful absence.** A target with bloodhound stopped yields an empty trace
    pane while the terminal remains fully functional.
11. **No observer feedback.** Opening the trace pane and leaving the operator idle
    does not produce a self-sustaining stream: the pane stays quiet rather than
    filling with the `tail` reader's own `read`/`openat` activity, and the drop
    counter does not climb on its own. This validates that the trace connection
    runs outside bloodhound's `auid` filter (§9).

## 17. Repository Structure

```
poc/traced-web-terminal/
├── DESIGN.md
├── go.mod                  # single Go module (carried over from web-terminal)
├── cmd/
│   ├── gw/                 # Gateway: fleet lifecycle, attach (/ws), trace (/ws/trace)
│   └── bff/                # BFF: static serve, target-list proxy, /ws + /ws/trace relay
├── internal/
│   ├── runtime/            # Runtime + Session contract + TraceStream; QEMU/SSH impl
│   └── wire/               # framing constants for both channels
├── config.json             # GW runtime + targets + credentials + trace settings
├── web/
│   ├── index.html          # split layout: trace pane (left) + xterm.js (right)
│   └── trace-worker.js     # /ws/trace consumer: OPFS persist + throttled forward
├── vm/
│   ├── fetch-image.sh       # base image, overlays, cloud-init seeds, bloodhound deploy
│   └── cloud-init/          # per-target seed data + bloodhound systemd unit
└── README.md               # verification procedure
```

The starting point is a copy of `poc/web-terminal`; the additions are the
`TraceStream` method, the `/ws/trace` handlers, the split-layout frontend with the
trace worker, and the bloodhound provisioning in `vm/`.

## 18. Next Steps

- **Command-unit correlation:** group raw events into command windows and render
  an exec tree (the originally-considered scope), reimplemented as a streaming
  correlator rather than the batch `bloodhound-tui`.
- **Scroll-back from OPFS:** windowed reads of the persisted capture to scroll
  past the ring buffer.
- **Filtering / search in the trace pane:** display-only filtering by event type,
  shell-housekeeping, and substring (path / pid / comm) has landed (§11); a
  structured search UI (regex, field-scoped queries, jump-to-match) is the
  remaining refinement.
- **Alternate Runtimes:** realize `TraceStream` for a non-QEMU backend to confirm
  the contract holds.

## 19. Caveats

- **Observation visibility is a deliberate inversion of the production posture.**
  In the target abyss0 architecture the in-VM observation surface is hidden from
  the learner. This PoC intentionally exposes the raw trace for inspection, so it
  models an instructor / debugging / demonstration view, not a learner-facing
  screen.
- **bloodhound is consumed, never modified.** All changes live in this PoC and in
  provisioning; the tracer is a built artifact.
