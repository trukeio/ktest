# Ktest peer protocol: two racks, one DUT

Status: **draft, protocol version 1**, 2026-11-20. Normative.

This document specifies how a **master** test application drives a **secondary** that operates and observes a device under test (DUT). It is written so that either side can be something other than `ktestd`:

- a lab's own test software as master, driving a `ktestd` DUT rack;
- `ktestd` as master, driving a DUT rack built on other software;
- two programs, neither of them `ktestd`, that agree on this protocol.

`ktestd` implements both roles today (`-contract` makes it a secondary, `-peer` a master). Where `ktestd` does something this document does not require, or does not yet do something it requires, §13 says so. The design rationale is in `doc/two-racks-handoff.md`; this document does not repeat it.

The key words MUST, MUST NOT, SHOULD, SHOULD NOT and MAY are to be read as in RFC 2119 and RFC 8174.

## 1. Model

```
        master                                     secondary (DUT rack)
  ┌──────────────────┐   REST over HTTPS     ┌─────────────────────────────┐
  │ procedure        │ ─ lease, arm, mode, ─▶│ modes, actions              │
  │ daemon rules     │   actions, clock      │                             │
  │ (stress control) │                       │ monitors ◀── DUT inputs     │
  │ recording        │ ◀─ /stream/sub ────── │ watermark                   │
  └──────────────────┘   Logb, per record    └─────────────────────────────┘
          │                                                 │
     lab instruments                                       DUT
```

- The secondary **owns the DUT**. It powers it, runs its operating modes, and judges whether its functions behave. It keeps doing so whatever happens to the master or the link.
- The master **owns the stress**. It applies the conditions a standard prescribes and removes them when the DUT side asks for protection or can no longer vouch for the DUT.
- Everything the master may see or ask of the secondary is declared in one file, the **contract** (§3). Nothing else crosses the link: no raw frames, no arbitrary channels.
- Commands go master → secondary over REST and are answered *accepted* or *refused*, never with state (§6). Results come back as records on the subscription stream (§7).
- Monitor results are only as good as the **watermark** (§5.6): a monitor state is final once the watermark has passed its time. Silence is never "nothing happened".

### 1.1 Terms

| Term | Meaning |
|---|---|
| contract | the `dut-interface.json` file both sides hold, byte for byte (§3) |
| input | a name in the contract for a channel the secondary records |
| monitor | a named boolean expression over inputs and other monitors, with a state (§5) |
| mode | a named DUT operating mode, entered by an action, with a settle time |
| action | a named operation the secondary runs on the DUT on request |
| watermark | a time T such that every monitor transition at or before T has been sent |
| lease | the exclusive right to change the DUT's mode and run its actions |
| record | one Logb record: a time and the fields of its stream's schema |

### 1.2 Conformance classes

| Class | Must implement |
|---|---|
| **Secondary** | §2, §3, §4, §5, §6, §7, §8, §10 |
| **Master** | §2, §3 (load and hash), §6 (client side), §7 (reader), §8, §9 |

A secondary MAY implement more endpoints than §6 lists. A master MUST NOT rely on any.

## 2. Transport and authentication

- The secondary MUST serve HTTPS, TLS 1.2 or later. A master MUST verify the secondary's certificate against a certificate it was configured with (a pinned certificate or a private CA); it MUST NOT disable verification.
- Every request MUST carry `Authorization: Bearer <token>`. A secondary MUST answer a missing or unknown token with `401` and MUST NOT act on the request.
- A token has a **name** and a **role**:

  | Role | Allows |
  |---|---|
  | `view` | every `GET` of §6 and §7, and `POST /dut/disarm` |
  | `operate` | everything |

  A master needs `operate`.
- Over TLS the lease holder (§6.3) is the token's name. A secondary MUST NOT let a client name itself otherwise; it MUST refuse a `holder` that differs from the token's name.
- A secondary MAY serve the same interface on a local socket without TLS; this document does not cover it.

## 3. The contract

The contract is a JSON file, UTF-8, one object. A secondary loads it at start and MUST refuse to start if it fails any rule below. A master loads its own copy and MUST refuse to proceed if its hash differs from the secondary's (§9.1).

