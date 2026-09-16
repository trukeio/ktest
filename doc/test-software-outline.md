# Electronic test software — design outline

Status: draft, for discussion. Milestones 1 to 3 are built, and so are the
first two of milestone 4's three slices (§12); of §6–§13, only what §12 records
as built is committed. §5 is settled too: all three Logb changes are implemented and pushed
(`rveen/logb@ae8308d`), along with the viewer work that makes §5.1 visible
(`@b8cf4cc`), and that section now records what was decided rather than what was
proposed. The SocketCAN figures in §8, §10.5 and §12 are measured on `vcan`, not
estimated.

## 1. Purpose

A single program that turns a Linux machine plus a set of connected test equipment
into an instrument rack usable from a browser.

It covers three things that are today done by three unrelated tools:

- **Bus work** — CAN / CAN FD / LIN monitoring, decoding, transmit and replay,
  with ISO-TP and J1939 to follow (§8). The role Vector CANoe occupies.
- **Bench work** — programmable supplies, loads, DMMs, scopes, function generators
  over LXI and USBTMC.
- **Recording** — everything above on one timeline, in one open format.

The third item is the reason to build it. CANoe does not see the bench, and bench
software does not see the bus. Correlating a supply transient with the CAN traffic
it caused is currently done by hand, if at all.

## 2. What it is not

Stating this early because a lab tool attracts requests.

- Not a production end-of-line test system. No operator mode, no yield reporting,
  no shop-floor integration.
- Not an IDE, a report generator, or a requirements tool.
- Not a real-time system. Sub-millisecond deterministic response is out of scope
  for the daemon and is delegated to the kernel or to instrument hardware.
- Not an appliance distribution. It is a normal Linux daemon, not an OS image.

## 3. Architecture

```
  browser (any OS)
      |  REST (control)     HTTP chunked response (Logb stream)
      v                                  ^
  +---------------------------------------------------+
  |  ktestd (Go, Linux)                               |
  |                                                   |
  |  channel registry  --  scripting  --  logger      |
  |        |                                    |     |
  |  bus layer      instrument layer            v     |
  |  (SocketCAN)    (LXI, USBTMC)          .logb file |
  +---------------------------------------------------+
      |                    |
   CAN interfaces     bench instruments
```

**The daemon is `cmd/ktestd`**, and it is deliberately not the recorder. `canlogb`
stays what milestone 1 made it: a single-purpose CAN trace tool that records a bus
to a file and can serve that file's bytes live. It has no control plane, no leases
and no mode, so it cannot actuate anything and does not have to be trusted not to.
`ktestd` is the rack daemon, built on the same packages — `can/`, `record/`,
`stream/` — with the control plane, the leases and interlocks of §7, and the
live/replay mode above them. Keeping a small verifiable tool alive beside the larger
one is worth the second binary; merging them would mean the trace tool inherits
every reason the daemon needs guarding.

Four properties define the design:

1. **The daemon owns the hardware.** Connections stay open, sessions are leased,
   and a run survives the browser disconnecting.
2. **Everything produced is a Logb stream.** The file on disk and the bytes on the
   wire are the same format, and the transport supplies no framing of its own —
   Logb frames are self-delimiting and CRC'd. Live view and replay share one
   decoder.
3. **Control is asymmetric.** Commands go in over REST; results come back only on
   the stream. The widget is never the source of truth. This is also what removes
   the need for a bidirectional data transport; see §6.1.
4. **Timing lives below the daemon.** Cyclic and content-triggered transmit are
   configured into the kernel, not executed by the event loop.

## 4. Channels

One flat namespace, one naming convention, fixed before the first driver is
written. Renaming a channel later invalidates every panel and every recording
that references it.

```
can1.EngineSpeed          bus signal, decoded
psu1.ch1.v.set            setpoint requested by the operator
psu1.ch1.v.applied        setpoint acknowledged by the instrument
psu1.ch1.v.meas           readback
scope1.ch1                waveform stream
```

The three-way split on controlled quantities matters. Collapsing `set`, `applied`
and `meas` into one channel makes the recording lie the first time an instrument
clamps, refuses, or trips. Keeping them separate lets a recording answer "what did
we ask for, what was accepted, what happened" without inference.

Setpoint channels are **held**: a record only when the value changes, and the last
value written stands until the next one. Waveform channels are dense with an
implicit uniform axis. Bus channels are explicit-axis. All three are ordinary Logb
streams.

**On the word.** An earlier draft called these channels *sparse*, which collided
twice. Logb's viewer badged a §6.2 *guarded* field — one that may legitimately be
absent from a record — as "sparse", and both kinds of channel appear in the same
signal tree. *State* is taken too, by the viewer's categorical state bands. So:

| Concept | Word |
|---|---|
| A field may be absent from a record (Logb §6.2) | **guarded** — the spec's own term; the viewer's badge was renamed to match it in `logb@b8cf4cc` |
| A channel whose last value stands until the next record | **held** — the property; **log-on-change** is the writer policy that produces it |
| A categorical field drawn as a band | **state**, unchanged |

*Held* names the reader-side consequence rather than the writer-side cause, which is
the right thing to name: it is exactly why §5.1 is needed, and why a dense channel
needs nothing — there, the next sample is along shortly. It also survives the case
*sparse* does not, a held channel that changes every millisecond. Rejected: *step*
collides with `axis_step` and with "step a supply" (§9), *latched* reads as hardware
latching on a bench, *change-of-state* drags "state" back in.

## 5. Logb changes

Two findings from checking the current specification against this use case, plus
one that needed no spec change. Both concern scope acquisitions and held control
channels. Neither is a redesign, and both were checked against the implementation
rather than against the prose alone.

**All three are done.** The problem statements below stand as written — they are
why the changes exist — and each subsection ends with what was actually built.

### 5.1 Held channels must be restatable at a segment boundary

**Problem.** Design rule 3 says a reader handed the middle of a file resynchronises
and decodes with full schema. For a dense channel this is true and sufficient. For a
held channel it is true and useless: a reader joining mid-stream knows that
`psu1.ch1.v.set` exists and what its bit layout is, but not that it was moved to
12 V forty minutes ago. Schema is restated per segment; **value is not**.

This is not a corner case here. Every browser connecting to a live stream is a
mid-stream join, and the rack it renders is exactly the set of held channels.

**Proposal.** A **HOLD frame**, frame type `0x14`, emitted alongside the SCHEMA
frames after each SYNC frame, carrying the last known value of every held field in
the segment. Cost is proportional to channel count, paid once per segment, not per
record — the same trade Logb §6.1 already accepts for stream metadata.

Named HOLD rather than STATE because the viewer already means something else by
"state" (§4).

**It is a free extension.** `0x14` is unallocated — Logb's frame table runs `0x01`,
`0x10`–`0x13`, `0x20`, `0x30`, `0x40`, with `0x50` reserved for SIGN — and the spec
requires that unknown frame types be skipped by `payload_len`. So every reader that
exists today (`logbdump`, `logbview`, the core reader) skips a HOLD frame and
behaves exactly as it does now. Adding it breaks nothing and needs no version bump.

**Consequences if adopted.**

- A cut file behaves the way rule 3 claims it does, for every channel type.
- A browser join needs no "get current state" REST endpoint, which removes a second
  path to the same information that could disagree with the stream.
- The daemon can synthesise an opening segment on connect — sync frame, schemas,
  current values — so join latency is zero and the file's own segment cadence stays
  tuned for file reasons. `Writer.BeginSegment(wallTimeNs)` is already public and
  already restates every schema and run, so this costs no writer change beyond
  emitting the new frame.
- A live pane has no leading gap to draw. That matters more than it sounds: see
  §10.3, where expressing a gap is exactly what forces the fast path to box its
  data.

An earlier draft recorded a cost here that turns out not to exist — that the browser
stream stops being byte-identical to the file. `NewWriter` emits a file header, so a
stream to a client was always going to be a new file rather than a tail of the one
on disk. "Record what I am watching" was never `io.Copy`; the splice was already
there, and §5.1 does not cause it.

**Built.** Frame `0x14`, field flag bit 2, specified in Logb §6.10 and its BNF.
The payload is a presence bitmap plus one ordinary record, so a restated value
decodes by exactly the rules a written one does — a restatement that decoded
differently would eventually disagree with the records it restates, silently.

The API turned out better as writer *state* than as a call: `SetHold` records the
value and `BeginSegment` emits it into every segment thereafter. A frame whose
whole job is to be in every preamble is one a caller would forget. Readers take it
through an `OnHold` hook, never through `Next`, so a restatement can never reach a
plot as a sample. `logbdump` renders them.

The free-extension argument is now executable rather than asserted:
`TestHoldIsSkippedByAReaderThatDoesNotKnowIt` rewrites the frame-type byte to an
unallocated id and checks the file reads identically. The golden fixture did not
move, because bit 2 is clear in every schema that existed before.

Logb's viewer renders them, which is what turns the frame from correct into
useful: held fields draw stepped, and a pane opens from the value in force rather
than from the first sample inside the window. `Stream.HoldAt` takes the later of a
frame statistic and a restatement, so a complete file usually answers from its own
records and a cut file answers from the restatement — the case the frame exists
for. It is also what §10.3 leans on when it says the live path has no gaps to
express.

### 5.2 `axis_step` is schema-scoped and `axis_base` is frame-scoped

**Problem.** §5 places `axis_base` in the DATA frame and `axis_step` in the SCHEMA
frame. BNF.md confirms schema frames appear only at segment start:

```
<segment> ::= <sync-frame> <schema-frame>+ <run-frame>* <segment-body>*
```

So each acquisition can carry its own t0, but a change of sample interval cannot be
expressed within a segment. An operator changing the scope timebase mid-session
produces runs whose real `dt` no longer matches the stream's declared `axis_step`.

A naive writer will emit them anyway, and every reader will compute wrong axis
values and report nothing wrong. That is the silent-wrong-answer failure mode §5
already rejects for an unknown `axis_mode`, one level down.

**A new segment is necessary but not sufficient.** Logb §6.1 also says that *across*
segments the same `stream_uuid` MUST carry an identical schema — a schema change
means a new `stream_uuid` — and `axis_step` lives in the SCHEMA payload. So opening
a segment does not let the timebase change under the same stream: it forces a **new
stream identity**, and `scope1.ch1` becomes a different stream the moment an
operator turns the timebase knob. That collides head-on with §4, where a stable
channel name is what every panel and every recording references.

Three ways out:

1. **Accept it.** The channel name stays stable in the registry, one channel maps to
   several `stream_uuid`s over a session, and every consumer stitches them. No spec
   change; the work lands in the viewer, the exporter, and anything else that reads
   a recording.