```json
{
  "contract": "acme-bcm-v3",
  "schema": 1,
  "inputs":   {"Vx": "psu1.ch1.meas.v", "Speed": "EngineData.EngineSpeed",
               "Alive": {"channel": "DutStatus", "max_age": "50ms"}},
  "monitors": {
    "vx":    {"ok": "Vx > 1.0V"},
    "speed": {"ok": "abs(Speed - 3000) <= 30", "debounce": "20ms", "modes": ["active"]},
    "alive": {"ok": "age(Alive) < 30ms and delta(Alive.counter, 16) == 1"},
    "abort": {"ok": "Vx < 16V"},
    "ready": {"ok": "vx and speed and alive"}
  },
  "modes":    {"active": {"action": "enter_active", "settle": "500ms"}},
  "resource": "actions.robot",
  "actions":  {"enter_active": "Enter Active", "recover": "Recover"},
  "enums":    {"class": ["A", "B", "C", "D", "E"]},
  "timeouts": {"ready": "10s", "recover": "5s"}
}
```

### 3.1 Keys

| Key | Required | Value |
|---|---|---|
| `contract` | yes | the contract's name, non-empty |
| `schema` | yes | `1`, the contract schema this document defines |
| `inputs` | yes, non-empty | alias → channel name, or alias → `{"channel", "max_age", "enum"}` |
| `monitors` | yes, non-empty | name → `{"ok", "debounce", "modes"}`; `ok` required |
| `modes` | no | name → `{"action", "settle"}` |
| `resource` | no | a file, relative to the contract, that defines the actions; required if `actions` is non-empty |
| `actions` | no | name → an identifier in the resource |
| `enums` | no | name → ordered list of distinct strings |
| `timeouts` | no | name of a monitor, mode or action → duration |

### 3.2 Rules

- **Strict.** An unknown key anywhere, a second JSON value after the object, or a wrong type refuses the contract.
- **Names** of inputs, monitors, modes, actions and enums match `[A-Za-z_][A-Za-z0-9_]*`. A monitor MUST NOT share a name with an input. `watermark` and `gap` are not monitor names.
- **Inputs** name decoded channels the secondary records: a stream and a field, `stream.field`, or a stream whose single value field is meant. A raw bus identifier is not an input. Field names may contain dots, so a reference resolves to the **longest declared stream name that prefixes it**, then a field; a reference that resolves more than one way refuses the contract.
- **`max_age`** is how old an input's latest sample may be before monitors that read it go `unknown`. The default is three times the input's observed sample period. A held channel (one whose value stands until the next record) never goes stale and MUST NOT have a `max_age`.
- **Monitors** are expressions of the `monitor` profile (§4). Every name they read MUST resolve to an input, an input's field, or another monitor. A monitor that reads itself, directly or through others, refuses the contract.
- **`abort` is reserved.** A monitor of that name, when `dev`, is the secondary asking the master to remove the stress (§9.4).
- **Modes** name an entry action that `actions` declares, or none. `settle` defaults to 0. A monitor's `modes` list only declared modes.
- **Actions** map to identifiers in `resource`. What an action does is the secondary's business; `ktestd` runs them as Robot keywords (§10.3).
- **Durations** are a number and a time unit: `ns`, `us`, `µs`, `ms`, `s`, `min`, `h`.

### 3.3 Hashes

- The **contract hash** is the lowercase hex SHA-256 of the contract file's bytes, as read.
- The **resource hash** is the same over the resource file, or `""` if there is none. A changed resource is a changed contract in all but its hash, so both are compared (§9.1).

## 4. Expression language

One language serves contract monitors, limit expressions and procedure conditions, so that a condition means the same wherever it is written. A secondary MUST evaluate monitors with exactly these semantics; a master that writes conditions SHOULD use the same language.

### 4.1 Grammar

```
expr     = or
or       = and { "or" and }
and      = not { "and" not }
not      = [ "not" ] cmp
cmp      = sum [ ("==" | "!=" | "<" | "<=" | ">" | ">=") sum ]
sum      = prod { ("+" | "-") prod }
prod     = unary { ("*" | "/") unary }
unary    = [ "-" ] atom
atom     = number [unit] | string | "True" | "False" | ref | call | "(" expr ")"
ref      = name { "." name } | "$" name { "." name } | "${" name { "." name } "}"
call     = func "(" [ expr { "," expr } ] ")"
func     = "abs" | "min" | "max" | "age" | "delta" | "rate" | "mean" | "stable"
```

There is no assignment, no user function and no side effect. `&&`, `||` and `!` are not operators.

### 4.2 Values and units

- Values are booleans, strings, ordered-enum values, or numbers carried in **SI base units**.
- **Units** accepted on literals: `V`, `A`, `W`, `Ohm` (or `Ω`), `Hz`, `s` with SI prefixes `p n u µ m k M G`; `min`, `h`; `dB`, `°C` (or `degC`) and `%` without prefixes. `W` is `V·A`, `Ohm` is `V/A`, `Hz` is `1/s`. `%` is dimensionless, scaled 0.01.
- A channel's unit comes from its schema. A unit outside the list above is its own dimension.
- **Dimensions are checked when the expression is compiled.** Comparing or adding quantities of different dimension is a load error. A bare number takes the unit of the other operand: `abs(Speed - 3000) <= 30` is valid for `Speed` in rpm.
- `<` and `>` on enum values compare by declared order.

### 4.3 Unknown

Any value may be **unknown**: an input not yet seen or gone stale, a division by zero, an empty window.

- Arithmetic and comparison with an unknown operand are unknown.
- `and`, `or` follow Kleene logic: `false and unknown` is false; `true or unknown` is true; otherwise unknown. `not unknown` is unknown.
- A monitor whose expression is unknown is in state `unknown` (§5.1), never `dev`.

### 4.4 Functions

Time is the samples' time, never a clock read during evaluation. Each call site keeps its own bounded state.

| Function | Value |
|---|---|
| `abs(x)`, `min(a, b, …)`, `max(a, b, …)` | as usual |
| `age(x)` | time since the last sample of stream or field `x`, at the evaluation time |
| `delta(x)` | the last sample of `x` less the one before it |
| `delta(x, m)` | the same modulo constant `m > 0`, for a counter that wraps at `m` |
| `rate(x, w)` | samples of `x` per second over the last window `w` |
| `mean(x, w)` | mean of `x`'s samples over the last window `w` |
| `stable(x, w)` | true if `x` has not changed for `w` |

A window that held more samples than the implementation keeps answers unknown until it no longer does. An implementation MUST state its per-window limit.

### 4.5 Profiles

| Profile | Time functions | Arithmetic | Used by |
|---|---|---|---|
| `monitor` | yes | yes | contract monitors |
| `limit` | yes | yes | limit expressions |
| `condition` | no | no | procedure `IF` / `WHILE` |

## 5. Monitor semantics (secondary)

### 5.1 States

| State | Code | Meaning |
|---|---|---|
| `ok` | 0 | the expression holds, and has held for the debounce |
| `dev` | 1 | it does not hold, and has not for the debounce |
| `unknown` | 2 | there is no answer: an input unseen or stale, an unknown value, a gap |
| `inactive` | 3 | the applied mode is not one the monitor lists |
| `settling` | 4 | a listed mode was applied less than its `settle` ago |

Every monitor is `unknown` from the start until its inputs are known.

### 5.2 When a monitor is evaluated

On every new sample of any stream it reads, and at every **deadline** its state implies: an `age()` crossing its threshold, a debounce expiring, an input going stale, a settle ending, a window edge.

### 5.3 Timestamps

A transition carries the time of the sample that caused it, or of the deadline at which it became true. **Never the time it was evaluated.**

### 5.4 Debounce

- The expression must be false for at least `debounce` before `ok → dev`, and true for at least `debounce` before `dev → ok`. Within the debounce the monitor keeps its state. The transition's time is the end of the debounce: the time the expression's value changed, plus `debounce`.
- A monitor that is not yet judged (coming from `unknown`, `inactive` or `settling`) stays `unknown` until its expression has held one value for `debounce`, then takes `ok` or `dev`.
- `unknown`, `inactive` and `settling` are entered without debounce.