2. **Carve out `axis_step`** as the one schema field permitted to differ across
   segments under one `stream_uuid`. Nearly free in a reader — schemas are restated
   per segment and re-read anyway — but "same uuid implies identical schema" stops
   being literally true, and concatenation and merging tools will have assumed it.
3. **Make `axis_step` run-scoped.** Rejected: it puts axis computation in two places,
   and `dt` is properly part of a stream's identity rather than a run's.

**Decided: (2).** Logb §6.1 now exempts `axis_step`, and only `axis_step`, from the
cross-segment identity rule, and §5.4 states normatively that a change of it MUST
open a new segment. A segment boundary is a SYNC frame plus a schema restatement;
it is cheap, and with 5.1 adopted it is cheap and complete.

The rule is enforced rather than merely written down: `WriteData` refuses a
mid-segment change with `ErrAxisStepChanged`, which turns the silent wrong answer
into a loud refusal at the write that would have caused it. The exemption's price
is stated in §6.1 where a reader will find it — a tool caching schemas by
`stream_uuid` must re-read the step per segment, and a merge tool must not assume
one interval per stream. (1) remains the fallback if that turns out to bite.

**The escape hatch that needs no spec change, and why it loses.**
`axis_mode = explicit` lets `dt` vary freely, per run or per frame, with nothing to
settle. It costs 4–8 bytes per sample — 4 MB on a one-megapoint acquisition — which
is precisely the zero-axis-bytes property §5.4 banks on. Segment-per-timebase is
right, and the identity question is now closed.

### 5.3 Not a spec change: the reader needs a bulk column path

`reader.go` currently exposes `Value(i, f int) (any, error)` and `Raw(i, f int)`.
Per record, per field, boxed. For a single-field, fixed-width, byte-aligned stream
— which is exactly what a waveform is — the correct access is a bounds check and a
`memcpy`, with an endian swap where needed:

```go
func (b *Batch) Column(f int, dst []int16) (int, error)
```

A one-megapoint acquisition through the boxed path costs a million interface
conversions. This blocks nothing, but it is what will make a scope widget feel slow,
and the SPICE transient mapping in §11 has the same shape, so it pays twice.

It is cheaper to add than it looks: `Batch.Data` is already the decompressed,
de-filtered, contiguous run of `Count` fixed records, and `Batch.Record(i)` already
hands back the byte slice. Purely additive — no format change, no new dependency.

Two things the signature above does not handle, both to be settled before it is
written:

- **Guarded fields have per-record absence.** Packing an absent sample as a zero
  reproduces exactly the bug guards exist to prevent, and it looks entirely
  plausible. `Column` either refuses a guarded field or returns a presence mask
  beside the values. It must not quietly do neither.
- **`dst []int16` is monomorphic.** Scope data is `i8`, `i16` or `f32` depending on
  the digitiser. Either make it generic over a numeric constraint, or have it refuse
  anything but an exact type match — never convert silently, which would put the
  cost straight back.

**Built, and both questions resolved by refusing.** `Column[T](b, f, dst)` is a
free function rather than a method, because Go methods cannot take type parameters.
Measured on a megapoint batch: **2.0 ms against 25.9 ms**, 1066 MB/s against 81.

Refusal turned out to be more informative than either alternative, because the
three ways a field can fail to be a column are three different mistakes:
`ErrColumnGuarded` for a guarded field, since a slice cannot say "absent" and a
zero there is the lie guards exist to prevent; `ErrColumnShape` for anything not
byte-aligned and byte-sized, such as a 12-bit CAN signal, which has no Go type at
all; `ErrColumnType` for a slice whose element type is not the field's type *and*
width — `[]float64` for an `f32` field included, because the silent widening would
restore exactly the per-element cost being removed.

### 5.4 What did not need changing

Recorded because it was investigated and the answer was "already covered":

- **A scope acquisition** is a stream with `axis_mode = 0` (implicit uniform), one
  record per sample, one `run_id` per acquisition. Zero axis bytes per record;
  trigger settings go in the RUN frame's parameter set; the axis restarts per run.
- **Storing a waveform as one opaque `bytes` blob is legal and wrong.** The blob has
  no sample type, no byte order, no conversion and no internal axis. That is the
  opaque-payload gap the format exists to close.
- **Acquisition atomicity** is `run_id`, which §6.5 already lists "a repeated test
  cycle" as a case for.

## 6. Control plane

**REST in.** Set setpoint, arm output, start and stop a run, acquire and release an
instrument lease, load a panel, list instruments. Ordinary request/response, plain
status codes, drivable from `curl` and therefore from any language the user already
has. This is what stops the scripting choice from being a captive decision.

**Logb out.** A POST returns accepted or an error. It does not return state. The
resulting `set`, `applied` and `meas` values arrive on the stream like everything
else.

Consequences of that discipline: two browsers stay in sync, a page refresh recovers
the rack, and replay drives the display through the identical path as live.

Practical costs, both handled at the widget: a dragged slider must render optimistically
and mark unconfirmed state visibly, and intermediate drag values must be rate-limited
or committed on release, or every recording fills with slider noise.

### 6.1 The stream is an HTTP response, not a WebSocket

A browser cannot open a raw socket — WebSocket, `fetch` + `ReadableStream`, SSE,
WebTransport, WebRTC, and nothing else — so a "fast direct socket" only ever applies
to consumers that are not browsers. But the choice is smaller than that anyway:
**Logb frames are self-delimiting, length-prefixed and CRC'd, so the transport
supplies no framing at all.** The stream is bytes. Which transport carries them is a
per-consumer detail rather than an architectural decision, and one producer feeds
all of them.

Control asymmetry then settles the browser case on its own terms. Commands go in
over REST and results come back only on the stream, so the data connection is
one-way, server to client. Bidirectionality is WebSocket's whole reason to exist,
and this design deletes the requirement. A one-way byte stream is an HTTP response
body.

```
GET /stream/live          endless chunked response, application/vnd.logb
GET /stream/replay/{id}   the same bytes, from a file
unix:///run/ktest.sock    the same bytes, for local high-rate consumers
```

What that buys over a WebSocket:

- **`curl http://rack/stream/live > run.logb` is a recording** — a valid, playable
  file, because the writer emits a header and `BeginSegment` restates the schemas.
  This is §6's argument applied to the data plane instead of only the control plane,
  and Python gets it from `requests(stream=True)`. A WebSocket cannot do it.
- **Backpressure propagates.** With `http.ResponseWriter` and `Flush`, a slow client
  stalls the write and TCP says so — which is the signal §10.5's per-class drop
  policy has to observe. Go WebSocket libraries generally sit behind an internal
  queue that hides the stall until it is a memory problem.
- **No dependency.** Logb's viewer server is `net/http` only, and the core library is
  near-zero-dependency by rule. WebSocket means gorilla or coder, or hand-rolling
  RFC 6455 framing behind `Hijack`.
- **Reconnect is correct by construction.** A new connection opens with header, SYNC,
  schemas and HOLD, so it resynchronises itself. Logb gives this, not the transport —
  it would work the same over a socket — but it removes the usual reason to want a
  transport that manages reconnection on your behalf.

Two things to verify rather than assume, and they are the real content of this
decision:

- **HTTP/2 flow control.** §7 requires TLS, and Go negotiates h2 over TLS by default.
  h2 has per-stream flow-control windows that will throttle a sustained binary stream
  unless tuned. Either tune it or pin the data endpoint to HTTP/1.1. This is the one
  place a WebSocket is genuinely simpler, riding its own connection.
- **Intermediary buffering.** A reverse proxy will buffer a chunked response into
  uselessness unless told not to. WebSocket has an analogous problem around
  `Upgrade`. Roughly a wash; both need deliberate configuration.

Rate is not what decides this. Pushing only new buckets rather than resending the
window (§10.3), a scrolling rack at 30 Hz is kilobytes per second, and decoded CAN FD
at full bus load is a few megabytes per second. Both transports do far more than that
locally. Measure it once milestone 1 runs, but do not expect it to decide anything.

## 7. Safety and modes

The browser is an actuator. This is not optional.

- Interlocks live in the daemon. Per-channel absolute limits, independent of what
  any panel permits.
- Explicit arm/disarm for outputs.
- Single-writer leases per instrument, with automatic expiry. Without expiry, the
  first crashed script blocks the lab.
- TLS and authentication before the first control widget works.
- Disconnect policy is per class. A bench supply holds its output; an active
  transmit or sweep does not.
- **Replay must not actuate.** Hardware bindings resolve only in live mode. The
  mode is set in the daemon and must be impossible to mistake in the UI. A rack that
  looks identical in both modes is correct only if the mode is unmissable.

## 8. Bus and instrument layer

**Linux only for the core.** Windows and macOS reach it through the browser. This
removes cgo almost entirely: SocketCAN is plain sockets via `golang.org/x/sys/unix`,
so the daemon is a static binary that cross-compiles.

Kernel facilities used deliberately:

- `CAN_BCM` for cyclic and content-triggered transmit, off the daemon's hot path.
  This is what makes residual bus simulation feasible without a real-time system,
  and it is measurably better than a userspace loop rather than merely tidier: on
  `vcan` at a 1 ms period, BCM cyclic transmit holds inter-frame jitter to p50 31 µs
  / p99 76 µs / max 103 µs, against p50 166 µs / p99 983 µs for a `time.Sleep` loop.
  Most of BCM's figure is steady lateness rather than scatter: it re-arms from each
  expiry, so every interval runs a few tens of µs long (§12, milestone 2).
  Sleeping to within 250 µs of the deadline and spinning the remainder costs a core
  and buys almost nothing (p99 869 µs). At 1 ms, userspace jitter is the same order
  as the period being reproduced.
- `SO_TIMESTAMPNS` for receive timestamps, with `SO_TIMESTAMPING` where the adapter
  offers a hardware clock. The two are not interchangeable — see below.
- `vcan` for development and testing with no hardware at all. This is cheaper and
  covers more than assumed; §12 says what it verifies and what it cannot.

**Interfaces are opened, not configured.** Setting bitrate requires `CAP_NET_ADMIN`,
which drags in root, capabilities or udev rules, all distro-specific. The operator
runs `ip link set can0 type can bitrate 500000 dbitrate 2000000 fd on`. The daemon
runs as a plain user.

**Vector VN hardware is out of scope.** SocketCAN adapters only.

**The bench's interface is an MCP2518FD on SPI**, decided for milestone 4
(§12) over the USB adapters: the lab's own MCP2518FD board on the SPI header of
an Orange Pi Zero 2 (Allwinner H616), under the mainline `mcp251xfd` driver —
no firmware and no out-of-tree code. Two properties decide it. The controller
stamps each frame from its own counter, clocked by the board's crystal, as the
frame is on the wire rather than when some firmware gets round to it. And it
reports a transmitted frame from its transmit event FIFO once the frame has
actually been on the bus, with that stamp, so `<bus>.tx.applied` means on the
bus, which is what §4's three accounts want. The daemon runs on the Zero 2 as a
static arm64 binary, with the bench instruments reached over the network as
before; the overlay and wiring are in `examples/orangepi-zero2-mcp2518fd/`. USB
adapters stay usable wherever a plain interface is enough: a DSD TECH SH-C31A
(`gs_usb`, CAN FD) is the bench bus's second node and the comparison, and the
Pico board of `doc/can-lin-pico-interface.md` remains a project of its own.

**ISO-TP and J1939 are further socket protocols, not further hardware.** Both are
in-kernel SocketCAN families reached exactly as `CAN_RAW` is — `socket(AF_CAN,
SOCK_DGRAM, CAN_ISOTP)` and the same with `CAN_J1939` — so they cost no cgo, no new
adapter and no change to the portability story above. `golang.org/x/sys/unix`
already carries what they need: both protocol numbers, `SockaddrCAN.RxID`/`TxID` for
ISO-TP addressing, and a `SockaddrCANJ1939` with `Name`, `PGN` and `Addr`. The
socket *option* constants are not exported and would be declared locally, as
`cmd/vcanprobe` already does for the CAN FD and BCM structures. Both live in
loadable modules — `can-isotp`, `can-j1939` — that a stripped kernel can lack, so
they join `CAN_BCM` in the capability probe rather than being assumed present.

**Their unit is a message, not a frame**, which is where they press on this design
instead of merely extending it. An ISO-TP message runs to 4095 bytes in its classic
form and a J1939 transport-protocol message to 1785, so the `Frame` vocabulary the
portability boundary warns about must not harden around 8 or 64 bytes. ISO-TP is
also request/response rather than observation, so it follows the transmit path in
milestone 2 rather than arriving with milestone 1. How the payloads are stored is a
real question and §13 keeps it open.

**LIN is in scope too, and is the awkward one.** Where ISO-TP and J1939 arrive free
with the kernel, LIN has no mainline support at all: it is not a SocketCAN protocol,
nothing in `drivers/net/can` implements it, and there is no module to load. The
usual answer is the out-of-tree `sllin` line discipline, which must be rebuilt
against every kernel the lab runs and is worth avoiding. Two routes avoid it, and
§13 keeps the choice between them open.

**Buy: the uCAN ULC.** An open-source LIN 2.1 dongle on an STM32F042 that enumerates
as a USB CDC virtual COM port, so `cdc-acm` carries it and there is no driver work at
all. It monitors, and it masters or simulates a slave with up to 15 scheduled frame
slots held in firmware. Its ceilings are what to check against real buses before
committing: 15 slots, 9600 and 19200 baud only, and an external 4.5–28 V supply for
the transceiver. The vendor also claims SLCAN compatibility with socketcan and parts
of can-utils — if that holds, `slcand` presents it as a CAN interface and the whole
existing path is reused unchanged, which is worth an hour with the hardware to find
out. Its `uCANLinTools` parses LDF and encodes frames in Python, so it is a reference
to check a Go implementation against even if none of its code is used.

**Or build: a Pico carrying both buses.** An RP2040 with the existing MCP2518FD
CAN-FD board and a LIN transceiver on the UART, presenting CAN through `gs_usb` —
mainline, the driver behind candleLight and CANable — for a SocketCAN interface with
no driver work at all, and LIN on a second USB endpoint as CDC ACM (`/dev/ttyACM0`)
behind a small userspace daemon. What this buys over a bare UART is that break, sync,
PID and checksum timing lives in the microcontroller rather than in termios, so the
daemon speaks a simple framed protocol instead of doing break detection against a
tty. `doc/can-lin-pico-interface.md` carries the design: transceiver candidates,
firmware split, and the phasing that keeps CAN working before LIN starts.

Two consequences survive both routes. **The schedule belongs in the device.** LIN is
master/slave and there is no `CAN_BCM` to hand a schedule table to, so a master
driven from userspace inherits the jitter measured above — p99 983 µs, unremarkable
against LIN slot times of 5–50 ms but not free. Both routes put the table in firmware
instead, which is the right place for it and one of the better arguments for either
over a raw UART. **The description format is the LDF.** `dbc/` does not parse it and
logb has no equivalent, so that is real new work whichever hardware wins, unlike
ISO-TP and J1939 which reuse extended ids and the existing DBC path.

**Timestamp quality is the open risk, and neither route fixes it for free.**
Both are CDC ACM, so both stamp on arrival in userspace after USB and tty
buffering unless the firmware both stamps on the wire and carries that stamp
alongside the frame in the protocol. The ULC's firmware either does this or it
does not, which is a question for the hardware and not for this document. On the
Pico it is a change we are able to make, but `doc/can-lin-pico-interface.md`
lists hardware timestamping among the optional features rather than the planned
ones, so building buys the ability to fix this and not the fix. What both routes
owe §11 regardless is the grade: a LIN channel stamped in userspace must be
recorded as such and never read like a hardware-stamped CAN one.

**LIN asks nothing of the format**, though. A LIN frame carries at most 8 bytes
under a 6-bit id, so it is a fixed-width `bytes` field exactly as a CAN payload is,
and the §13 question about ISO-TP and J1939 payloads does not arise for it.

**These numbers were measured, not assumed**, by `cmd/vcanprobe`, which is in this
repository and re-runnable — against a real adapter as readily as against `vcan`.
Re-run it before trusting any figure here on a different kernel, adapter or
distribution. On a 7.1 kernel with `vcan`, loopback latency from `write` to the
kernel receive timestamp is 0.7–1.7 µs; one raw socket
accepts roughly 700 000 frames/s where a 500 kbit/s bus saturates near 7 000, so the
virtual bus is never the bottleneck and supplies no back-pressure of its own; a
64-byte CAN FD payload round-trips with `CANFD_BRS` intact; and a frame written with
`CAN_ERR_FLAG` set is delivered to a socket holding a `CAN_RAW_ERR_FILTER`, so the
daemon's error handling is exercisable without provoking a real bus fault.

**A zero timestamp is not a timestamp.** The same probe, with `SO_TIMESTAMPING`
enabled in a process that had already opened and closed other timestamping sockets,
stamped 31, 0, 200 and 54 frames out of 200 across repeated runs — and 200 of 200 in
a fresh process. `SO_TIMESTAMPNS` was 200 of 200 every time.

The effect is intermittent and appears to be machine-wide rather than per-process:
whatever enables receive timestamping is shared and outlives the socket that asked
for it, so any recent user of it anywhere on the system hides the problem completely.
`vcanprobe` runs its naive pass first and from cold for that reason, and still does
not reproduce it on every run. That is the substance of the rule rather than a
caveat on it — a capability probe that samples once can come away satisfied and be
wrong later. So: an unstamped frame MUST be rejected or recorded as unstamped, never
written as t=0, and the check belongs on every frame rather than at startup. §11
records the timestamp source per stream; this is the case where that source lies by
omission rather than by being wrong.

**Raw sockets need an explicit `EINTR` retry.** Go's async preemption signal
interrupts a blocking `recvmsg` on a raw CAN socket. Without a retry loop frames are
lost silently: a first cut of the probe above dropped about 88 % of a 2 000-frame run,
and still took 17 interruptions once the retry was in place. This is a property of
every raw socket in the daemon, not an artefact of the test tooling.

**Capabilities are probed at runtime and written into the recording.** Hardware
timestamping depends on the adapter, not the distro; `CAN_BCM` can be absent from a
stripped kernel. A stream that states "software timestamps, BCM unavailable" is
honest everywhere without the daemon knowing what it is running on.

**Portability boundary.** One interface, two build-tagged files:

```go
type BusSource interface {
    Frames(ctx context.Context) <-chan Frame
    Caps() Capabilities
}

type BusSink interface {
    Send(Frame) error
    Cyclic(Frame, period time.Duration) (CyclicHandle, error)
}
```

`Cyclic` is where the kernel BCM lives; elsewhere the same interface is satisfied by
a ticker with worse jitter, and `Caps()` says which. The thing to guard is not the
syscalls but the vocabulary: if `Frame` is a thin cast over `can_frame`, portability
is gone invisibly. The instrument path — LXI over TCP, USBTMC over gousb — is
portable already and needs no work.

No Windows or macOS backend is to be written. The option costs a build tag and is
not exercised until someone asks.

## 9. Scripting

### 9.1 The split that makes this tractable

CAPL conflates two things, and that conflation is why it had to be a compiled
C-like language:

- **Reaction** — respond to a frame within microseconds. This never runs in a
  script. It is `CAN_BCM` configuration or it is Go.
- **Orchestration** — sweep a parameter, step a supply, run a sequence, react to a
  decoded signal at human or millisecond timescales. This is where scripting belongs
  and where nothing about it is hard.

Once separated, the language choice stops being load-bearing.

### 9.2 Recommendation

| Layer | Tool | Why |
|---|---|---|
| Limits and conditions | `expr-lang/expr` | Typed, sandboxed, pure Go, no control flow to abuse. `v_out > 4.75 && v_out < 5.25`. |
| Orchestration | Lua via `gopher-lua` | Pure Go, no cgo, single binary preserved. One `LState` per run maps onto a goroutine and bounds cleanly with `context`. Learnable in an afternoon. |
| Analysis-heavy work | External, over REST | numpy, scipy, pandas. Runs as the user's own process. |

The build order is Lua first and `expr` after (§12, milestone 5): the limits
`expr` would generalise are already enforced as a file of numbers the control
plane reads at start, and orchestration is the half a bench cannot be driven
without.

The third row is the important one. Because control is REST and results are a Logb
stream, an engineer can drive the rack from a Python script, a notebook, or a shell
loop without the daemon embedding anything. The embedded layer exists for logic that
must run close to the data and survive the client disconnecting — not as the primary
way to use the system.

### 9.3 Options considered and rejected

- **Starlark** — Python syntax and pure Go, but no `while` and no recursion in the
  default configuration. "Ramp until it fails" becomes `for _ in range(1000)` with a
  break, which is a trap in an exploratory context.
- **Embedded CPython** — cgo bindings are fragile and destroy the single-binary
  property. If real Python is needed it belongs out of process, which the REST API
  already provides.
- **A purpose-built DSL** — costs a year and produces something worse than Lua.
- **Yaegi** (interpreted Go) — attractive for reusing driver types, but asks
  engineers to learn Go, which was the original objection.

### 9.4 Rule

Scripts must not sit in the timing-critical path. Cyclic transmit, triggered
response and limit enforcement are daemon or kernel concerns. A script that stalls
must not be able to stall the bus.

## 10. Web UI

### 10.1 Model