### 5.5 Purity

The same input history MUST give the same transitions, with the same timestamps to the nanosecond, whether evaluated live or replayed from a recording. A secondary MUST be able to show this; `ktestd` does it with `monitor-check` (§12).

### 5.6 Watermark

- The secondary publishes `mon.watermark` with field `upto`: every transition with timestamp ≤ `upto` has been sent.
- It MUST be emitted at least every 10 ms while the evaluator keeps up. SHOULD be configurable.
- It MUST NOT go backwards.
- It MUST stop advancing if the evaluator falls behind. It MUST NOT advance past inputs not yet evaluated.
- The evaluator MAY hold a record for a short **reorder window** for earlier ones to arrive; the watermark trails the clock by at least that window.

### 5.7 Gaps

The evaluator MUST NOT drop an input silently. If it loses records (a full queue) or receives one behind the watermark, it MUST:

1. publish `mon.gap` with the time range, count and reason (`overflow` or `late`), and
2. hold every monitor `unknown` from the watermark at that moment until its inputs are known again.

### 5.8 Isolation

Evaluating monitors MUST NOT delay the secondary's acquisition or DUT operation. If evaluation falls behind, the watermark stops (§5.6), and the master acts on that (§9.4).

## 6. REST interface (secondary)

Base: the secondary's HTTPS URL, `https://host:port`. Request and response bodies are JSON, `Content-Type: application/json`. Unknown keys in a request body are refused.

### 6.1 Answers

A command is answered **accepted** or **refused**, never with the resulting state. The result arrives on the stream (§7).

| Answer | Status | Body |
|---|---|---|
| accepted | `202` (`201` when a lease is created) | `{"seq": n}`, plus `token`, `ttl` for a lease |
| refused, bad request | `400` | `{"error": "<sentence>"}` |
| refused, no such mode or action | `404` | `{"error": …}` |
| refused, lease or permission | `403` | `{"error": …, "seq": n}` |
| refused, conflict (not armed, lease held by another, DUT busy) | `409` | `{"error": …, "seq": n}` |
| refused, unavailable | `503` | `{"error": …, "seq": n}` |
| not authenticated | `401` | `{"error": …}` |

- `seq` is the request's sequence number in the secondary's recording. It is present whenever the request was recorded, refusals included. A master SHOULD record it with its own record of the request.
- `error` is a human sentence. A master MUST NOT parse it.
- A master MUST treat any `2xx` as accepted and anything else as refused.

### 6.2 Endpoints

| Method and path | Role | Lease | Request | Accepted answer |
|---|---|---|---|---|
| `GET /dut/contract` | view | — | — | `200` `{"protocol", "contract", "sha256", "resource_sha256", "provenance": {…}}` |
| `GET /clock` | view | — | — | `200` `{"t": <Unix ns>}` |
| `POST /dut/lease` | operate | — | `{"holder", "ttl"}` | `201` `{"seq", "token", "ttl"}` |
| `PUT /dut/lease` | operate | yes | optional `{"ttl"}` | `202` `{"seq", "ttl"}` |
| `DELETE /dut/lease` | operate | yes | — | `202` `{"seq"}` |
| `POST /dut/arm` | operate | yes | — | `202` `{"seq"}` |
| `POST /dut/disarm` | view | — | — | `202` `{"seq"}` |
| `PUT /dut/mode` | operate | yes, armed | `{"value": "<mode>" \| ""}` | `202` `{"seq"}` |
| `POST /dut/actions/{name}` | operate | yes, armed | — | `202` `{"seq"}` |
| `GET /stream/sub?ch=…` | view | — | — | `200`, a Logb stream (§7) |

The lease token travels in the header `Ktest-Lease: <token>`.

### 6.3 Lease

- One lease on the DUT at a time. Taking it while another holder has it is refused `409`.
- `holder` is required; over TLS it is the token's name (§2). At most 32 bytes of UTF-8.
- `ttl` is a duration; default `30s`. The lease lapses if not renewed within it. A master SHOULD renew at a third of the TTL.
- **A lapsed lease stops nothing on the DUT.** It only stops the holder from changing the mode or running actions. Cyclic traffic and anything a mode started belong to the secondary and continue (§10.1).