Widgets bind to channel names, resolved against the schema in the stream. No DBC in
the panel and no database dependency — the recording names and scales its own
signals, so a panel built for live use replays unchanged.

**Keep the widget set generic.** One widget per instrument model is a path with no
end. A small typed set covers nearly everything:

numeric readout, plot, setpoint field, slider, knob, toggle, momentary button,
enum selector, state lamp, gauge, trace table.

An "instrument widget" is then a *composition* declared in configuration, not new
code. A supply panel is a layout over nine channels, not a class.

One rule holds this together, stated here because it is easy to lose while making
the UI prettier: **a widget owns its rendering surface, and the framework never
re-renders it on data.** Composition is declarative; the data path is not. §10.3 is
what happens when that is violated.

Which makes the panel definition data — and Logb already has `attach` frames.
Embedding the panel as an attachment makes a recording self-contained: schema, data,
and the rack that produced it. Someone opens the file in 2032 and gets the
instrument rack back, not just a signal tree. The facility already exists, so this
costs almost nothing.

### 10.2 Recommended libraries

The rule applied throughout: **do not write the plotting; do write the controls.**
Plotting at these data rates is a solved and difficult problem. Instrument controls
are easy and there is no library worth adopting.

| Concern | Recommendation | Notes |
|---|---|---|
| Components | **Lit** (web components) | Each widget is a custom element with a `channel` attribute. Framework-agnostic, works without a build step, and a panel definition maps directly onto custom elements — which is what makes panel-as-data natural. |
| Plotting | **uPlot** | Canvas, tiny, no dependencies, built for exactly this: many points, fast redraw, time-series first. The right default for both scalar channels and decimated waveforms. |
| Plotting, heavy | **Apache ECharts** | If uPlot's feature set runs short (multi-axis, richer interaction). Considerably larger. Only if needed. |
| Plotting, extreme | WebGL/WebGPU libraries exist, several commercial | Not needed if waveforms are decimated server-side, which is the plan. Revisit only if that assumption fails. |
| Rack layout | **gridstack.js** | Drag and resize panels into a rack. Serialises to JSON, which is the panel definition. |
| Parameter panels | **Tweakpane** | Good for dense parameter sets where a purpose-built widget is not worth it. Optional. |
| Knobs, lamps, gauges | Write them | A few hundred lines of SVG per widget. Every library in this space is either audio-oriented, abandoned, or brings a framework. |

Deliberately not used: any charting library that renders points as DOM elements
(D3, Highcharts, most SVG libraries). They are fine to tens of thousands of points
and unusable past that.

**Logb's viewer is Preact, not Lit**, so the plot UI is not reuse as it stands: nine
of its fourteen frontend files import Preact, and only `layout.ts`, `axis.ts`,
`api.ts` and `types.ts` — about 680 lines, including the pane-alignment work that was
expensive to get right — are framework-free. Sharing means extracting the uPlot layer
as framework-neutral and wrapping it, which is work rather than adoption. Lit is
still the right call, but for §10.1's reasons and not for speed: the framework is not
in the data path, so it cannot be what makes the rack fast or slow.

### 10.3 The live update path

Update rate is where a rack is judged, and the framework is not what decides it. In a
correctly built live plot the framework renders the *rack* — which panes exist, their
titles, the layout — and never touches the data. Socket to typed array to `setData`
on a retained plot instance. A framework in the per-frame data path has already lost,
in Lit exactly as much as in Preact.

Logb's viewer shows both halves of this. `api.ts` builds `Float64Array` views
directly onto the response buffer — genuinely zero-copy, and the server pads its
binary header to 16 bytes so the 8-byte alignment holds. The next few lines then
throw it away: `Array.from(a, …)` boxes every column into a `(number | null)[]`,
`x.map(…)` allocates again, `pad()` allocates again, and the whole thing hangs off an
effect keyed on the window, so every zoom and pan is a fresh fetch and a fresh set of
allocations. That is a sound design for pull-per-window replay and the wrong shape
for live.

The boxing is not gratuitous, which is the part worth knowing before repeating it:
**uPlot's draw path tests `yVal === null`**, and a typed-array element is never null.
A `Float64Array` cannot express a gap to stock uPlot.

The way out is that **the live path has no gaps to express.** A waveform channel is
dense by construction. A held channel is a step trace with no gaps either — once
§5.1 supplies the opening value, which is a second and independent argument for the
HOLD frame. Gaps in the current viewer come from empty decimation buckets over a
sparse file, which is a replay concern. So: boxed arrays on the historical path,
where correctness beats speed and there is one fetch per zoom; pure typed arrays on
the live path.

The rest is ordinary, and should be built in from the start because each item is
unpleasant to retrofit:

- **Preallocated ring buffers per trace**, sized to the visible window. Never
  allocate in the hot path; hand the plot the same array objects every frame and let
  it read the new tail.
- **Push new buckets, not the window.** A scrolling window at 30 Hz appends a handful
  of columns per frame, not three thousand. This is what makes §6.1's transport
  question uninteresting.
- **One `requestAnimationFrame` loop for the whole rack**, not one per pane. Coalesce
  whatever arrived since the last frame and drop intermediates. N panes cost one
  redraw pass, and every pane lands on the same frame — which makes the shared-axis
  property `layout.ts` already enforces exact rather than approximately exact.
- **Decode in a Web Worker**, transferring buffers with `postMessage(buf, [buf])`.
  This is the browser mirror of §5.3: the worker turns bytes into typed arrays and
  does nothing else.

### 10.4 Waveform decimation

The browser gets a min/max envelope per pixel column — roughly 3000 values per
trace instead of a million, preserving glitches. The disk gets full rate.

Implement this as **two Logb writers**, not as a special path in the widget layer:
one decimated stream to the browser, one raw stream to disk, both native Logb, both
readable by the same viewer. The decimated file is independently useful as a
lightweight artefact.

The reduction itself is already written: `viewer/decimate` does min/max envelopes,
run-lengths and event density, with a tested guarantee that the coarse tier's
envelope always contains the exact tier's — so a one-in-five-million spike survives a
whole-file overview. What is new here is running it on a live stream rather than on
an indexed file.

Default: decimated always, raw on request, explicit per channel. Continuous
full-rate waveform capture is the exception, not the norm, and should be a decision
someone made rather than a surprise disk fill during an overnight run.

### 10.5 Backpressure

Whatever the rate, something eventually fails to keep up. Policy is per channel
class:

- Bus frames MUST NOT be dropped.
- Waveform acquisitions MAY be dropped.
- **A dropped acquisition MUST appear in the recording as a drop.** A gap that does
  not say it is a gap is the worst failure this system can produce.

Cheap to build in now, unpleasant to retrofit.

The kernel helps with exactly half of this. With `SO_RXQ_OVFL` set, a socket whose
reader falls behind reports its losses exactly: in a deliberate overrun the counter
read 119 215 against 119 215 frames missing by payload sequence, counted
independently. But the counter rides on a delivered packet, so a reader that stalls
outright is told nothing — an overrun that lost 19 982 of 20 000 frames reported
`SO_RXQ_OVFL` 0, `dropped 0` from `ip -s link`, and a 100 % match ratio in
`/proc/net/can/stats`. Interior gaps are detected; a lost tail is invisible
everywhere the kernel is willing to look.

So the daemon MUST NOT read the absence of an overflow signal as evidence of no
loss. A run has to be closed by something that knows it ended — a frame count, a
heartbeat that stops arriving — and never by the receive path merely going quiet.

## 11. Provenance

The daemon knows things nobody remembers to record. Written automatically into the
recording's metadata at run start:

- Interface names, adapter types, serial numbers where available
- Kernel version, daemon version, driver versions
- Timestamp source and quality per stream (hardware vs software, and whether any
  frames arrived unstamped; see §8)
- Instrument identification strings and calibration dates where the instrument
  reports them
- Loaded panel definition and script versions

A measurement from 2027 should be reconstructable in 2030. This is where most lab
data quietly loses its value.

## 12. Milestones

Sequential. Each stands alone.

Rather more of this exists than the sections above imply, because Logb's viewer was
built against the same problems from the file end:

| Needed by | Already in Logb |
|---|---|
| §10.4 decimation | `viewer/decimate` — min/max envelopes, run-lengths, event density, with the two-tier containment property tested |
| §10.2 plotting | uPlot, plus the pane-alignment work in `layout.ts` |
| §11 provenance, panel-as-attachment | ATTACH and META already used exactly this way — `mdf2logb` embeds `source.dbc`, its sha256 and `dbc.database`, and a test re-decodes the file from its own attachment |
| milestone 1 DBC import | `dbc/` — multiplexing to guards, Motorola offsets checked against Vector's own algorithm over 465 600 cases |
| live view over a growing file | `viewer/index/grow.go` — re-scans from the last SYNC and merges the tail, assuming the file was mid-write |
| §5.1, §5.2, §5.3 | all three, as of `logb@ae8308d`: the HOLD frame, the `axis_step` segment rule, and `Column` |
| §10.1 held channels on screen | `logb@b8cf4cc` — stepped traces anchored at the value in force, `Stream.HoldAt`, and the `guarded`/`held` badges |
| §8 capability probing | `cmd/vcanprobe` — seven probes covering loopback, CAN FD, error frames, timestamp options, pacing, throughput and drop reporting, with `-json` in the shape `Caps()` wants |
| milestone 1 traffic generation | `can-utils` — `canplayer` replays a generated log byte-exact, `cangen` supplies sustained load. A test-time dependency, not a build one |

Milestone 4 added two more to that column. `viewer/decimate`'s min/max envelope
is what the scope's reduction has to agree with, and a test holds it to that,
bucket for bucket. And `Column`'s bulk path (§5.3) is what makes a waveform
affordable to read back at all.

What is genuinely absent is the daemon: no control plane and no streaming transport.
The write path and the as-written index are now built, and the second turned out to
rest on a gap in the core rather than on anything in the viewer — `logb.Writer` had
no way to say what it had just emitted. It now mirrors the reader's three hooks
(`logb@7b6d6c0`), and `viewer/index.Recorder` builds the index from them as the file
is written: offsets from `OnFrame`, identities from `OnSchema`, restatements from
`OnHold`, and the per-frame statistics from the records at `WriteData`, which is the
one moment in a file's life when they are uncompressed and in hand. On a 93 MB
recording of 9.16 million records, opening it costs 4.5 ms against 1.94 s to scan,
for a sidecar of 511 kB. The index a writer produces is tested for equality with the
one a scan produces, not for similarity, because anything less makes it a second
opinion rather than the same answer arrived at cheaply.

1. **SocketCAN in, Logb out, live view.** `vcan` first, replaying a `candump` log,
   so hardware is a late and boring integration step. DBC import is done (`dbc/`).
   logbview gains a streaming source (§6.1) and an index that grows as it writes.
   This is already a usable CAN trace tool, and it validates the format under
   sustained real load, which is what the milestone is really for.

   **Done**: `can/` for the socket, `record/` for the three streams a recording
   carries, `cmd/canlogb` to run it, the index written as the file is, `stream/`
   serving the same bytes live over HTTP, and `logbview -follow` keeping up with a
   recording still being written. Sustained load is no longer a claim: 4.25 million
   frames in twelve seconds against `cangen -g 0`, and the 4 077 825 the kernel then
   dropped are in the recording as drops rather than as silence.

   The live view costs what the tail costs, not what the file costs. As a recording
   grew from 8.6 MB to 113 MB and from 1.0 to 10.3 million records, each re-index
   stayed flat at 300–420 ms: only the first pass scans, and every one after it
   resumes from the last segment boundary and merges the new tail. The one case a
   growing file adds over an appended one is a partial frame, because a recorder
   flushes on a timer rather than on frame boundaries; that is a frame that has not
   finished arriving rather than damage, and it is now tested as such.

   §6.1's own test passes: `curl -N .../stream/live > run.logb` produces a file that
   the checker accepts against the candump log it was replaying. The transport needed
   no framing, as §6.1 predicted — `stream/` parses the producer's own byte stream to
   find frame boundaries and nothing in it knows what HTTP is.

   Verified with `canplayer` and a generated log, not with a purpose-built injector
   and not with hardware. A custom injector was considered and rejected: measured on
   `vcan`, `canplayer` replays 2000 frames byte-exact and in order with p50 149 µs /
   p99 976 µs of jitter against a 1 ms spacing, which is what a hand-written
   userspace injector also achieves (p50 166 / p99 983). Nothing is gained short of
   `CAN_BCM`, and BCM paces cyclic traffic rather than replaying a log's arbitrary
   timing, so for replay it does not apply.

   **The candump log is the ground truth, and it is a better one than a second Logb
   file** — the tempting design, where an injector records what it sent in Logb and
   verification compares two Logb files, is weaker rather than stronger: both sides
   are written and read by the same library, so a fault in the writer or the reader
   cancels out and the comparison still passes. A text log is foreign to the format,
   which is exactly what makes it able to falsify it.

   Signal generation needs no injector either. A ramp, a channel held for minutes, a
   burst sized to overrun the receive queue — all of it is offline text generation,
   replayed by `canplayer -l i`; `cangen -g 0` covers sustained load. So the harness
   is a generator script, `canplayer`, the daemon, and a checker that asserts the
   `.logb` against the log it replayed. The generator and checker are small and are
   not tools to maintain. The DBC signal encoder is not avoided work, only work in
   its right place: the daemon needs it for transmit in milestone 2, so it belongs in
   `dbc/` then rather than pulled forward to serve a test harness.

   Two things `canplayer` cannot do. Error frames, because the log format cannot
   express `CAN_ERR_FLAG` distinctly — but that is `cansend`-scale code and
   `cmd/vcanprobe` already does it. And reactive traffic, because replay is
   open-loop: **traffic that has to answer rather than repeat is the trigger to
   revisit this**, and it is a daemon feature (§8's content-triggered transmit) from
   milestone 2, not a test tool.

   What `vcan` cannot verify at all, and what therefore waits for an adapter: bus
   load, arbitration loss, and the conditions that produce bus-off — a synthetic
   error frame exercises the handler, not its cause — hardware timestamping, and
   timing fidelity below roughly 100 µs.
2. **Transmit.** Interactive send, then cyclic via `CAN_BCM`. This is where
   `cmd/ktestd` starts, because it is where a program first needs a control plane;
   `canlogb` records and does not actuate, and stays that way. Leases and interlocks
   arrive here for the same reason, and with them §7's TLS — which then forces the
   HTTP/2 decision §6.1 left open, since Go negotiates h2 over TLS by default.

   **Begun**: `cmd/ktestd` owns one bus, records it exactly as `canlogb` does, and
   takes single sends, classic or FD, as `POST /bus/{bus}/send`. Until §7's TLS
   exists the only control listener is a Unix socket, created 0660, so its owner
   and group are the access control; `-http` serves the recording read-only and
   nothing else. A send is answered with a sequence number or a refusal, never
   with state (§6).

   What a send did is recorded as §4's three accounts, and the probe came before
   the design. A monitoring socket sees every frame sent from the host flagged
   `MSG_DONTROUTE` — the daemon's, `CAN_BCM`'s and `cansend`'s alike — so `local`,
   now a bit in `<bus>.raw`, says where a frame came from and never who sent it.
   Attribution comes from the transmit socket instead, which sets
   `CAN_RAW_RECV_OWN_MSGS` and gets exactly its own frames back, marked
   `MSG_CONFIRM`. So `<bus>.tx.set` is each request, on the daemon's clock, refused
   ones included with the kernel's errno; `<bus>.tx.applied` is each echo, on the
   kernel's; and `<bus>.raw` is the bus. One clock per stream, so neither transmit
   stream is ever out of order.

   Merging three clocks in one recording exposed a latent fault. The raw and drop
   streams' axis field was unsigned, so a record stamped just before the segment it
   landed in would have wrapped to an enormous offset. It is signed now, as
   `dbc.Schema` already declared its decoded streams' axis; `axis_base` is per DATA
   frame and signed, so the format needed no change.

   Verified on `vcan` as milestone 1 was, against references foreign to the format:
   300 sends posted with `curl` from a generated log, `candump -L` recording the bus
   independently, and `logbcheck -tx` asserting the requests against the log they
   were posted from and each echo against its request — once each, as itself, and
   never before it was asked for. It holds with `canplayer` replaying the identical
   frames from another process at the same time: 2 300 frames on the bus and still
   exactly 300 echoes. A kernel refusal — an FD frame on a classic-MTU interface,
   `EMSGSIZE` — is answered 503 with its number and recorded all the same. (`vcan`
   on this kernel comes up with CAN XL's MTU of 2060, so FD needs no `ip link set`.)

   **Cyclic transmit** followed, through `CAN_BCM`. `PUT /bus/{bus}/cyclic/{id}`
   starts or changes a task and `DELETE` stops it; the id is the frame's identifier
   as candump writes it, which is also the end of the task's stream name. Measured
   first, on `vcan`: a task lives exactly as long as its socket — closing it, or
   `SIGKILL` on the owning process, stopped a 10 ms task at once — so a daemon that
   dies takes its cyclic traffic with it, which is §7's disconnect policy for an
   active transmit, supplied by the kernel. An update without `SETTIMER` changes a
   running task's data and keeps its timer, so a data change is seamless. A counted
   run reports `TX_EXPIRED` once its last frame is out (`TX_COUNTEVT` is `0x04`), and
   `TX_READ` returns the kernel's own account of a task.

   Each task has its own stream, `<bus>.cyclic.<id>`, and its fields are held. Each
   change is written with the request beside the kernel's `TX_READ`, and every
   segment restates the task, anchored at the moment it last changed, so a consumer
   joining mid-run sees what is transmitting now — §5.1's case exactly. One stream
   per task because a HOLD frame restates one record per stream. The kernel's
   account is guarded rather than zeroed when it holds no such task, and an expiry
   is believed only when `TX_READ` agrees the run is spent, since one queued by a
   task's previous run can arrive after a request has restarted it.

   Two things the checker had to learn, both measured and neither a fault in the
   recording. **`CAN_BCM` keeps no timetable**: it re-arms from each expiry, and every
   interval ran 22–30 µs long on average whatever the period — 5, 10, 20 or 50 ms —
   and none was ever short. Over seconds a task's count falls measurably short of its
   length over its period while no frame is missing, so `logbcheck -cyclic` asserts
   each interval rather than the count. **Kernel receive stamps are not monotonic in
   delivery order** when two senders on the host race on different CPUs: one frame
   in about a thousand arrived stamped 2.9 µs before the frame ahead of it, and
   candump saw exactly the same reversal, so a backwards step fails the check only
   when the reference lacks it.

   Verified as before, against candump: a classic task, an FD one with BRS, an
   extended one and a counted one; a seamless data update, a period change, a rerun
   after expiry, deletes, and a task left running into shutdown, recorded as ended.
   `logbcheck -cyclic` holds each change's request against the kernel's account, and
   every frame against the task's running time, its data in force and its rhythm.
   Controls with a stray frame after a stop, a duplicate from another process during
   a run, an FD frame rewritten as classic, and the stamp reversal smoothed out of the
   reference all fail it.

   **Leases and interlocks** came next, and with them the rule that nothing is sent
   by default. A client takes the bus's lease — a holder name, a token and a term, 30
   s unless it asks otherwise — presents the token as a `Ktest-Lease` header on every
   request, and renews before the term runs out. It then arms the bus, and only an
   armed bus takes sends and cyclic tasks. Releasing the lease, or letting it lapse,
   disarms the bus and deletes every task: §7's rule that an active transmit does not
   outlive its client, and the answer to the crashed script that would otherwise
   leave cyclic traffic running. Anyone who can reach the socket may disarm, lease or
   no, because stopping is always safe and an emergency stop that needed the lease
   would be out of reach exactly when its holder has hung. Until authentication
   exists the lease keeps two clients from colliding, not anybody out.

   What even the lease holder may do is bounded by limits read once, at start, from
   a JSON file — a misspelt key refuses to start rather than falling back to a
   default nobody chose — or built in when there is none. The first limit is a floor
   on cyclic periods, 1 ms by default. The limits in force go into the recording as
   META and the file as an attachment, and every decision the control plane takes,
   refusals included, is recorded in `<bus>.control` with the state it leaves; its
   held fields say at every segment whether the bus is armed and who holds it. A
   refusal is answered with its sequence number too, since it is recorded under it.

   The holder's name exposed a gap in the format, now closed in Logb's spec: a fixed
   `string` field is zero-padded, and a reader returns the value up to the first zero
   byte. Without the rule the padding was part of the value, and a holder read back
   as its name followed by NULs — as, very likely, every fixed string imported from
   MDF did.

   `logbcheck -control` holds the stream to the limits the recording declares:
   nothing accepted unarmed or without the lease, nothing below the floor, no refusal
   its state does not justify, every disarm stopping every running task under its own
   number within 50 ms, and every lapsed lease expiring within 0.25 s of its term. A
   daemon that keeps to its limits cannot be made to produce the recording that
   should fail this, so its controls are synthetic records in a unit test, one per
   rule. Verified live against candump through a scenario that walks every refusal,
   an emergency stop, a lapsed lease, a release and a shutdown, and with a limits file
   whose 2 ms floor refused a 1 ms task.

   **TLS and authentication** followed. `-https` serves the same control plane and
   streams over TLS, and admits a request only with a named token — `Authorization:
   Bearer` — checked against a file of token hashes. `-new-token name:role` makes
   one, printing the token for the client and, separately, the line that admits it,
   so the file never holds a token. A name has a role: view may read the streams and
   disarm, operate may also lease, arm and transmit. Over TLS a lease belongs to the
   name that took it, and its holder is that name rather than one the client
   chooses, so a lease token copied out of one client does not let another drive
   the bus. Every control decision now records who asked and through which
   listener, and the recording lists the names and roles — never the hashes — with
   the certificate's fingerprint. The Unix socket stays the local route, trusted as
   its file permissions are, and plain `-http` stays as it was, read-only and
   unauthenticated. Without certificate files the daemon makes a self-signed one
   that is its own authority, so that `curl --cacert` trusts it rather than skipping
   verification, keeps it across restarts, and prints a pin for `--pinnedpubkey`.

   The HTTP/2 question of §6.1 and §13 is settled, by measurement: h2 stays on. With
   `cangen -g 0` flooding `vcan`, an HTTP/2 consumer, an HTTP/1.1 one over TLS and a
   plain one each received the entire 51.6 MB stream at the 6.45 MB/s the recorder
   produced, and the Hub disconnected none. That is the producer's ceiling rather
   than the transport's, and the clients were curl; a client with a small h2 receive
   window is the one to measure if a consumer is ever seen falling behind.

   The same flood found a cost in the transmit socket's design. It receives the
   whole bus to find its own echoes in it, and at `vcan`'s full rate — some 530 000
   frames/s, seventy times a saturated 500 kbit/s bus — its queue overflowed and lost
   4 260 frames. None were echoes, since nothing was sent, but under that load an
   echo could be, and `tx.applied` would then show a request unconfirmed. The kernel
   reports the loss and ktestd prints it; a real bus is far from the rate that
   causes it, and a larger receive buffer is the first remedy if one approaches it.

   Verified live: no token and a forged one refused with 401; a view name refused the
   lease, and recorded as refused, yet able to read the stream and disarm; a holder
   named by the client refused over TLS; another operate name refused the use of the
   lease's token, and the local socket allowed it; the printed pin accepted and a
   wrong one refused; and `logbcheck -tx -cyclic -control`, now also checking names
   and roles against the list the recording carries, passing. Unit tests cover the
   tokens file, the self-signed certificate's persistence and names, and a planted
   fault for each new rule.

   **Sends by signal** close the milestone. With `-dbc`, a send or a cyclic task can
   name a message and its signals in physical units —
   `{"message":"EngineData","signals":{"EngineSpeed":3000,"Gear":"Reverse"}}` — and
   the frame is encoded with the database the recording embeds and decodes with. The
   encoder is `Message.Encode` in Logb's `dbc/`, written as decoding run backwards:
   each signal goes in at its `BitOffset`, in its field's byte order, so the bits
   land where a reader of the recorded frame looks for them. A signal left out takes
   the database's `GenSigStartValue` — the one attribute the parser now reads — and
   one the database gives none must be named; a value is rounded to the nearest step
   the signal represents; and a value outside the range the database declares, a
   name its value table lacks, or a signal the chosen multiplexor does not carry is
   refused. A cyclic task sent by message must bear that message's identifier as its
   name.

   The encoder is checked from both sides. Against Logb's own decoder, through a real
   writer and reader, at every layout a classic frame allows — 4 032 of them, Intel
   and Motorola, every start bit and width, signed and unsigned. And against cantools,
   which shares no code with it: 1 880 frames over 603 messages — random layouts and
   scalings, values between steps though never exactly halfway, where Python's
   rounding and Go's part company, multiplexing, a value table, and IEEE floats in
   both byte orders — byte for byte identical. Live on `vcan`, cantools decoded every
   frame candump saw back to the values asked for, and Logb's decoded streams showed
   the same. `logbcheck` had to learn one thing: the same message can now be sent
   once and every period, so it sets the daemon's single sends aside before it
   attributes frames to a cyclic task. Each is in `tx.applied` with the kernel's
   stamp, identical to the nanosecond with the monitor's copy of the frame.

   **Milestone 2 is done.** `ktestd` owns a bus, records it, and transmits on it —
   single frames and cyclic tasks paced by the kernel, by byte or by signal — under a
   lease, an arm, limits no client can loosen and, over the network, TLS and named
   roles. Every decision it takes is in the recording, beside the frames it caused.
3. **Panels.** Generic widget set, gridstack layout, panel-as-attachment. First
   bench instrument driver (an LXI supply) to prove the channel model spans bus and
   bench.

   **Done**, in three slices chosen so that nothing actuates before the rack that
   shows it is proven: a read-only rack first, then the supply with its limits and
   setpoint widgets, then panels edited in the browser.

   The read-only rack is built. `ktestd -https` serves it; it asks for a token and
   then reads `/stream/rack` — the same records as the recording, written by a
   second writer uncompressed, so that the browser decodes Logb itself (§3, §10.3)
   without carrying zstd, and so that §10.4's decimation has a place to go. The
   decoder is `web/src/logb/reader.ts`, a Logb reader in TypeScript running in a Web
   Worker. It takes bytes in whatever pieces they arrive and hands the page
   Float64Arrays every 50 ms; the page keeps them in mirrored rings that give a plot
   its window as one slice with nothing copied, and draws the whole rack from one
   animation-frame loop. Widgets own their surfaces: Lit draws each one's frame
   once, and data reaches it only through a draw call. A panel — widgets on a grid,
   bound to channels by name — is given with `-panel`, served to the browser, and
   embedded in the recording beside the limits (§10.1); without one the rack lists
   every channel.

   The reader is held to Logb's Go reader, not to the prose. A Go test writes a
   recording shaped like ktestd's and one that exercises the specification — both
   float widths and byte orders, unaligned fields, all seven conversions, complex
   values, guards, fixed and variable strings, the transpose filter, frequency and
   log axes, held values — reads both with each reader, and requires the same
   values, fed whole, a byte at a time, seven at a time and at random: 716 records
   and restatements agree, and a zstd frame is refused rather than misread. On its
   first run it disagreed about one field, and the Go reader was the one at fault:
   an interpolating table conversion came back from the wire as a stepped one, so
   every file using one had been read wrong while Logb's own test of the
   conversion, which never went through a file, passed. Fixed in Logb, with a
   round-trip test that fails without the fix.

   Checked in Chrome against a live bench: a wrong token refused; readouts matching
   what a driver was sending, a held string among them; plots drawing at the tasks'
   rates — 50 samples a second from a 20 ms task, 10 from a 100 ms one — one of them
   merging channels from two messages onto one axis, each carried forward, so the
   arrays have no gaps to express (§10.3); and the recording, written beside the
   rack's stream, still passing `logbcheck`.

   One naming question is open. §4 writes a bus signal as `can1.EngineSpeed`, and
   the recording names a decoded stream by its message alone, so the rack's channel
   is `EngineData.EngineSpeed`. With one bus the two say the same thing; with two,
   message names can collide, and the bus belongs in the stream name before the
   first multi-bus panel is written.

   The supply is built, against a simulator: `psusim` speaks SCPI over TCP in the
   Keysight dialect, clamps a setpoint beyond a channel's rating and says so only
   in its error queue, as a real supply does, and regulates into a resistive load,
   so that what it measures comes from the load and not from its setpoints.
   `ktestd -psu` drives one from a goroutine that owns the connection, so a slow or
   silent instrument holds up nothing. Each supply is an instrument of its own in
   the control plane — its own lease, its own arming, its own control stream — and
   the limits file bounds each channel's setpoints; a limit left out is recorded as
   "none", never omitted. The three accounts of §4 are three things in the
   recording: a channel's stream holds what was asked for beside what the supply
   reports holding after it, with whatever its error queue said; its measurements
   are a stream of their own. A setpoint nobody has set since the daemon started is
   absent, by a guard, not zero. The stop, `POST /disarm`, disarms every instrument
   under one number and switches every supply output off; a lease that lapses only
   disarms, leaving outputs as they were.

   The rack writes through setpoint and toggle widgets, which show the applied
   account beside the set one and mark where they differ, and lamps; a control bar
   takes each instrument's lease, arms it, and carries the stop. A widget unlocks
   only when this page holds the lease and the recording says the instrument is
   armed.

   The supply's reference is its own transcript of what it was told, which
   nothing that writes Logb wrote. `logbcheck -psu` requires the writes the
   recording says were carried out to be the transcript's, in order, channel and
   value, and `-control` now checks every instrument's stream: no write above its
   stated limit, no refusal the limit does not justify, every output switched off
   by a stop, nothing written that the control plane did not accept, and a stop
   present in every stream. Each rule is also planted as a fault and must be named.
   Checked in Chrome: locked until taken and armed; 12 V asked of a 6 V channel shown
   as 6 V held; 20 V refused against a 12 V limit; the output on, 5 V and 0.5 A
   measured, then 2 V and 0.2 A once the current limit bites; a reloaded page given
   its lease back; the stop switching the output off — and the recording passing
   both checks, 7 of 7 writes matching the transcript.

   The panel is edited in the browser. The rack is a gridstack grid whether it
   is being edited or not, so the rack edited is the rack seen, and its widgets
   go on drawing while they are dragged, resized, added and bound. A save is
   `PUT /panel`, held to the rules the file is held to at start — which gained
   one, that widgets lie inside the grid and clear of each other, since
   gridstack would otherwise move them somewhere the file does not say — then
   written back to the `-panel` file by a rename and attached to the recording
   again as `panel.json`. Attaching under the same name is the whole of the
   history mechanism: a reader keeps the last attachment of a name, and
   `stream.Hub` replays every attachment in order, so a recording ends with the
   panel in force at its end and a late joiner's preamble with the one in force
   now. Which was in force when is `rack.panel`, a held stream carrying each
   save and each refusal with the revision, sha256 and saver in force after it.

   A save names the panel it was edited from by that sha256, as `If-Match`, so
   of two people editing at once the second is refused with 412 and told why,
   rather than overwriting the first unseen; a view name is refused with 403;
   both refusals are recorded. The answer is a number, never the panel (§6):
   every page, the saving one included, takes the new panel from the stream,
   and the saving page leaves editing when the recording carries the bytes it
   sent. The file a save writes has one widget to a line, as a person would
   write it. `logbcheck -control` holds `rack.panel` to what a save may do: the
   stream opens with the panel the daemon started with, each save makes the
   next revision and a refusal none, only operate names save over TLS, and the
   `panel.json` the recording ends with has the sha256 its last record names —
   only the last, since the Go reader keeps one attachment per name. Each rule
   is planted as a fault and must be named.

   Checked in headless Chrome, driven over the DevTools protocol, against the
   bench: the rack laid out at the file's positions; a lamp dragged into free
   columns and a readout added, labelled and resized by its corner; the save
   ending the edit on the saving page and changing a second page; two pages
   editing at once, the second warned as soon as the first saved and refused
   when it tried; a view name refused. `curl` walked a save without `If-Match`
   (428), overlapping widgets (400), the view role (403), a stale tag (412), a
   save, and a save over the local socket; the `-panel` file's sha256 matched
   the new ETag each time. The recording passed `logbcheck -control`, twelve
   decisions, ten of them panel records.

   **Milestone 3 is done.** The rack is served by the daemon it drives: a bus
   and a bench supply on one timeline, read by a Logb decoder in the browser,
   written through widgets that show what was asked beside what was held, and
   laid out by a panel that is edited where it is used and recorded where it
   was used. The naming question above is still open, and is the first thing a
   second bus will need.
4. **Scope and waveforms.** Unblocked: §5.1, §5.2 and §5.3 are all in Logb. What
   is left here is the acquisition path and the widget, not the format.

   **Slices 1 and 2 are built; slice 3 waits on the instrument.** What follows
   records what was planned, and then what building it turned out to be.

   Three slices, against a Siglent SDS1202X-E: two channels
   sharing one 1 GSa/s ADC, 8 bits, 14 Mpts with one channel on and 7 Mpts each
   with both, 500 µV/div to 10 V/div, 1 ns/div to 100 s/div. As with the
   supply, a simulator speaking its dialect comes first, so that the daemon is
   built and checked before the hardware is plugged in, and the hardware slice
   finds out where the simulator was wrong rather than where the daemon is.

   What the dialect is, from Siglent's programming guide in two revisions
   (PG01-E02C and -E02D), the user manual and the datasheet, and so what the
   simulator has to be. The documents disagree in places, with each other and
   with themselves, and where they do the simulator is told which reading to
   take, so that the hardware slice changes a setting rather than a design:

   - **Transport.** SCPI over a raw socket on port 5025. Port 5024 is Telnet,
     with a `SCPI>` prompt, for people; VXI-11 and USB-TMC are there besides.
     Raw socket, as the supply.
   - **Readout.** `C1:WF? DAT2` answers a header, then `#9` and nine ASCII digits
     of length, then that many bytes — the current memory depth, one byte a
     sample — then `0A 0A`. The header's length depends on `CHDR`, and the
     guides' own examples put the first sample at byte 22 in one revision and
     23 in the other, so a reader finds `#9` and never counts. A sample is an
     8-bit code, and volts are `code × (vdiv / 25) − offset`, from `C1:VDIV?` and
     `C1:OFST?`: 25 codes a division. How a code above 127 becomes negative is
     where the two revisions part: E02D subtracts 256, E02C and its Python
     example subtract 255 — `FC` is −4 in one and −3 in the other. A reader that
     picks wrongly is out by one code, a twenty-fifth of a division, on every
     negative sample and never says so; the hardware settles it, against a
     known voltage.
   - **Time.** Sample *i* is at `−(tdiv × 14 / 2) + i / sara`: fourteen
     divisions across, `TDIV?` and `SARA?`. Both revisions and E02C's Python
     example leave the trigger delay out, and third-party readers add `TRDL?`;
     the hardware settles which.
   - **Pacing.** `TRMD SINGLE` and `ARM` start an acquisition; `INR?` bit 0 says
     one has completed, bit 4 that a Sequence segment has, and bit 13 that the
     trigger is ready, and reading it clears it. `MSIZ` picks the depth. The
     datasheet and the user manual give 14 Mpts with one channel on and 7 Mpts
     each with two; the guide's `MSIZ` entry words interleaving the other way
     round. The simulator follows the datasheet.
   - **The scope's own clock.** In Sequence mode the scope fills a segment per
     trigger, up to 80 000 of them at up to 400 000 a second, and History then
     replays them; `FRAM` picks a frame and `FTIM?` gives its acquire time, to
     the microsecond, on the scope's clock — `FTIM 00: 05: 12. 650814`, with
     the spaces. What that clock counts from is not said.
   - **Sparsing.** `WFSU SP,n` sends every *n*th point, and `NP` and `FP` a
     window of them.

   **Slice 1: a simulated scope, and its acquisitions recorded.** `scopesim`
   speaks the dialect above, answering with the headers and units the scope
   does (`C1:VDIV 5.00E-01V`), and makes its signal from a description —
   a sine, a square, a step, noise — so that a test knows what each acquisition
   holds. Like `psusim` it keeps a transcript: every setting it was given and
   what it holds after, clamped to the 1-2-5 steps the scope has, and for each
   acquisition it handed over, its settings, its frame time and the sha256 of
   its bytes. It keeps Sequence mode and History with their frame times, since
   they are the scope's only account of when it triggered, and it takes the
   dialect's disputed readings — the sign rule, the trigger delay — as settings.
   It can be told to transfer slowly, since at 14 Mpts a readout over the
   network is the slow part of everything here, and the rate is to be measured
   on the hardware, not guessed.

   `ktestd -scope scope1=host:5025` drives it from a goroutine of its own, as a
   supply is driven: ARM, wait for INR bit 0, read each channel, record, again.
   Each channel is a stream, `scope1.ch1`, with `axis_mode` implicit uniform and
   one `run_id` per acquisition (§5.4), the trigger settings in the RUN frame's
   parameters, and a new segment whenever `axis_step` changes (§5.2). The
   settings are channels with §4's accounts — `scope1.ch1.vdiv.set` beside what
   the scope reports holding after it — under the scope's own lease and arming,
   bounded by the limits file, and the stop leaves the scope as it is: stopping
   an acquisition harms nothing, and a stop is for outputs.

   One decision came first, because it is §5.2's problem met again on the
   vertical axis: **how a sample is stored**. Stored as its 8-bit code, with the
   conversion to volts in the field's schema, a megapoint acquisition is 1 MB
   and `Column` hands it back as `[]int8` — but the conversion is schema, and a
   change of volts/div or offset is then a change of schema, which Logb §6.1
   answers with a new `stream_uuid`, exactly as it did the timebase. Stored as
   `f32` volts, the schema never changes, at four times the bytes — 56 MB for
   one full-depth acquisition — and a value the scope never measured at that
   precision. Carrying the scale in the RUN frame instead is ruled out: a
   reader that does not apply it reads codes as volts and says nothing, the
   silent wrong answer §5 exists to refuse.

   **Decided: `f32` volts** (§13). The knob is turned often and the channel
   name has to survive it: §4 rests on `scope1.ch1` naming one thing for the
   whole session, and §5.2's exemption covers `axis_step` alone, so the
   vertical axis has no equivalent of the timebase's way out. The four times
   the bytes are paid on the disk, where §10.4 already says full rate is opt-in
   per channel; the browser gets the envelope either way. The code the
   digitiser actually produced is recorded beside the samples — `vdiv`, `ofst`
   and the sign rule in force, per acquisition, in the RUN frame's parameters
   and in `scope1.acq` — so it is recoverable, and the recording says by what
   arithmetic.

   The reference is the simulator's transcript: `logbcheck -scope` requires every
   acquisition the scope handed over to be in the recording, byte for byte by
   its sha256, under the settings in force when it was taken, with its axis
   computed by the dialect's rule and not the daemon's; and every setting the
   recording says was applied to be the transcript's. Planted faults: an
   acquisition missing, one recorded under the previous timebase, a volts/div
   change that did not open a new stream or segment as decided.

   **Done**: `scope/` for the dialect, the driver and the simulator,
   `cmd/scopesim` to run one, `cmd/ktestd/scope.go` for the daemon's side, and
   `logbcheck -scope`. On a three-second run against `scopesim` with a timebase
   change and a second channel switched on part way through, 223 acquisitions
   of 14 000 samples pass the check: every one is the transcript's, at the same
   position, under the same settings, with the axis the dialect's rule gives,
   and every sample recovering to the sha256 the simulator recorded.

   Four things the build settled that the plan had not:

   - **The hash is taken over recovered codes.** §13 stores volts, so the bytes
     in the file are not the bytes the instrument sent; the checker runs the
     recording's own stated conversion backwards and hashes that. A volt that
     is not a whole code is reported rather than rounded, because rounding it
     away is exactly how a wrong conversion would pass. A daemon that converted
     with the wrong volts/div, the wrong offset or the other sign rule, or that
     stated a conversion it did not use, fails on different bytes.
   - **`sub255` is not a bijection**, which is an argument against PG01-E02C
     that neither document makes. It maps `FF` and `00` both to zero and
     reaches only −127, so one of the 256 codes an 8-bit converter can produce
     means the same as another and a stored volt cannot say which was sent. The
     simulator never emits `FF` under that rule; an instrument that did would
     settle the question by itself.
   - **A run per acquisition needed a writer change in Logb.** `BeginSegment`
     restates every run ever declared, which is right for a sweep and ruinous
     for a scope: an acquisition a second for an hour restates 3600 RUN frames
     into every segment, for a reader that needs only the ones that segment
     carries. `Writer.EndRun` drops a finished run from the set restated, and a
     test holds the difference — 20 acquisitions over 20 segments cost 230 RUN
     frames without it and 20 with.
   - **The planted fault "a volts/div change that did not open a new stream or
     segment" no longer exists**, because `f32` makes a volts/div change
     neither. The rule that still bites is §5.2's on the horizontal axis, and
     that is what is planted instead: a timebase change that did not open a
     segment. Twenty planted faults in all, each caught by the check that
     exists for it, and a faithful recording passing.

   **Slice 2: decimation and the waveform widget.** Built. The rack's writer carries a
   min/max envelope per acquisition, some 3000 columns, made by
   `viewer/decimate` on an acquisition as it arrives rather than on an indexed
   file (§10.4). The scope's own sparsing is no substitute: every *n*th point
   is the aliasing and the lost glitch an envelope exists to prevent, and at
   most it saves a readout's time for a preview. The recording carries the
   envelope too, and full rate only for a channel the configuration says so
   for — continuous full-rate capture is a decision someone made, not a
   surprise disk fill. §10.5's drop policy lands
   here with it: an acquisition the rack's stream cannot keep up with may be
   dropped, and is recorded in that stream as a drop, while bus frames never
   are; the Hub today disconnects a slow consumer whole, which is right for
   neither class. The widget is a `scope` panel type: the envelope drawn as a
   band from typed arrays with no gaps to express (§10.3), the settings in
   force beside it, and the trigger point marked.

   **Done**: the reduction is `(*scope.Acquisition).Envelope`, which is
   `viewer/decimate`'s `Numeric` on an acquisition as it arrives — a test
   asserts the two agree exactly, bucket for bucket, across sizes and bucket
   counts, so this is a faster path to the same reduction and not a second
   opinion about what the reduction is. It had to be a separate path: going
   through `decimate` directly means materialising the acquisition as `float64`
   values and a presence mask first, which at 14 Mpts is 112 MB a trigger for a
   48 kB answer. It allocates nothing per acquisition.

   The envelope is a stream of its own, `scope1.ch1.env`, with `present`,
   `n`, `min` and `max`, the last two guarded on `present`: a bucket that held
   no sample is absent, never a zero volt, and the widget breaks the band there.

   §10.5's drop policy is where the Hub and the scope meet, and the answer is
   that the producer asks first. `stream.Hub.Room` reports how far the slowest
   consumer still has to fall; the daemon compares it against what the envelope
   would add and, when it does not fit, writes the acquisition to the recording
   and not to the rack, recording it in `scope1.acq` as a drop. Measured: with
   a rack consumer that takes the preamble and then stops reading, a run of
   1770 acquisitions recorded 342 to the rack and dropped 678 of the rest,
   every one of them accounted for in the recording, and `logbcheck -scope`
   passing over all 1770. The Hub's own answer — disconnect the consumer whole
   — stays right for bus frames, where a hole in a byte stream is corruption
   rather than loss.

   The widget is a `scope` panel type. Three things the browser needed for it,
   none of which existed: the reader learned to decode RUN frames and hand a
   batch its run, which is where §5.4 puts an acquisition's settings and which
   nothing had read before; the worker gained a path that turns an envelope
   batch into typed arrays and hands them over transferred, separate from the
   per-channel sampling path because an acquisition replaces its predecessor
   rather than extending it; and the recording now states each instrument's
   kind in its control stream's schema, because a supply and a scope are leased
   on paths of their own and a rack that inferred the kind from the shape of a
   name would be wrong the first time a third kind arrived. The reader's
   cross-check against the Go reader gained a scope fixture — a uniform axis, a
   guarded envelope, RUN frames, and a timebase change opening a segment —
   since all three are things a reader can get plausibly wrong and look right
   doing.

   **Slice 3: the SDS1202X-E.** The simulator's dialect checked against the
   instrument — the headers, the terminator, the sign rule, the fourteen
   divisions, the trigger delay — and each difference fixed in the simulator
   first, so that the tests keep describing the hardware. The readout rate
   measured at each memory depth, since it decides how often the rack can
   redraw and how much of the bus a slow readout lets pass between
   acquisitions. And the question this milestone exists to answer, which the
   simulator cannot: **when the scope triggered, on the recording's timeline.**
   A single acquisition carries no trigger time, and the daemon knows only when
   it read one out, hundreds of milliseconds late and by a variable amount. But
   the scope does stamp its History frames, to the microsecond, on its own
   clock, so the question shrinks from when each acquisition triggered to where
   the scope's clock lies on the recording's — one offset and a rate, not a
   stamp per acquisition. Sequence mode then looks like the right way to
   acquire for correlation: segments taken at trigger rate with no readout
   between them, each stamped by the scope, read out afterwards. Until the
   clocks are tied, an acquisition's time is recorded as userspace and stamped
   at readout, with the scope's frame time beside it, never in place of it
   (§11). To be measured: what `FTIM?` counts from, how fast its clock drifts
   against the daemon's, and whether it survives a stop and a run.

   The tie is the scope's own CAN trigger: triggered on a frame that `ktestd`
   also records with the kernel's timestamp, the two clocks meet at a known
   instant, and a second such frame minutes later measures the rate. That is
   the §1 correlation — a supply transient against the bus traffic it caused —
   made by measurement rather than by hand. It needs a real CAN adapter on a
   real bus, since `vcan` puts nothing on a wire, and it is the scope's trigger
   that sets the bus: classic CAN, 5 kbit/s to 1 Mbit/s, no FD.

   **The interface is the MCP2518FD on an Orange Pi Zero 2** (§8), and
   `ktestd` runs there. The board's 20 MHz crystal clocks the controller, so
   its stamps have 50 ns resolution and a crystal's accuracy, tens of parts per
   million. The kernel converts them at the crystal's nominal rate from the
   moment the interface opens and never corrects it, so even that drifts from
   real time, by a fraction of a second an hour: the rate is measured —
   against the kernel's software stamps of the same frames, over an hour and as
   the board warms — and recorded beside the stream, never silently rescaled
   (§11). Where on the frame the stamp is taken, its start or its end, is read
   from the driver and then seen on the scope, which watches the same wire.

   The crystal costs two things. SPI runs at most at 8.5 MHz, 0.85 × 20 MHz /
   2, which carries a fully loaded classic bus and moderate FD rates. And at
   the highest FD data rates the bit timing is coarse: 5 Mbit/s leaves four
   clock periods to a bit, where 2 Mbit/s leaves ten. A 40 MHz crystal doubles
   both. `ktestd` has two changes to make for it. `can/` asks the kernel for
   hardware stamps and reads only the software one, so it learns to read the
   hardware stamp and to record which clock each stream is on, the software
   stamp beside it. And it reads back the bitrates the interface actually
   runs, because `ip link` accepts a rate the divisors cannot reach and runs
   the nearest one. `cmd/vcanprobe` runs against the real interface before
   `ktestd` does (§8).

   **A DSD TECH SH-C31A** is the bench bus's second node — a CAN bus needs one
   to acknowledge every frame — and the comparison. It is a CANable 2.0
   derivative; from September 2026 it ships `gs_usb` firmware v1.4 (`SH-C31x`
   by `DSD TECH` over USB; older stock is flashed over DFU, the image checked
   against its published SHA256), classic and FD, whose image advertises
   hardware timestamps (feature word `0x5fb`, read out of the released binary
   rather than taken from its notes). Its limits are why it is not the
   interface. It has no crystal: its stamps run on an internal RC oscillator
   specified to about one per cent, converted at the nominal rate and never
   corrected, which DSD TECH's own "0.94 % deviation" fits. By the source of
   the fork it is built from, a received frame is stamped when the firmware
   picks it up and a transmitted one when it is handed to the controller, not
   when it has won the bus. And the source of DSD TECH's own changes is not
   published. On the same bus as the MCP2518FD, it shows what the crystal and
   the transmit event FIFO buy.
5. **Scripting.** **Lua first, `expr` after.** §9.2 keeps both layers, and the
   build order is now the other way round from what this line first said,
   because the two are not equally urgent. Orchestration is the thing a bench
   cannot be driven without — sweep a parameter, step the supply, wait for a
   signal, take an acquisition — and it is what a client does over REST today,
   from a shell loop, which is exactly the logic §9.1 says should survive the
   client disconnecting. Limits and conditions, `expr`'s half, are already
   enforced by the limits file and the control plane: a JSON floor and a
   per-channel maximum, read once at start and unchangeable by any request
   (§7). `expr` generalises that from a number to a predicate, which is worth
   having and is not what blocks anything. Lua first, then, under §9.4's rule:
   one `LState` per run on a goroutine of its own, bounded with `context`, and
   never in the timing-critical path.

The failure mode to avoid is five things at 70%. Build one specific configuration
end to end before generalising anything; the generality extracted from two working
cases will be right, and the generality designed before the first will not.

## 13. Open decisions

§5 is closed and out of this list, and so is rendering a held channel — Logb's
viewer draws them stepped and anchored as of `@b8cf4cc`. What remains is about the
daemon, and none of it blocks milestone 1.

- **Sync-frame cadence for live streams.** Built, and the answer is smaller than
  "synthesise a segment": `stream.Hub` keeps the bytes that opened the current
  segment, exactly as they were written, and replays them to each new subscriber. So
  nothing is fabricated, and the DATA frames that follow are consistent with the SYNC
  in front of them by construction. A late joiner's stream is the same frames as the file, not
  always the same bytes — a replayed preamble groups metadata and attachments ahead of
  the segment's schemas, which no reader can tell apart, since a schema still precedes
  every DATA frame using its id. What is still unmeasured is the restatement cost at
  high channel counts, which is the part of this that was always the real question.
- **HTTP/2 flow control on the data endpoint** (§6.1). **Decided: h2 stays on**, on a
  measurement (§12, milestone 2): over TLS, HTTP/2 and HTTP/1.1 consumers each
  received the whole stream at `vcan`'s full rate, and the Hub dropped neither. The
  measurement's limits are the producer's rate and curl's generous windows; a client
  with a small h2 receive window is the one case left to measure.
- **Does the daemon resume a configured acquisition on boot with no browser
  connected?** For a long thermal run after a power blip the answer is probably yes,
  which makes configuration persistent state on the machine rather than something a
  browser pushes in.
- **How ISO-TP and J1939 payloads are stored.** Logb makes bus payloads fixed-width
  `bytes` fields deliberately: SPEC §6.4 records that a variable-length field costs
  the batch its seekability, and that a CAN payload of 8 or 64 bytes is not worth a
  length prefix. A 4095-byte ISO-TP message inverts that arithmetic — a fixed field
  that wide is mostly padding in every record, and a variable one gives up
  seekability for the whole batch. This is the one place where these protocols may
  ask something of the format rather than only of the daemon. Decide it against a
  real diagnostic trace, not in advance.
- **How LIN is reached.** In scope, and no longer a question about kernel modules
  (§8) — both candidates avoid the out-of-tree `sllin`. Either the uCAN ULC dongle,
  a CDC ACM device needing no driver and scheduling master frames in firmware, or the
  Pico interface of `doc/can-lin-pico-interface.md`, speaking `gs_usb` for CAN with
  LIN on a second endpoint as CDC ACM behind a small userspace daemon. Buy first and
  build only if the dongle's ceilings bite: 15 master slots, two baud rates, an
  unverified SLCAN claim, and whether it timestamps on the wire. An LDF parser is
  needed either way.
- **How a scope sample is stored** (§12, milestone 4). **Decided: `f32`
  volts.** The alternative was the digitiser's 8-bit code with the conversion
  in the schema — compact, and exactly what was measured — but it makes every
  volts/div or offset change a schema change and so, under Logb §6.1, a new
  stream identity: §5.2's problem on the vertical axis, with the same three
  ways out, and this time without §5.2's escape, since `axis_step` is exempted
  from the identity rule and a field's conversion is not. `f32` costs four
  times the bytes and states a precision the scope did not measure; it buys a
  schema that never changes, so `scope1.ch1` stays one stream across every turn
  of the volts/div knob, which is what §4 needs a channel name to mean. The
  third way — codes with the scale beside them in the RUN frame — stays ruled
  out: a reader that ignores it reads codes as volts and says nothing.
  The measured 8-bit code is not lost: the conversion is recorded per
  acquisition, in the RUN frame's parameters and in `scope1.acq`, so the code
  is recoverable from the volts and the recording says how.
- **Automotive Ethernet.** Out of scope — a separate project (100BASE-T1 media
  converter plus AF_PACKET).