### 6.4 Arm and disarm

- Mode changes and actions need the lease **and** the DUT armed under it.
- `POST /dut/disarm` ends the action under way and stops the DUT rack's cyclic tasks. It needs no lease, so that any viewer can stop the DUT side.

### 6.5 Mode

- `PUT /dut/mode {"value": "active"}` asks for a declared mode; `""` asks for none.
- On acceptance the secondary records `mode` with event *requested*, runs the mode's entry action, and on its end records `mode` with event *applied* (or *failed*, with a message).
- The monitors that list the mode are `settling` from the *applied* record for the mode's `settle` time, then evaluated.
- Only one mode change or action runs at a time; a second is refused `409`.

### 6.6 Actions

- `POST /dut/actions/{name}` runs a declared action. Its outcome arrives as `dut.action.<name>` records: event *started*, then event *ended* with an outcome (§7.4), under the request's `seq`.
- An action MUST have a timeout. The contract's `timeouts` entry for it, if any, sets it.

### 6.7 Clock

`GET /clock` answers the secondary's wall clock in Unix nanoseconds, read as late as possible before answering. The response MUST NOT be cached.

## 7. Subscription stream (secondary)

```
GET /stream/sub?ch=mon.*,DutStatus
```

### 7.1 Format

- A **Logb** stream (the `github.com/trukeio/logb` format), **uncompressed**, **one record per DATA frame**, each frame written and flushed the moment its record is recorded.
- It opens with the stream metadata and the schemas and current held values of every stream it will carry, so a subscriber that joins mid-run learns the present state.
- `ch` is a comma-separated list of stream names or globs (`path.Match` syntax: `*`, `?`, `[…]`). A malformed glob is refused `400`.
- Always sent, whatever `ch` says: `mon.watermark`, `mon.gap`, `mode`, `mode.*`, and the metadata.
- A subscriber that falls behind is disconnected. The secondary's own recording is not affected. A subscription never ends of its own accord; its end is a link loss (§9.3).

### 7.2 Time

- Every stream has a time axis in nanoseconds (`AxisExp = -9`), field `t`, first in the record.
- The metadata key `recording.epoch` is Unix nanoseconds. A record's time is `recording.epoch` + its axis value. A stream without `recording.epoch` MUST be refused by the master.
- Times are on the **secondary's clock**. The master converts them (§8).

### 7.3 Metadata

The metadata MUST include `recording.epoch`, `peer.protocol` (§11) and the secondary's provenance: software name and version, host, contract name and hash. Its keys are the secondary's choice beyond `recording.epoch`; a master records them as it receives them.

### 7.4 Streams

All integers little-endian. Strings are fixed-width, UTF-8, NUL-padded. Offsets in bytes; `t` is bytes 0–7, signed.

**`mon.<name>`**, one per monitor, held:

| Field | Bytes | Type | Meaning |
|---|---|---|---|
| `state` | 8 | uint8, held | the state from this record on, code of §5.1 |
| `from` | 9 | uint8 | the state before |
| `cause` | 16–79 | string 64 | what caused it: a stream's sample, a deadline, a mode, a gap |

Schema metadata: `contract` (the contract hash).

**`mon.watermark`**, held:

| Field | Bytes | Type | Meaning |
|---|---|---|---|
| `upto` | 8–15 | int64 ns, held | every transition at or before this Unix time has been sent |

**`mon.gap`**:

| Field | Bytes | Type | Meaning |
|---|---|---|---|
| `from` | 8–15 | int64 ns | first record not used, Unix time |
| `to` | 16–23 | int64 ns | last |
| `lost` | 24–27 | uint32 | how many records |
| `reason` | 32–127 | string 96 | `overflow…` or `late…` |

**`mode`**:

| Field | Bytes | Type | Meaning |
|---|---|---|---|
| `seq` | 8–11 | uint32 | the request; 0 for the start |
| `event` | 12 | uint8 | 0 start, 1 requested, 2 applied, 3 failed |
| `requested` | 16–47 | string 32, held | the mode last asked for |
| `applied` | 48–79 | string 32, held | the mode the DUT is in |
| `by` | 80–111 | string 32 | who asked |
| `message` | 112–271 | string 160 | why the entry action failed |

**`dut.action.<name>`**:

| Field | Bytes | Type | Meaning |
|---|---|---|---|
| `seq` | 8–11 | uint32 | the request that started it |
| `run` | 12–15 | uint32 | action run number since start |
| `event` | 16 | uint8 | 1 started, 2 ended |
| `outcome` | 17 | uint8 | at the end: 1 ok, 2 nok, 3 timeout, 4 refused, 5 link_lost, 6 fail, 7 stopped |
| `via` | 18 | uint8 | 1 local socket, 2 TLS |
| `by` | 24–55 | string 32 | who asked |
| `mode` | 56–87 | string 32 | the mode this is the entry action of; empty if asked directly |
| `message` | 88–247 | string 160 | at the end: why it failed |

Other streams a subscriber names (decoded DUT messages, supply measurements) come with their own schemas; a master MUST read them by schema, not by assumed layout.

A master SHOULD read every stream by its schema even for the ones above, and SHOULD treat the layouts in this section as what a secondary MUST declare, not as what a master may hard-code.

## 8. Clocks

- The master estimates the secondary's clock offset, NTP-style:
  - sample `GET /clock` at least once a second, noting send time `s` and receive time `r` on its own clock;
  - each sample gives `offset = t − (s + (r − s)/2)`, right to within `(r − s)/2`;
  - keep the last N samples (N ≥ 8) and use the one with the **shortest round trip**.
- The master MUST record the offset and its uncertainty whenever they change.
- Every window the master judges (§9.5) MUST be converted to the secondary's clock with the offset, and widened by the uncertainty where a decision depends on it.
- Neither side may assume the clocks agree. Neither side SHOULD step its clock during a run; a secondary MUST NOT let a clock step move the watermark backwards.

## 9. Obligations of a master

### 9.1 Connect

1. `GET /dut/contract`. Compare `sha256` and `resource_sha256` with the hashes of the master's own copies. **If either differs, the master MUST NOT proceed.**
2. Check that every mode, action and monitor the master's procedures name exists in the contract. If not, MUST NOT proceed.
3. Record the secondary's contract name, hashes and provenance.
4. Take the lease, arm, open the subscription, start the clock estimate.

### 9.2 Lease

Renew the lease well inside its TTL. On losing it (refused renewal), treat the link as lost (§9.3).

### 9.3 Link loss

The link is lost when the subscription ends or errors, or when requests fail. The master MUST record each loss and reconnection. On reconnection it MUST repeat §9.1 step 1.

### 9.4 Protection rules

A master that applies stress MUST implement at least these two rules, and MUST NOT let any client turn them off:

| Rule | Holds while | Then |
|---|---|---|
| `monitoring_lost` | the link is lost, **or** the latest `mon.watermark.upto` (converted to master time) is older than a configured age | remove the stress |
| `dut_abort` | `mon.abort` is `dev` | remove the stress |

- "Remove the stress" means bringing the stress instruments to their safe state, outputs off, by the same path as an operator stop.
- While a rule holds, arming a stress instrument MUST be undone the same way.
- Each rule's taking hold and clearing MUST be recorded with its cause. A procedure under way MUST fail with the rule's name as its cause.
- The watermark age SHOULD be at most 100 ms. The rules SHOULD be evaluated on each received record and at least every 10 ms.

### 9.5 Judging a window

To decide whether a monitor deviated over a window [t0, t1] (in the secondary's time):

1. wait until `mon.watermark.upto ≥ t1`;
2. the monitor deviated if it was `dev` **or `unknown`** at any instant in [t0, t1], counting the state held at t0.

A master MUST NOT judge a window before the watermark has passed its end: until then, a later-arriving transition may still change the answer.

### 9.6 Recording

The master MUST record, with its own clock:

- every request it sends and its answer, with the secondary's `seq`;
- every record it receives, with both the secondary's time and its own receive time, under a prefix naming the peer (`ktestd`: `<peer>.<stream>`, e.g. `dut.mon.alive`);
- the clock offset and its uncertainty;
- link state;
- rule firings.

### 9.7 What a master MUST NOT do

- send anything to the DUT except through §6;
- run DUT operation itself: it changes modes and runs actions only;
- treat a lapsed lease, a lost link or its own stop as something the secondary needs to be told to keep the DUT safe.

## 10. Obligations of a secondary

### 10.1 The DUT survives the master

Link loss, lease expiry and master restarts MUST NOT stop DUT operation: the applied mode, cyclic traffic and monitoring continue. Only `POST /dut/disarm`, the secondary's own operator, or the secondary's own rules stop them.

### 10.2 Recording

The secondary records everything it streams, plus every request and its answer, in its own recording. Every record sent on `/stream/sub` MUST also be in that recording, with the same time.

### 10.3 Actions

The secondary decides how actions are implemented. It MUST give each action a timeout, run one at a time, and report it by §7.4. `ktestd` runs them as keywords of a Robot-subset resource file; another secondary MAY use anything.

### 10.4 Latency

From a sample arriving to the resulting transition being on the wire SHOULD take under 5 ms at the 99th percentile on loopback. A secondary SHOULD measure and publish this figure.

## 11. Versioning

- The **contract schema** is `"schema": 1` (§3).
- The **protocol version** is 1. A secondary MUST state it in the `GET /dut/contract` answer as `"protocol": 1`, and in the subscription stream's metadata as `peer.protocol` = `1`. A master MUST refuse a secondary that states a version it does not implement, in either place. A secondary that states none is version 1.
- A new version is needed for any change that would make an existing conforming master or secondary misread the other. Adding an optional request key, an answer key, a stream or a metadata key does not need one: a reader ignores keys and streams it does not know.

## 12. Conformance

*Planned.* The kit will consist of:

- **secondary conformance**: a simulated DUT (`cmd/dutsim`), a set of contracts and input recordings, and the transitions and watermarks each must produce, checked with `monitor-check`'s evaluator as reference;
- **master conformance**: a reference secondary that can be told to stall the watermark, raise `abort`, drop the link and refuse requests, and a checker of the master's recording against §9.

## 13. `ktestd` against this document

| Item | `ktestd` today |
|---|---|
| §11 protocol version | stated in both places; `peer.Protocol` |
| §2 roles | as specified; tokens file holds `name role sha256` |
| §6.2 `POST /dut/disarm` | implemented; also `POST /disarm` stops the whole rack |
| §7.1 always sent | as specified |
| §8 clock sampling | every 1 s, N = 8 |
| §9.4 rules | `limits.json` `rules`: `monitoring_lost`, `dut_abort`, `remove_stress` |
| §9.6 recorded names | `<peer>.<stream>` with `peer_t` and `restated`; `<peer>.link`; `<peer>.clock`; `rules` |
| §4.4 window limit | fixed per call site in package `expr` |
| §10.4 latency | measured with `cmd/sublat` |

## 14. Go libraries

| Package | For | Status |
|---|---|---|
| `contract` | either side: the contract file read strictly, its hashes and names (§3) | done |
| `expr` | either side: the language of §4 | done |
| `peer` | a master: `Client`, `Link`, `View`, `DUT`, `Guard` (§6–§9) | done; `ktestd -peer` runs on it |
| `monitor` | a secondary: contract loader and evaluator (§3, §5); how actions are defined is a hook, `ActionCheck` | done |
| `secondary` | a secondary: `Server`, over the host's streams and an `Actions` it runs (§5–§7, §10); the record layouts of §7.4 and the `/stream/sub` filter, which `ktestd -contract` shares | done |

A program that is a DUT rack declares its streams with `record.Schema`, gives `secondary.New` the contract, its streams, an `Actions` and an `Auth`, serves `Server.Handler` over TLS, and writes its records with `Server.Write`. The server's reorder window (`Config.Window`, 20 ms by default) must exceed the longest the program takes between a record's time and writing it; a later record is a gap.

`ktestd -contract` keeps its own event loop, which it shares with its bus and instruments, and so is not built on `secondary.Server`; both are held to the same layouts and the same evaluator, and `monitor-check -live` checks either's recording.
