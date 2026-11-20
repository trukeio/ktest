# Two racks, one DUT: implementation handoff

This document hands a design over for implementation. It is written for a Claude Code session working in this repository. It is self-contained: everything needed is here, and the related design note on claude.ai is a reader's version of the same content.

Read this whole document before writing code. Then read `doc/test-software-outline.md` §6, §7, §9 and §12, and `cmd/ktestd/main.go` around `type loop`.

## 1. The problem

A lab tests a device under test (DUT) with two racks. Each rack is driven by its own `ktestd`.

| Rack | Purpose | Its `ktestd` |
|---|---|---|
| Lab rack | applies the conditions a standard prescribes (climatic, vibration, EMC, transients, supply profiles) and measures them | **master**: runs the lab's validated procedures |
| DUT rack | powers, operates and observes the DUT | **secondary**: runs DUT operating modes and judges whether DUT functions behave |

Both are validated and under change control. The master changes rarely. The secondary changes with each DUT.

The master's procedure must give the DUT rack instructions at defined points (change mode, run an action), react to DUT data (pass or fail, threshold search, removing the stress), and record DUT data with its own. Each side keeps its own validation.

The current live stream is too slow for this. `ktestd` flushes the recorder on a 200 ms ticker (`time.NewTicker(200 * time.Millisecond)` in `loop.run`) and batches records into compressed DATA frames. It also cannot say "complete up to time T".

## 2. Decisions already taken

Do not reopen these without asking the user.

1. **Two processes, not one.** The master treats the secondary as an instrument, through a peer driver.
2. **Declared interaction only.** The master reaches the secondary only through modes, actions and monitors declared in a contract file, `dut-interface.json`. Never through raw frames or arbitrary channels.
3. **Commands over the existing REST interface.** The master takes a lease on the secondary under its own token.
4. **Results over a new subscription stream.** It carries Logb records, flushed per record, filtered by channel, with a watermark.
5. **Monitors are expressions**, evaluated on the secondary. Lua monitors are deferred until a real state machine or protocol needs one (see §2.1, C9).
6. **One expression grammar** for monitors, limit expressions and procedure conditions. The logic operators are `and`, `or`, `not`, not `&&`.
7. **Procedures are written in a strict subset of Robot syntax**, run by a Go executor in the master. There is no Python. Algorithms are library keywords in Go.
8. **New needs become a new library keyword, or a new parameter** with a backward-compatible default. They never become new syntax.
9. **The secondary runs DUT operation itself.** The master only changes modes. A link loss must not stop the DUT.
10. **Every interaction is recorded.** This covers procedure steps, mirrored monitor transitions, the clock offset, the watermark and the secondary's provenance.

### 2.1 Decisions taken in review (2026-11-19)

These resolve where this handoff contradicted the outline, the manual or the code. They are as binding as the list above.

| # | Decision |
|---|---|
| C1 | **Robot-subset procedures are ktestd's in-daemon scripting**, replacing Lua as the outline's milestone 5. A validated procedure wants bounded loops, so `WHILE` without a limit is refused; "ramp until it fails" is the `Threshold Search` keyword. Outline §9 is rewritten to say so. |
| C2 | Logic operators are `and`, `or`, `not`. The `&&` examples in outline §9.2 and manual §1.5 are corrected. |
| C3 | Cyclic frames held by the daemon's internal holder (started by a mode) survive a lease expiring. Frames a client starts still stop when its lease lapses. Operator STOP and `POST /disarm` stop both. Outline §7 and the manual are updated. |
| C4 | Contract inputs name decoded streams or their fields. A raw CAN ID is not an input. |
| C5 | Channel names are what the code writes: `psu1.ch1.meas.v` (stream `psu1.ch1.meas`, field `v`), decoded streams named per DBC message. Outline §4 is corrected. |
| C6 | `Evaluate` returns `nok`, not `deviated`. |
| C7 | The "R8" reference is dropped. |
| C8 | Fixtures come from `examples/supply-bench`. |
| C9 | DUT actions are Robot keywords in a resource file named by the contract, run on the secondary by the same executor. No `gopher-lua`. Lua monitors wait. |
| C10 | **Recovery is classified by ISO 16750-1's functional status, A to E** (decided 2026-11-19), the newer standard, replacing the Roman classes I–V of the earlier drafts. A: every function as designed, during and after. B: every function works during, some beyond tolerance, all return by themselves. C: some function does not work during, all return by themselves. D: some function needs a simple reset to return. E: needs repair. The contract's enum is `"class": ["A", "B", "C", "D", "E"]`. `Classify Recovery`'s rules are in §4.8. |

Adjustments taken with them:

- **Name resolution.** Field names contain dots (`v.set`), so a reference resolves to the longest declared stream name that prefixes it, then a field. A reference that splits more than one way is refused.
- **Robot conditions** accept `${x}` as well as `$x`.
- **BuiltIn subset:** `Log`, `Fail`, `Set Variable` (local scope only), `Should Be True` (condition profile). These are keywords, not syntax.
- **`WHILE limit=`** takes a count or a duration. One of them is mandatory.
- **Robot parse check:** a test runs `robot.api.get_model` over every procedure fixture, skipped when Robot is not installed. Python is a test-time dependency only.
- **`/stream/sub`** is one third writer (uncompressed, one record per DATA frame, flushed per record) into its own hub. Subscribers filter frames by stream id, preamble included.
- **Internal record subscription** taps typed records before encoding, through a bounded queue. On overflow the evaluator writes `mon.gap` and holds every monitor `unknown` until it has caught up.
- **Evaluator time** advances only by explicit `Advance(t)` events: a timer online, the recorded `mon.watermark` in replay.
- **Two-rack demo:** the master runs on its own `vcan1`, since `ktestd` takes one bus.
- **`procedure.step` arguments** go in a fixed-width string with a truncated flag.
- **Timers:** a second 10 ms ticker in `loop.run` for rules and the watermark; the 200 ms flush tick stays.

## 3. Repository facts to build on

| Fact | Where |
|---|---|
| Module path `github.com/trukeio/ktest`; Go 1.25; Logb checked out at `../logb` | `go.mod` |
| Single event loop: requests, BCM events, supply and scope events, a 200 ms flush tick | `cmd/ktestd/main.go`, `type loop`, `loop.run` |
| Leases, arming, limits, control decisions recorded as records | `loop.handle`, `loop.admit`, `loop.record`, `record/` |
| Live fan-out of recorder bytes, backpressure, disconnecting slow consumers | `stream/hub.go` (`Hub.Write`, `Hub.Subscribe`), `stream/http.go` |
| Second uncompressed writer for the rack stream | `cmd/ktestd/tee.go` |
| Strict loading: unknown keys are errors, refusal to start on a doubtful configuration | `cmd/ktestd/panel.go`, limits loading in `main.go` |
| Driver pattern: a driver is a conversation; policy lives in the daemon | `psu/`, `scope/`, `cmd/ktestd/supply.go`, `cmd/ktestd/scope.go` |
| Simulators with transcripts, and `logbcheck` to hold a recording to them | `psu/sim.go`, `scope/sim.go`, `cmd/psusim`, `cmd/scopesim`, `cmd/logbcheck` |
| Provenance in recordings | `record/provenance.go` |
| Planned scripting (superseded by §2.1 C1): Lua via `gopher-lua`; limits via `expr-lang/expr` | outline §9.2 |

The outline's choice of `expr-lang/expr` conflicts with decision 6: unit literals (`30ms`), load-time unit checks and time functions over a channel's history. Resolve this in milestone 1 (see §6, M1).

## 4. Specification

### 4.1 Expression grammar

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
ref      = name { "." name } | "$" name { "." name }
call     = func "(" [ expr { "," expr } ] ")"
func     = "abs" | "min" | "max" | "age" | "delta" | "rate" | "mean" | "stable"
```

- **Units** are SI with prefixes: V, A, W, Ohm, Hz, s, dB, °C, %. A channel's unit comes from its schema, for example a DBC signal's unit. Dimensions are checked at load: comparing or adding mismatched dimensions is a load error. A bare number takes the unit of the other operand, so `abs(Speed - 3000) <= 30` is valid for a signal in rpm.
- **Names are resolved at load.** An unknown name is a load error.
- **Ordered enums** are declared in the contract. `<` and `>` on enum values compare by declared order.
- **Time functions** keep bounded state per call site:
  - `age(x)`: time since the last sample of x.
  - `delta(x)`: difference between the last two samples.
  - `rate(x, w)`: events per window.
  - `mean(x, w)`: mean over the window.
  - `stable(x, w)`: true if x has not changed for w.
- **Profiles** say which parts each use may have:

| Profile | Time functions | Arithmetic | Used by |
|---|---|---|---|
| `monitor` | yes | yes | contract monitors |
| `limit` | yes | yes | future limit expressions |
| `condition` | no | no | procedure `IF` / `WHILE` |

- The grammar has no assignment, no other functions and no side effects.

### 4.2 Contract file: `dut-interface.json`

```json
{
  "contract": "acme-bcm-v3",
  "schema": 1,
  "inputs":   {"Vx": "psu1.ch1.meas.v", "Speed": "EngineData.EngineSpeed", "Alive": "DutStatus"},
  "monitors": {
    "vx":    {"ok": "Vx > 1.0V"},
    "speed": {"ok": "abs(Speed - 3000) <= 30", "debounce": "20ms", "modes": ["active"]},
    "alive": {"ok": "age(Alive) < 30ms and delta(Alive.counter) == 1"},
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

- Load strictly, like `panel.json`: unknown keys, unknown inputs, cycles between monitors and unit errors all refuse the load.
- The contract hash is the sha256 of the file bytes. It is recorded by both daemons.
- A monitor named `abort` is reserved. When it goes `dev`, the secondary is asking the master for protection.

### 4.3 Monitors (secondary)

- Each monitor is a held channel `mon.<name>` with the values `ok`, `dev`, `unknown`, `inactive`, `settling`.
- **When it evaluates:** on each new sample of any input, and at the deadlines implied by `age()`.
- **Timestamp:** a transition carries the timestamp of the triggering sample, or the computed deadline. Never the time of evaluation.
- **Input staleness:** each input has a maximum age, by default 3 × its observed period. If any input is older than that, the monitor is `unknown`.
- **Debounce:** the condition must be false for at least `debounce` before `dev`, and true for `debounce` before returning to `ok`. The default is 0.
- **Modes:** outside its listed modes a monitor is `inactive`. During a mode's `settle` time it is `settling`.
- **Purity:** the same input history always gives the same output. Write a test for exactly this.
- **Isolation:** the evaluator runs in its own goroutine, fed by an internal subscription. It must never block the event loop. If it falls behind, the watermark stops advancing. Do not drop inputs silently.
- **Watermark:** a held channel `mon.watermark`, emitted every 10 ms (configurable). Its value T means that all transitions with timestamp ≤ T have been emitted.

### 4.4 Subscription stream (secondary)

- `GET /stream/sub?ch=<glob>[,<glob>…]` over the existing HTTPS listener, with the existing token roles (`view` is enough).
- **Contents:** Logb, uncompressed, one record per DATA frame, flushed per record. It opens with the schemas and current held values of the matching channels, like `/stream/live`.
- **Always included:** `mon.watermark`, `mode.*` and the secondary's provenance.
- **Unchanged:** the disk recording and `/stream/live` keep their batching.
- **Latency target:** sample → transition on the wire under 5 ms at the 99th percentile on loopback. Measure it; don't assume it.

### 4.5 Modes and actions (secondary)

- `PUT /dut/mode {"value":"active"}` runs the mode's entry action, sets `mode.applied`, and makes the mode's monitors `settling` for its settle time.
- `POST /dut/actions/<name>` runs a declared action (a keyword of the contract's resource file), one at a time, under the caller's lease.
  - The answer is accepted or refused, as everywhere in Ktest.
  - The outcome arrives on the stream as `dut.action.<name>` records: start, end, result.
- **Lease scope:** the lease guards modes and actions only. DUT cyclic frames started by modes belong to the secondary itself and survive the master's lease expiring (decision 9). This differs from today's rule that lease expiry stops cyclic transmit. Implement it as frames owned by the daemon's own internal holder, not by the client's lease.
- **Action scope (C9):** actions are keywords in the contract's `resource` file, run by the procedure executor of M2 with a timeout. The secondary's library adds keywords for send, cyclic, set channel and wait for monitor; `Settle` is the sleep. No other keywords.

### 4.6 Peer driver (master)

```
-peer dut=https://host:8443,token=dut.token,cacert=dut.pem,contract=dut-interface.json
```

- **On connect:**
  1. Fetch the secondary's provenance and contract hash.
  2. Refuse to start if the hash differs from the local copy, or if the procedure names a mode, action or monitor that the contract lacks.
- **Mirroring:**
  - Subscribe to `mon.*` and `mode.*`.
  - Mirror each record into the master's recording as `dut.<name>`, keeping the secondary's timestamp and adding the master's receive time.
  - Record `dut.watermark`, and `dut.connected` (held).
- **Clock offset:**
  - Estimate it from REST round trips, NTP-style: a minimum-RTT filter over the last N samples, every 1 s.
  - Record it as `dut.clock.offset`, together with its uncertainty (half the RTT).
  - Express all procedure windows in master time, with the offset applied.
- **Lease:** hold a lease on the secondary and renew it like any client.
- **No policy:** the driver holds no policy. Daemon rules decide what a stale watermark means.

### 4.7 Daemon rules (master)

A new `rules` section in `limits.json`, read at start and not changeable by any client:

```json
"rules": {
  "monitoring_lost": {"watermark_age": "100ms", "action": "remove_stress"},
  "dut_abort":       {"monitor": "abort", "action": "remove_stress"},
  "remove_stress":   ["psu1", "gen1"]
}
```

- **`remove_stress`** means disarming the listed instruments with outputs off. That is the same path as STOP, restricted to the list.
- **Every firing is recorded** as a control decision, with its cause.
- **When they are evaluated:** in the event loop, on each mirrored record, and on a 10 ms timer for watermark age.
- **Procedure:** a running procedure is failed with the cause `monitoring_lost` or `dut_abort`.

### 4.8 Procedures (master)

**Syntax: a strict subset of Robot.** Files must parse under Robot's own parser, but they run only here.

- **Sections:** `*** Settings ***`, `*** Variables ***`, `*** Test Cases ***`, `*** Keywords ***`.
- **Settings:** `Resource <file>`; `Variables <file.json|yaml>`.
- **Keyword calls:** cells separated by two or more spaces or a tab. Positional and `name=value` arguments. `${x}=` captures a result.
- **Variables:** `${scalar}`, `@{list}`, `&{dict}`, and field access `${v.field}`.
- **Settings in tests and keywords:** `[Arguments] [Setup] [Teardown] [Documentation] [Tags] [Timeout]`.
- **Control flow:** `FOR … IN` and `IN RANGE`; `IF / ELSE IF / ELSE / END`; `WHILE … limit=<n>`, where the limit is mandatory; `BREAK`, `CONTINUE`, `RETURN`.
- **Conditions:** use the grammar of §4.1 with the `condition` profile.
- **Refused:** `Library`, `Evaluate`, `${{…}}`, `TRY`/`EXCEPT`, `GROUP`, templates and the `Set * Variable` scopes. Refuse them with a sentence that names the construct and its line.

**Load-time checks.** All of these refuse the load, before anything runs:

- every keyword is known;
- every argument name is valid;
- units fit;
- every variable is defined;
- every contract name exists;
- no keyword is recursive.

**Five operations underneath every DUT keyword:**

| Operation | Behaviour |
|---|---|
| `SetMode(m)` | PUT mode, then wait for `mode.applied == m` and the settle time |
| `Invoke(a)` | POST action, then wait for its result record |
| `WaitFor(cond, timeout)` | wait until a condition over `dut.mon.*` holds |
| `Evaluate(monitors, t0, t1)` | wait until `dut.watermark ≥ t1`, then return `nok` if any listed monitor was `dev` (or `unknown`) at any time in [t0, t1] |
| `Snapshot(channels)` | record the current values |

**Outcomes:** each operation returns one of `ok`, `nok`, `timeout`, `refused`, `link_lost`.
- `nok` is a value.
- `timeout`, `refused` and `link_lost` fail the keyword. The test case stops and `[Teardown]` runs.

**First keyword library:**

| Keyword | Purpose |
|---|---|
| `DUT Prepare    mode    ready=<monitor>    timeout=` | prepare the DUT |
| `DUT Set Mode`, `DUT Invoke`, `DUT Wait For`, `DUT Evaluate    monitors    window=` | the DUT operations |
| `Verify Baseline    monitors    window=` | fails unless every monitor is `ok` |
| `Settle    duration` | wait a fixed time |
| `Record Step    point    level    result    [threshold=]` | record a step result |
| `Classify Recovery    monitors    function=    action=    timeout=` | returns a class from the enum, using the rules below |
| `Threshold Search    point    monitors    target=    nominal=    start=    step=    floor=    duration=    recover=    action=    [method=descending]` | find the threshold (the full signature is M7's) |
| `Mark Time` | the master's clock, for `since=` |
| `DUT Snapshot    monitors` | the monitors' states as last mirrored, as text |
| `Stress Apply`, `Stress Remove` | thin wrappers over the lab instruments named in the procedure's variables; start with the supply driver |

`Classify Recovery` rules (C10, ISO 16750-1). `monitors` are all the monitors judged; `function=` names those among them whose deviation means a function did not work, such as `alive`. The others' deviation means a function worked beyond its tolerance, such as `speed`.
- A: no listed monitor deviated.
- B: only monitors outside `function=` deviated, and all are back to `ok` without the action, within the timeout, after the stress is removed.
- C: a `function=` monitor deviated, and all are back to `ok` without the action, within the timeout.
- D: back to `ok` only after the action.
- E: not back after the action. The software cannot see whether a repair is needed, so E says only that the DUT did not return; an operator confirms it.

**Recording:**
- every keyword call is a `procedure.step` record: id, file:line, keyword, arguments, outcome, t_start, t_end;
- each run starts with a `procedure.run` record: the procedure hash, the library version, the contract hash and the mode (locked or engineering).

**Running:**
- `POST /procedures/run {"file":"…","test":"…"}` starts a run under the caller's lease. It is refused if a run is already active.
- `DELETE /procedures/run` stops the run; `[Teardown]` runs.
- The run lives in the daemon and survives the client disconnecting.

### 4.9 Locked mode (master)

- `-locked hashes.json` lists the sha256 of the binary, the library version, the procedure files, `limits.json` and the instrument configuration.
- At run start, if any hash differs, refuse the run (default) or mark it `"accredited": false`, according to the file.
- Record the mode in `procedure.run`.

### 4.10 `monitor-check` tool

```
monitor-check [-json] dut-interface.json recording.logb
```

- Replays a recording through the same evaluator, using the same package, offline.
- Prints every transition.
- `-expect ok` exits non-zero on any `dev`.
- `-expect dev@t1,t2,…` checks that `dev` occurs within a tolerance of each given time.

## 5. Conventions to keep

- No cgo. One static binary per program. Linux for `ktestd`.
- A configuration that would do nothing, or something doubtful, is refused at start with a sentence. Unknown keys are errors.
- Commands are answered with accepted or refused and a sequence number, never with state. Results arrive on the stream.
- Every decision, refusal included, is a record with who asked and through which listener.
- Gaps are records. Never infer "nothing happened" from silence.
- Simulator first. Every new behaviour gets a simulator or a recorded fixture, and a `logbcheck`-style independent check where it makes sense.
- Comments explain why, as in the existing code. Keep the house style of `stream/http.go`.
- Update `doc/manual.html` and the outline when a milestone lands: mark sections built, planned or proposed.

## 6. Plan

Each milestone is usable and tested on its own. Do them in this order. Stop after each one, report, and let the user review before going on.

### M1: package `expr`

- **Scope:**
  - Lexer and parser for §4.1, with units and dimension checking, the three profiles, and enum ordering.
  - Evaluation over a typed environment.
  - Time functions with bounded state and an explicit clock: time comes from sample timestamps, never from `time.Now`.
- **Decide first:** is `expr-lang/expr` plus a units pre-pass enough for §4.1 (unit literals, load-time dimensions, `age()` semantics)? Write half a page in this file with the reason for the choice. The default expectation is a small hand-written parser.
- **Tests:**
  - table tests for precedence and units;
  - refusal tests that check each error sentence;
  - property tests: the same input sequence gives the same output;
  - benchmarks: one evaluation under 1 µs for a typical monitor.
- **Done (2026-11-19).** Package `expr`, no dependencies.

#### M1 decision: a hand-written parser, not `expr-lang/expr`

`expr-lang/expr` is a good language for what it was built for, which is business rules over Go values. Three parts of §4.1 sit outside it, and each would be bolted on rather than built in:

- **Unit literals.** `30ms` and `1.0V` are not tokens to `expr-lang`. A pre-pass that rewrites them into numbers has to lex the expression itself, so it is a second lexer with its own idea of strings and names, kept in step with the real one. Errors in it point at a column of a text the user never wrote.
- **Load-time dimensions.** `expr-lang` checks types against Go types. A voltage and a time are both `float64` to it, so `Vx > 30ms` type-checks. Dimension checking needs the typed tree, which means walking `expr-lang`'s AST with our own checker. That is most of a compiler. The bare-number rule (a bare `30` takes the unit of what it meets, rpm included) is a rewrite of that tree on top.
- **Time functions with state per call site and an explicit clock.** `expr-lang` functions are stateless Go functions over values. `age(x)` needs x's sample history, not its value, and must know when its answer will change (§4.3: transitions at deadlines). That needs the call site to be a node the compiler knows and the evaluator owns, and it needs age compared with a constant to be recognised, so that the deadline is exact to the nanosecond. In `expr-lang` all of that would live in a side table keyed by AST position.

Smaller points weigh the same way: `expr-lang` offers `&&` beside `and`, `??`, pipes, closures, `let`, and member calls on Go values, and each would have to be refused one by one so that the three profiles hold. Its error sentences are its own, while §5 wants every refusal to say what to do.

What the hand-written package costs: about 1 700 lines, plus 500 of tests, and owning a grammar. The grammar is closed by decision 8, so that cost is paid once. What it buys: one lexer whose errors point at the user's own text, dimensions checked while the tree is built, deadlines that come out of the compiler, and no dependency. Measured on an i7-6700: 32 ns for `Vx > 1.0V`, 59 ns for the `alive` monitor, 225 ns for a monitor with two 50–100 ms windows, all with no allocation.

Choices made while building it, for review:

- **`delta(x, m)`.** An optional modulus, so that a rolling counter going from 15 to 0 has moved by one. Without it the contract example's `alive` monitor would deviate on every wrap. It is a new parameter with a compatible default (decision 8).
- **`age()` must be compared directly with a constant** (`age(x) < 30ms`). That is the form whose moment of change is known exactly. Anywhere else it is refused with a sentence.
- **`rate(x, w)` is a count** of the samples in (now−w, now], with no unit. **`mean(x, w)`** is the mean of the samples, not over time: it changes only when a sample enters or leaves the window, so its deadlines are known.
- **Windows are bounded** at 4096 samples per call site. A window that overflowed answers `unknown` until the lost samples have left it, rather than answering from part of the window.
- **Unknown is a value.** An input nobody has heard from, a division by zero, a window that is not whole: each is unknown. Logic follows Kleene (false and unknown = false), so a monitor goes `unknown` rather than `dev` on missing data.
- **Every expression is a predicate**, in all three profiles. A bare bool name is allowed (`ok`); a bare number is refused.
- **Units.** V, A, W, Ohm/Ω, Hz and s take SI prefixes p n u/µ m k M G. dB and °C are dimensions of their own and take no prefix. % has no dimension and is 0.01. A unit a schema states that this list lacks (rpm, B) becomes a dimension of its own. A literal may only carry a listed unit, so `3000rpm` is refused and is written bare.
- **The condition profile** allows a negative literal (`-12V`) but no other arithmetic and no calls.
- **Names** come from a `Resolver`. Variables are resolved with their dollar (`$level`, also written `${level}`). The longest-prefix rule for `stream.field` belongs to the resolver that knows the schemas, and arrives with M3.

### M2: procedure executor on a single rack (master)

Moved forward by §2.1 C1: the executor is ktestd's scripting, useful before a second rack exists.

- **Scope:** the single-rack part of §4.8, in package `procedure`.
  - Robot-subset parser, every refusal with its sentence, every load-time check.
  - BuiltIn subset, `Settle`, `Record Step`, and `Stress Apply` / `Stress Remove` over the supply driver.
  - `IF` / `WHILE` conditions over the master's own held channels, through the internal record subscription (§2.1), which M5 reuses.
  - `POST` / `DELETE /procedures/run`; `procedure.run` and `procedure.step` records; the run survives the client disconnecting.
- **Demo:** a supply ramp procedure in `examples/supply-bench`.
- **Tests:**
  - every refused construct and every load-time check;
  - the Robot parse check over the fixtures;
  - a run survives its client disconnecting; `DELETE` runs `[Teardown]`.
- **Done (2026-11-19).** Packages `procedure` (parser, loader, executor, BuiltIn subset) and `procedure/lib` (rack keywords), `ktestd -procedures`, `POST`/`DELETE /procedures/run`, and the streams `procedure.run`, `procedure.step`, `procedure.result`. `record.Config` gained `OnStream` and `OnRecord`, which are the internal record subscription: they see records before encoding, on the writer's goroutine. M2 reads the latest value per channel from them, and M5 will feed the evaluator from the same hooks.

#### M2: what was built, and choices for review

- **How a run holds instruments.** §4.8 says a run starts "under the caller's lease". The caller's lease would lapse when the caller disconnects, so the run takes leases itself instead, for the caller's name. The first time it writes an instrument it takes the lease and arms it; it renews every 10 s and releases at the end. Every step is an ordinary control decision with `via` 3 (`record.ViaProcedure`) and `by` set to the starter's name. `logbcheck` holds these to the starter's role and lease, as it holds TLS requests. A consequence: an instrument the starter holds in their browser is "held by another" for the run, and the run fails `refused`. Release it first.
- **A stop wins.** If someone stops the rack while a run is going, the run does not re-arm; its next write is refused and the run fails `refused`.
- **Static typing of variables.** A variable is typed at load from its literal value, a keyword's declared return, or, for a keyword's arguments, each call site: a user keyword is checked once per distinct set of argument types, with the error naming the call (`(called from p.robot:4)`). A condition over a variable whose type is not known at load is refused. A keyword nobody calls is still checked; conditions over its untyped arguments wait for a call.
- **Robot semantics kept:** cell text is a string until a keyword parameter converts it; `${x}` alone keeps its value's type; a value in a condition is read the way Robot's substitution would read it (`12V` is a number in volts, `True` a truth value, anything else text). Variables are local. `IF` defines a variable after it only if every branch, ELSE included, does. A loop defines nothing after it.
- **A cell `name=value` is always a named argument**, and an unknown name is refused. Text containing `=` is written `name\=value`. This is stricter than Robot, so that "every argument name is valid" holds.
- **New keywords**, under decision 8: `Wait Until    condition    timeout=10s    poll=10ms`, `Read Channel    name`, `Should Be Within    actual    expected    tolerance`. `Stress Apply` gained `current=`: psusim, like a real supply, starts with a 0 A current limit, so a stress without one measures 0 V. `Keyword.CheckArgs` checks at load that arguments agree with each other, such as the three units of `Should Be Within`.
- **`expr` accepts `min` and `h`** for durations (`Settle    2h`). They are not SI, but they are accepted alongside it.
- **Not done in M2:** YAML variable files, which are refused with a sentence (JSON works), and the rack UI for procedures (out of scope, §8).
- **Measured live** on `vcan0` with `psusim`, over TLS and the local socket: a full ramp; a second start refused while the first runs; a refusal by the limits (14 V); a stop mid-ramp with the teardown run; a run ended by the daemon stopping; and a 40 s run outliving the 30 s lease term through renewal. `logbcheck -control -psu` passes on the recording: every control decision, the run's included, keeps to the limits and roles, and every supply write matches psusim's transcript.

### M3: package `monitor`, the contract loader and `cmd/monitor-check`

- **Scope:** §4.2, §4.3 (offline part) and §4.10.
- **Fixtures:**
  - record a run of `examples/supply-bench`;
  - add a DUT simulator (M4) recording later.
- **Tests:**
  - debounce at its edges;
  - staleness to `unknown`;
  - the `inactive` and `settling` states;
  - deadline timestamps from `age()`;
  - replay determinism.
- **Done (2026-11-19).** Package `monitor` (contract loader and evaluator), package `channel` (stream.field resolution, moved out of `ktestd` so that the daemon and `monitor-check` share it), and `cmd/monitor-check`. The fixture is a `ktestd` recording of the supply bench running its ramp procedure: `cmd/monitor-check/testdata/supply-ramp.logb`.

#### M3: choices for review

- **A stale input makes its values unknown, not the monitor.** §4.3 says "if any input is older than that, the monitor is `unknown`". Taken literally, the contract's own `alive` monitor (`age(Alive) < 30ms and delta(Alive.counter, 16) == 1`) would say `dev` at 30 ms after the last message and `unknown` a moment later, so a DUT that stopped talking would read as unknown. Instead, a stale input's values go unknown (`expr.Program.Forget`) and the expression answers by Kleene's logic. `Vx > 1V` with a stale `Vx` is still `unknown`; `age(Alive) < 30ms and …` stays `dev`. **Accepted by the user, 2026-11-19.**
- **Held inputs never go stale.** A held field's value stands until its next record (the outline's §4), so there is no period to miss. "3 × its observed period" applies to sampled inputs, as 3 × the median of their last 8 intervals. A sampled input seen once has no period yet, so its value is not used until the second sample. `max_age` overrides the period rule; it is refused on a held input.
- **Inputs may be objects:** `"Alive": {"channel": "DutStatus", "max_age": "50ms", "enum": "class"}`. A plain string still works. This extends the §4.2 file format.
- **A stream named whole is an event.** `"Alive": "DutStatus"` names a decoded message. `age(Alive)` and `rate(Alive, w)` read its arrivals; `Alive.counter` reads a field; `Alive` alone, as a value, is refused (`expr.Event`).
- **The resource is hashed too.** The contract names action keywords; what they do is in the resource file. `Contract.ResourceHash` is recorded beside the contract hash, so that a changed resource is not hidden behind an unchanged contract hash. M6's hash check should compare both.
- **Instants.** A sample at the same nanosecond as a deadline is applied first, so there are no zero-length deviations. A deviation has to outlast its debounce with no sample at its end saying otherwise. A transition at a deadline carries the deadline's time.
- **Monitors reading monitors** see `true` while `ok`, `false` while `dev`, and unknown otherwise, `inactive` and `settling` included. Monitors are evaluated in dependency order, so a dependent changes in the same instant. Only monitors that list a mode settle when it is entered.
- **`monitor-check -expect dev@…`** also fails on a deviation the list does not name, so that the list is the whole account of the deviations. Times are seconds from the recording's first record. The mode comes from a text channel, `mode.applied` by default, which M5 will write; a contract with modes over a recording without one is replayed with no mode, and says so.

For M5, two things this milestone found:

- **Order.** The evaluator takes records in time order across streams and refuses an earlier one. Offline, `monitor-check` sorts. Online, records from different clocks (kernel stamps on the bus, userspace for instruments) can reach the recorder slightly out of order, so M5 needs a short reorder window in front of the evaluator, and the watermark stands at the window's trailing edge.
- **DBC streams are declared lazily.** The recorder declares a decoded message's stream the first time the message is seen, so a contract loaded at start cannot resolve `EngineData.EngineSpeed` from the recorder's streams. M5 resolves those against the database's schemas at start.

### M4: `cmd/dutsim`, a simulated DUT

- **What it simulates:**
  - On a CAN interface (vcan), it sends an alive message every 10 ms, with a counter, and a speed signal.
  - It reads its "supply voltage" by polling a `psusim` channel over SCPI. That supply is the lab rack's stress source in the demo.
- **Configurable behaviour below a threshold voltage:**
  - glitch only (functional status B, C10);
  - stop until the voltage is restored (C);
  - stop until a reset frame is received (D);
  - never recover (E).
- **Other:** it keeps a transcript for checks.
- **Why it comes now:** it makes the whole two-rack setup testable without hardware, on one machine.
- **Done (2026-11-19).** Package `dut` is the DUT as a state machine with no clock and no bus (times come in with every event), and its frames' codec. `cmd/dutsim` connects it to a CAN socket and a psusim poller. The frames are in `dut/dutsim.dbc` (`DutStatus.counter`, `EngineData.EngineSpeed`, `DutReset.Request`), which is embedded in dutsim and given to `ktestd -dbc`. A second `monitor-check` fixture is a ktestd recording of dutsim through a 300 ms dip to 6 V (`cmd/monitor-check/testdata/dutsim-dip.*`), and its test holds `alive`'s transitions to dutsim's transcript.
- **Measured live** on `vcan0` with psusim and ktestd, using the `restore` and `reset` behaviours (for `reset`, the reset frame was sent with `cansend vcan0 700#01`). `logbcheck -control -psu` passes on the `reset` recording against a candump of the bus: 361 frames byte for byte, 16 control decisions, 10 supply writes against psusim's transcript.

#### M4: choices for review

- **Behaviours are named, not numbered.** dutsim says `-behaviour restore` rather than a class. *Corrected after review:* this note first said that ISO 7637-2 numbers the reactions one higher than the plan did. That mixed two schemes. ISO 7637-2's function performance status has four statuses, I–IV, under which the plan's numbering was right. The five-way split is ISO 16750-1's A–E. **Decided (C10): ISO 16750-1.** `glitch` is B, `restore` C, `reset` D, `never` E.
- **What the DUT does at the edges:**
  - The first boot is always clean, whatever the behaviour: a power-on is a reset.
  - A dip during a boot restarts the boot for every behaviour, `never` included, since the DUT was not yet running.
  - A reset frame reboots a hung DUT, and a running one too. It is ignored while the supply is low, and the transcript says so.
  - How deep a dip goes does not matter: `glitch` tolerates 0 V.
  - `-vrestore` (default `-vmin` + 0.5 V) is the hysteresis.
  - The counter restarts at 0 on every boot.
- **The supply belongs to the other rack.** dutsim does not use `psu.Dial`, because that sends `*CLS` and would clear the error queue the master's daemon reads. It only sends `MEAS:VOLT?`. These queries fill psusim's transcript (200 a second at the default 5 ms poll), and `logbcheck` skips them.
- **A supply that stops answering stops dutsim**, with an error and a transcript line. dutsim does not guess the voltage.
- **`-poll` slower than `-period` is refused**, so that a dip the DUT should react to is not missed between readings.
- **A userspace ticker, not CAN_BCM.** The counter changes with every frame. The jitter is well inside `age(Alive) < 30ms` on vcan.
- **Reading the recording's epoch.** A ktestd recording's axis counts from its epoch. The recording states the epoch only as the first sync frame's wall time, which logb calls a coarse hint (ktestd writes it exactly), and inside decoded streams' namespaces. The fixture test reads the sync frame. M7's check of verdicts against the transcript needs the same thing, so a `recording.epoch` metadata key would be the honest way to state it.

For M5 and M7, three things this milestone found:

- **The DUT sees a dip before the rack does.** dutsim polls every 5 ms, while ktestd measures the supply every 100 ms. In the fixture, `alive` goes `dev` about 34 ms before `vx` does. Any rule or keyword that compares the DUT's reaction with the stress has to allow for the stress measurement's period.
- **`alive` can pass through `unknown` on its way to `dev`.** Staleness (3 × the observed 10 ms period) and the `age(Alive) < 30ms` deadline fall within a fraction of a millisecond of each other. When staleness comes first, `delta(Alive.counter)` goes unknown while `age` is still true, so the monitor shows `unknown` for about 0.3 ms before `dev`. That is correct Kleene logic, as accepted in M3, but a rule that acts on `unknown` would see it.
- **After a reboot, `alive` goes `dev → unknown → ok`.** A sampled input seen once has no value yet (M3), so the first frame gives `unknown` and the second gives `ok`.

### M5: secondary online (monitors, watermark, `/stream/sub`, modes, actions)

- **Scope:** §4.3 online, §4.4 and §4.5.
- **Wiring:**
  - feed the evaluator from the recorder's record path, not the encoded bytes;
  - add an internal record subscription if there is none;
  - emit monitor records back through the loop, like any other source.
- **Tests:**
  - an end-to-end latency measurement on vcan with `dutsim`, reported as numbers in this document;
  - the watermark is monotonic;
  - a stalled evaluator stops the watermark;
  - a slow subscriber is disconnected without affecting the recording;
  - lease expiry leaves the mode's cyclic frames running.
- **Split in two.** M5a is the monitors online: §4.3 and §4.4. M5b is modes and actions: §4.5, the secondary's keyword library and cyclic frames held by the daemon. The lease-expiry test belongs to M5b.

#### M5a: monitors online and `/stream/sub` (done 2026-11-19)

- **Built:**
  - `ktestd -contract dut-interface.json`.
  - `monitor.Online` and `monitor.Tap`, the online evaluator and the loop's non-blocking side of it.
  - The streams `mon.<name>`, `mon.watermark` and `mon.gap`.
  - A third writer behind `GET /stream/sub?ch=…`, with the hub filtering by stream.
  - `monitor-check -live` and `cmd/sublat`.
  - `recording.epoch` in every recording's metadata.
- **Tests:**
  - online reordering gives what the offline evaluator gives;
  - the watermark is monotonic over 5000 random items, and never ahead of the clock less the window, or of a transition;
  - a late record is a gap;
  - a full tap is a gap, and a lost tick is not;
  - a stalled evaluator stops the watermark, and its queue fills into a gap without blocking the tap;
  - a filtered subscriber gets only its streams, joining early or late, and a slow one is dropped while the recording stays whole;
  - `TestLiveIsReplay`: a ktestd recording (`cmd/monitor-check/testdata/rack-live.logb`) whose live transitions are the replay's, to the nanosecond, and a transition moved by 1 ns is caught.
- **Measured live** on `vcan0`. The setup was dutsim with `-behaviour restore` behind psusim, `ktestd -contract` with `dutsim.json`, and a procedure dipping the supply 100 times, with `sublat` subscribed over HTTPS on loopback:

  | Run (65 s each) | Transitions | p50 | p90 | p99 | max | Watermark p50 / p99 | Gaps |
  |---|---|---|---|---|---|---|---|
  | 1 | 709 | 3.21 ms | 3.68 ms | 4.45 ms | 9.96 ms | 2.44 / 3.29 ms | 0 |
  | 2 | 717 | 3.20 ms | 3.81 ms | 4.84 ms | 9.84 ms | 2.43 / 3.30 ms | 0 |

  In both runs, every transition the rack recorded is the replay's, to the nanosecond (710 and 718), and `logbcheck -control -psu` passes on a rack recording. Shorter runs with other settings, about 150 transitions each:
  - a 1 ms window: p50 2.1 ms, with two late-record gaps in one of two runs;
  - a 0.5 ms tick: no gain;
  - `-codec none`: no change. The 200 ms zstd flush is not the tail.

#### M5a: choices for review

- **The monitors' clock is the last moment the loop was caught up**, not the time a record is written. That moment is when its input queues and both CAN sockets were empty, and the sockets are asked with a zero-timeout `poll` (`can.Conn.Pending`; `SIOCINQ` gives ENOTTY on AF_CAN). Two measured failures led here:
  - With the clock read at write time, after any pause Go's `select` handled a tick or a fresh frame before a supply reading already queued. The watermark passed the reading, which was then late (2 gaps in 6 s).
  - With only the Go queues checked, frames still in the kernel's socket after a pause were stamped and invisible (1 gap in 65 s).

  With the sockets checked, there were 0 gaps in 130 s.
- **A record behind the watermark is a gap**, the same as a lost one. Every monitor goes `unknown` at the watermark, the evaluator starts again from nothing, and the mode carries over. The transitions already emitted cannot be taken back, and a replay would not match them, so this is the only honest answer. The cost: a 2 ms window has no margin for a real instrument whose reading arrives later than that after its stamp. Such an instrument needs a larger `-mon-window`, and pays for it in latency.
- **A 1 ms tick, not 10 ms.** The loop hands the monitors its clock every `-mon-tick` (1 ms). `mon.watermark` is recorded every `-watermark` (10 ms), as §4.3 says. A 10 ms tick would put the p99 near 13 ms. M6's 10 ms timer for rules is separate.
- **Defaults:** `-mon-window 2ms`, `-mon-queue 8192`.
- **Stream layouts:**
  - `mon.<name>`: `state` (held, text by `ValueToText`, so `mon.alive.state == "dev"` works in a condition), `from`, `cause`. Each monitor's first record is its state at start, `unknown → unknown`, cause `start`.
  - `mon.watermark`: the record's time is the watermark itself, with `upto` repeating it in Unix ns.
  - `mon.gap`: `from`, `to`, `lost`, `reason`, at the watermark where every monitor went unknown.
- **`/stream/sub`:**
  - `ch` globs match stream names, not channels.
  - `mon.watermark`, `mon.gap`, `mode.*` and all metadata are always sent. `mon.gap` was added to §4.4's list, because a watermark is only half the story without it.
  - It carries no waveforms and no acquisitions.
  - Over plain `-http` too, read-only, as `/stream/live` is.
- **The contract is checked before the recording file is created**, against the streams the configuration will produce: DBC messages through `record.DecodedSchema`, supplies, scopes, procedures, the panel. The procedures' channel table gets the DBC streams at start too, so a procedure may read a signal the bus has not carried yet.
- **`recording.epoch`** is new metadata in every recording, and each of the three writers states its own. `sublat` uses it, and M6 and M7 can.
- **`monitor-check` replays the recorded `mon.watermark` as the evaluator's `Advance`**, after the records of the same instant, as it happened live. `-live` compares up to the last watermark and the first gap. A replay sees records the live evaluator lost, so it gives what the rack would have given without the gap.
- **Live streams keep no index (decided 2026-11-19).** `logb.Writer` appended to its index for every DATA frame, for the INDEX it writes at `Close`, and never freed it. The `/stream/sub` writer writes a frame per record, so its memory grew by one entry per record for the daemon's whole life: measured at 2.3× peak RSS in a 6 s `cangen` flood, and at 2,000 frames/s about 5 GB a day. The rack writer grew the same way, about a hundred times slower. logb gained `Writer.NoIndex`, and SPEC §9 now says a writer may omit INDEX. ktest's `record.Config` gained `Live`, set for the rack and subscription streams. The flood's peak RSS is back to 183 MB.
- **Cost under a flood.** In a 5 s `cangen -g 0` flood (about 250k frames/s, more than any real bus), ktestd records about as many frames as before (1.25M and 1.31M in two runs, against 1.28M), with about 7% more user CPU, because every record is encoded a third time. A hub with no subscriber no longer copies DATA frames.

#### M5b: modes and actions (done 2026-11-19)

- **Built:**
  - The `dut` instrument, with its own lease and arming: `POST`/`PUT`/`DELETE /dut/lease`, `POST /dut/arm`, `POST /dut/disarm`.
  - `PUT /dut/mode {"value": "active"}` and `POST /dut/actions/{name}`.
  - The DUT library, `lib.NewDUT`: `Settle`, `Send Message`, `Start Cyclic`, `Stop Cyclic`, `Set Channel`, `Wait For Monitor`, and the BuiltIn subset.
  - `procedure.LoadResource`, `Suite.Call` and `Suite.Bare`.
  - The streams `mode`, `dut.action.<name>` and `dut.step`.
  - The request codes 13 (mode change) and 14 (action), the verdict 7 (busy) and `via` 4 (a DUT action).
  - `logbcheck` rules for all of these.
- **Tests:**
  - `TestDutResource`: the resource checked at start, and eight refusals, each with file:line.
  - `TestDutSignals`: signal values in the signal's own unit.
  - `TestControlCheckDUT`: the control rules, with eight planted faults.
- **Measured live** on `vcan0`, with dutsim (`-behaviour reset`) behind psusim and a contract with the mode `active` (settle 200 ms) and the actions `enter_active`, `leave_active`, `reset`, `stall` and `hold`. Over the local socket:
  - The mode is refused without the dut lease, and an unknown mode without a number.
  - With a lease and arming, the mode runs its entry action (`Start Cyclic TesterPresent 100ms`) and is applied. `speed` goes `inactive → settling`, then `ok` exactly 200 ms later.
  - A client's own cyclic task on a 2 s bus lease ran 2.1 s and stopped with that lease. The DUT rack's `TesterPresent` ran on, through that lapse and through the dut lease lapsing, for 5.9 s, until `POST /dut/disarm`.
  - With the dut lease lapsed, `reset` is refused. With a new lease it sends one `DutReset`, the hung DUT reboots, and `Wait For Monitor alive` holds. A second action during it is refused as busy.
  - `stall` ends `timeout` after its 500 ms. `hold` ends `stopped` when dut is disarmed.
  - `logbcheck -control -psu` passes, against a candump of the bus (1,637 frames). `monitor-check -live` gives the rack's transitions to the nanosecond, the mode's settling included.

#### M5b: choices for review

- **`dut` is an instrument, leased and armed.** A mode change or action needs both, as a send needs the bus's. Arming is Ktest's rule before anything acts (§7), and the master's peer driver will take the lease and arm.
- **The DUT goes on when its master goes** (decision 9, C3):
  - A lapsed or released dut lease leaves dut armed and its tasks running; until it is leased again, only new modes and actions are refused.
  - A lapsed or released bus lease stops the tasks its holder started, and leaves the DUT rack's.
  - `POST /dut/disarm` and the stop end the action under way and every task the DUT rack holds.

  `logbcheck` checks each of these, and flags a DUT rack's task stopped by a bus lease as an error.
- **An action's requests go to the instrument they act on, with `via` 4.** A send or cyclic task is recorded in `vcan0.control`, a channel write in `psu1.control`, with `by` the name that asked for the action. They are admitted while dut is armed, not under that instrument's lease, and the floor and limits apply. This keeps every instrument's stream the whole account of what was done to it, so the existing supply and cyclic checks hold unchanged. A task belongs to the DUT rack when the decision that last started or changed it came from an action; a client's update to the same identifier makes it the client's.
- **One action at a time.** A mode change or action while one runs is refused, recorded with verdict 7, busy. A mode's entry action is an action: it records `dut.action.<entry>` with the mode, and the mode is applied, with the monitors settling from that moment, only if the action succeeds. Otherwise `mode` records the failure, and `mode.applied` stays as it was. A mode with no action is applied at once. `""` leaves every mode.
- **Timeouts** come from the contract's `timeouts` by action name, 30 s otherwise. A timeout ends `timeout`, and a disarm or stop ends `stopped`, each with a sentence saying which.
- **The resource is checked at start, before anything is written**, with the DUT library, against the database, this rack's channels and the contract's monitors. Every action must be callable with no arguments. This is done on a stand-in daemon with the same configuration, then bound to the real one.
- **Signals are given as a dictionary** from the Variables (`&{RESET}    Request=1`, passed as `${RESET}`), since the subset has no free named arguments. A value is read in the signal's own DBC unit: converted from another unit of its dimension (500mV for a V signal), taken as written when bare or in the signal's unit (3000rpm), and looked up in the value table when it is a name.
- **`/stream/sub` always sends `mode`** as well as `mode.*`, since the stream is named `mode`. `dut.action.*` must be named, as the master's peer driver will.
- **The mode's time is the wall clock alone** (`Round(0)`). With Go's monotonic reading, the recorder and the monitors disagreed by 9 ns after 2 s, and `monitor-check -live` caught it.
- **`logbcheck` compares the bus in timestamp order.** Two senders on one host, a BCM task and dutsim 10 µs apart, reached the recorder and candump in different orders. The kernel stamps a frame once for every socket, so the stamp order is the bus's, and arrival order is a fact about a socket.
- **`dutsim.dbc` gained `TesterPresent`** (0x701), a message for the tester to send cyclically in a mode. dutsim does not need it.
- **Not built:** a ktestd test that drives the loop itself. ktestd has none, and the behaviour above was measured live, as in M2. M6's `examples/two-racks` will make that run a script anyone can repeat.

### M6: peer driver and daemon rules (master)

- **Scope:** §4.6 and §4.7.
- **Demo:** add `examples/two-racks/`. It runs two `ktestd` (master with `psusim`; secondary with vcan and `dutsim`) and a `demo.sh`, like `examples/supply-bench`. The master runs on `vcan1`.
- **Tests:**
  - contract mismatch refuses the start;
  - mirrored records keep both timestamps;
  - the clock offset is within its stated uncertainty when the secondary's clock is shifted artificially;
  - killing the secondary removes the stress within `watermark_age` plus 20 ms, recorded with its cause;
  - `abort` going `dev` removes the stress.

#### M6: what was built (2026-11-19)

- **Package `peer`**, the lab rack's driver for a DUT rack. It has no policy of its own:
  - `Info` asks which contract the DUT rack runs (the DUT rack's new `GET /dut/contract`).
  - `Lease`, `Renew` and `Release` hold the `dut` lease.
  - `Subscribe` decodes `/stream/sub` into records on the DUT rack's clock, using its `recording.epoch`.
  - `Clock` samples the DUT rack's clock (its new `GET /clock`), and `Offsets` keeps the shortest of the last eight round trips.
- **`ktestd -peer`.** At start, the contract and resource hashes are checked, and the lab rack refuses to start if the DUT rack does not answer or differs. Then:
  - `<peer>.<stream>` mirrors each record, at its receive time, with `peer_t` and `restated`;
  - `<peer>.link` records the link, `<peer>.clock` the offset, and `rules` each rule taking hold and clearing.
- **The rules** are a `rules` section in `limits.json`, evaluated on every mirrored record and every 10 ms. A rule's decisions are recorded as request 15, a disarm with the outputs off.
- **Other changes:**
  - `contract.Identify` (formerly `monitor.Identify`): a contract's names and hashes, for a rack that does not evaluate it.
  - `dutsim` sends `DutSupply.Voltage`, so that a DUT rack can watch the DUT's supply, which belongs to the lab rack.
  - `examples/two-racks`.
- **Tests:**
  - `TestClockOffset`: the DUT rack's clock shifted by 0, 250 ms, −3 s and 37 h, with uneven delays of 0–4 ms on each side of the clock read. The estimate is within its stated uncertainty of the shift at every sample, and that uncertainty is under 5 ms after 12 samples.
  - `TestSubscribe`: a real recorder and hub, with a late join, the state restated at the next segment, and each record's time to the nanosecond.
  - `TestCheckRules` (eight refusals), `TestMirrorSchema`, `TestIdentify`, and `TestControlCheckRule` for `logbcheck`.
- **Measured live** with `examples/two-racks` (a copy run in a scratch directory):

  | Test | Result |
  |---|---|
  | contract mismatch refuses the start | a changed contract, and separately a changed resource, each refused with both hashes |
  | mirrored records keep both timestamps | all 8 of the transitions the lab rack mirrored carry the DUT rack's own times, to the nanosecond, against the DUT rack's recording. The other 4 records are restated states, flagged |
  | the DUT rack killed (`kill -9`) under a 13.5 V hold | `monitoring_lost` 2.85 ms after the kill, as the link broke; the supply's off applied 0.57 ms later; the run ended `link_lost` |
  | the DUT rack frozen (`kill -STOP`) | the last watermark arrived 5 ms before the stop; the rule fired 100.7 ms after it, judging the watermark 108 ms old; the supply's off applied 102 ms after the stop, inside watermark_age + 20 ms; the rule cleared after `kill -CONT` |
  | `abort` going `dev` | 17 V set; the DUT read it, the DUT rack's `abort` went `dev`, and `dut_abort` fired 35 ms after the set, with the supply's off applied 0.6 ms later |

  `logbcheck -control -psu` passes on the lab rack's recording, the rules' decisions and the supply's switching off included.

#### M6: choices for review

- **A lost link is lost monitoring at once.** `monitoring_lost` holds while the subscription is down, as well as while the watermark is old. A killed rack is noticed in milliseconds rather than after `watermark_age`.
- **A rule is level-triggered, with the first time special.** When it takes hold, every listed instrument is disarmed with its outputs off, armed or not, since an output can be on with nobody holding the lease. While it holds, any listed instrument that is armed is disarmed again: arming is accepted and then undone, under the rule's number. Nothing fires at start, before the first connection, unless something is armed, so a lab rack started before its DUT rack leaves its outputs as they are.
- **`-peer` requires `rules.monitoring_lost`, and rules require `-peer`.** A lab rack that stressed a DUT nobody was watching would protect nothing, and rules about no peer would do nothing. `dut_abort` is optional, and its monitor must be in the contract.
- **The lab rack checks the contract by its identity only** (`contract.Identify`): names and hashes, as strictly decoded as `Load`. It cannot compile the monitors, since their channels are the DUT rack's. Both hashes, contract and resource, must match, and they are checked again on every reconnection.
- **A mirrored record's time is when this rack received it,** and `peer_t` holds the DUT rack's own time. One clock per stream keeps the recording in order. `peer_t` minus the recorded offset puts a record on this rack's clock, which is what M7's windows will do.
- **Mirrored names:** `dut.mon.watermark` rather than §4.6's `dut.watermark`, so that every mirror is the peer's name and the DUT rack's stream name. `dut.connected` is `dut.link.connected`.
- **Held states at join arrive with the next segment.** logb restates held values when a segment begins, so a lab rack joining mid-segment learns each monitor's state within a second, or from its next record if that comes first. That restatement is mirrored once, flagged `restated`.
- **The watermark's age** is by this rack's clock: the watermark's time less the estimated offset, or its receive time before there is an estimate. With the clocks in step, as on one machine, the two agree within the round trip.
- **The lab rack holds the dut lease** from connection on, renewed every 10 s. A lease refused, because someone else holds it, is recorded on `dut.link`, and the subscription and the rules go on: protection does not depend on the lease.
- **Link records are written on change.** Reconnection attempts every 500 ms that fail the same way are not recorded again.
- **The two racks' clocks** are both read from this machine on one host. The `TestClockOffset` shift is applied to the clock the DUT rack reports; a DUT rack whose recorded times were shifted is not something ktestd can be made to do.
- **Not done:** the procedure check in §4.6 ("refuse to start if the procedure names a mode, action or monitor that the contract lacks"). There are no DUT keywords yet; it comes with M7, using `Identity`'s names.

### M7: DUT keyword library (master)

- **Scope:** the DUT part of §4.8, on the executor of M2: the five operations, `DUT *`, `Verify Baseline`, `Classify Recovery`, `Threshold Search`. The library is in `procedure/lib`.
- **Demo:** a sweep in `examples/two-racks/` over supply dips (the stress), classifying the DUT per point.
  - Expected results come from `dutsim`'s configuration.
  - Write a check, in the style of `logbcheck`, that holds the procedure's verdicts to `dutsim`'s transcript.
- **Tests:**
  - every refused construct, with its sentence;
  - every load-time check;
  - each outcome path of each operation, including `link_lost` during `Evaluate`;
  - replaying a recording reproduces the same verdicts.

#### M7: what was built (2026-11-19)

- **The master's DUT keywords**, `lib.AddDUT`, added to a daemon's library with `-peer`:
  - the operations: `DUT Set Mode`, `DUT Invoke`, `DUT Wait For`, `DUT Evaluate`, `DUT Snapshot`;
  - the keywords made of them: `DUT Prepare`, `Verify Baseline`, `Classify Recovery`, `Threshold Search`;
  - `Mark Time`.
- **Each name is checked at load against the contract's identity:** every mode, action and monitor a call names, including `ready=` and `function=`. This is the procedure check §4.6 asked for.
- **In the daemon,** `peerView` keeps what the lab rack mirrors: each monitor's states on the DUT rack's clock, its watermark, the mode as applied, the actions' ends, the link and the offset. `dutPeer` implements the operations over it. The driver arms the DUT once it holds its lease.
- **`<peer>.judgement` records every answer the monitors gave a keyword.** A `Deviations` answer gives its window on the DUT rack's clock; an `AllOK` answer gives the watermark it read at. Both carry the offset used.
- **`logbcheck -judgements`** replays each judgement from the mirrored states. **`logbcheck -dutsim`** holds every step result to dutsim's transcript.
- **The demo:**
  - `examples/two-racks/lab/procedures/sweep.robot`: `Sweep` dips the DUT's supply to 12, 10, 9.5, 8, 6 and 3 V and classifies each; `Threshold` searches from 12 V in 0.5 V steps.
  - `demo.sh` takes `BEHAVIOUR=` for dutsim.
- **Tests:**
  - `TestDUTLoadChecks`: nine refusals at load, each at its line.
  - `TestClassifyRecovery`: every class from scripted monitors, including C for a function that stopped on the way back, and E for an action that ran and failed.
  - `TestDUTOutcomes`: ok and nok as values; refused, timeout and fail; `link_lost` during `DUT Evaluate` and `DUT Wait For`; the teardown run each time.
  - `TestThresholdSearch`, `TestDutsimCheck` (five planted faults) and `TestReplayJudgement`.
- **Measured live**, each run checked with `logbcheck -control -judgements -psu -dutsim`:

  | dutsim | Sweep 12 / 10 / 9.5 / 8 / 6 / 3 V | Checks |
  |---|---|---|
  | `glitch` | A A A B B B | 6 verdicts match the transcript |
  | `restore` | A A A C C C | 6 match |
  | `reset` | A A A D D D | 6 match; 19 judgements replay |
  | `never` | A A A E, then the next baseline fails (the DUT is gone) | 4 match |
  | `reset`, `Threshold` | ok to 9 V, nok at 8.5 V: threshold 9 V, dutsim's `-vmin` | 8 match |

#### M7: choices for review

- **Windows run forward from the call.** `DUT Evaluate    alive,speed    window=2s` watches the next two seconds, which reads in order in a procedure: apply the stress, then evaluate it. `since=${t}` (from `Mark Time`) evaluates back to a moment instead, and `Classify Recovery` takes it, since it is called after the stress is removed.
- **A window is put on the DUT rack's clock with the offset, and widened by its uncertainty on both sides,** so that a deviation at an edge counts against the DUT. `DUT Evaluate` waits for the DUT rack's watermark to pass the window's end; 2 s past it, the link counts as lost (`link_lost`). That is well beyond `watermark_age`, so the rule fires first.
- **nok means dev or unknown**, as §4.8 says. `inactive` and `settling` are not deviations.
- **`Classify Recovery`** (C10) looks for deviations from `since` to the call. If there are none, the class is A. Otherwise it waits up to `timeout` for every monitor to be ok, and looks again up to that moment, so that a function stopping after the stress is removed still counts. Then:
  - back by itself: B if only monitors outside `function=` deviated, C if one in it did;
  - not back: `action=` is run, and D if that brings everything back, E if not;
  - an action that runs and fails, its own wait timing out for example, still leads to E. Only a refusal or a lost link fails the keyword. The `never` run found this.
- **`Threshold Search`** has the signature in §4.8's table, since the handoff left it open. It dips from `start` down by `step` to `floor`, each dip for `duration`, back to `nominal`, then waits `recover` for every monitor, running `action=` if they are not back. A level passes if no monitor deviated from the dip's start until all were ok again. Each level is a step result (`ok`, `nok`, or `lost` if the DUT did not come back), and so is the threshold. The levels are counted, not accumulated, so 0.1 V steps do not drift. Only `descending` exists.
- **Verdicts replay through their judgements.** A class cannot be recomputed from a recording alone, because the moments its keyword waited until are not in it. Every answer the monitors gave is in `<peer>.judgement`, and the class follows from those answers by fixed rules. So `-judgements` recomputes every answer, and `-dutsim` holds every verdict to the DUT's own account.
- **Conditions over `dut.*` channels need the channels mirrored before the procedure loads.** `DUT Wait For` is checked against the types of the channels mirrored so far, so a procedure started before the lab rack has connected is refused with "not a channel here". The keywords that name monitors check against the contract, not against the mirrored channels.
- **`demo.sh` takes `BEHAVIOUR=`** for dutsim, so that every class can be shown.

### M8: locked mode

- **Scope:** §4.9.
- **Tests:**
  - a matching run is marked accredited;
  - a changed procedure byte refuses the run, or marks it, as configured.

#### M8: what was built (2026-11-19)

- **`ktestd -locked locked.json`** (`cmd/ktestd/locked.go`), with `-procedures`. The file:

  ```json
  {"schema": 1, "on_mismatch": "refuse", "binary": "<sha256>", "library": "ktest-lib 0.1.0",
   "limits": "<sha256>" | "built-in", "instruments": "<sha256>",
   "procedures": {"sweep.robot": "<sha256>", "common.resource": "<sha256>"}}
  ```

  Loaded strictly: unknown keys, a sum that is not 64 lowercase hex digits, an `on_mismatch` other than `refuse` or `mark`, a procedure path outside the directory or not in clean form, and a file with no procedures all refuse the start.
- **`ktestd … -lock-hashes`** writes that file for the configuration given by the other flags, and exits before touching a bus or an instrument.
- **At run start** the loaded procedure is held to the file: the daemon's four sums, and every file the procedure was loaded from (`procedure.Suite.Sums`, new: the sha256 of each file's parsed bytes). Nothing differs: accredited. Something differs: refused, `403` and a `procedure.run` refusal naming what, or with `mark`, run and recorded as not accredited with the same sentence.
- **`procedure.run`** gains `accredited` (held, bit 1 of the state byte) and `lock` (held, the lock file's sha256, bytes 580–643); `mode` is now `locked` or `engineering`. The lock file is attached to the recording as `locked.json`, with `locked.source` and `locked.sha256` in its provenance.
- **Tests:** `TestLockedRun` drives `POST /procedures/run` through a loop recording to memory and reads `procedure.run` back: a matching run accredited; one byte changed in the procedure, or in a resource it pulls in, refused under `refuse` and marked under `mark`; a file the lock does not list refused; a changed binary refusing a matching procedure; engineering mode accrediting nothing. Breaking the per-file comparison fails four of them. `TestLoadLockedRefusals` (13 sentences), `TestLockedDaemonDiffs`, `TestLockSums`, `TestInstrumentConfig`.
- **Measured live** on `vcan0` with `psusim` and the supply bench's `ramp.robot`: a matching run recorded `accredited=true mode=locked` at start and end; one comment line appended to `ramp.robot` was refused with `ramp.robot (sha256 e2d35abea965…, locked 95bc58878ab9…)`; the same under `mark` ran to `ok` with `accredited=false` and that sentence.

#### M8: choices for review

- **The check is per run, not at start.** A daemon whose binary, limits or instruments differ from its lock still starts, records and drives its instruments, and says at start that every run will be refused (or marked). Refusing the start would take the rack down for what concerns only accreditation. The convention "doubtful configuration refused at start" still applies to the lock file itself.
- **Per-file sums, not the suite hash.** `procedure.run`'s `hash` covers one run's files together; a lock lists each file, so that one lock covers every procedure in the directory, and a refusal says which file changed. `-lock-hashes` sums every regular file under the directory (dotfiles skipped). A file a run uses that the lock does not list counts as a difference.
- **Sums of the parsed bytes.** The sums are taken as the procedure is loaded, from the bytes that were parsed, so a file changed between the check and the run cannot slip through.
- **The binary is `/proc/self/exe`**, the file this process started from, even if another has since been installed at its path.
- **The library is compared by its version string**, as §4.9 says, not hashed; it is compiled into the binary, whose sum covers it anyway.
- **"Instrument configuration"** is the sha256 of a sorted text: the bus, the DBC file's sha256 (or `none`), every `psu.*` and `scope.*` provenance line (address, channels, dialect, sign rule, trigger delay, full-rate channels), and the two poll periods. Instrument addresses are in it, so moving a supply to another address needs a new lock. `-panel` is not: it is presentation.
- **The DUT contract is not locked.** It changes with each DUT and is validated on the DUT rack's side; the master already refuses a contract that differs from the DUT rack's, and every run records its sha256. Locking it would mean a new lock file per DUT. Add `"contract"` to the file if the lab wants it.
- **Without `-limits`**, the lock says `"limits": "built-in"`: the defaults are part of the binary.
- **A refusal is `403`** (`errLocked`), as a refusal by policy is, not `400`: the procedure itself is valid.
- **Engineering mode records `accredited=false`**, never true: accreditation needs a lock.

## 7. Open questions and what to assume meanwhile

| Question | Assume until answered |
|---|---|
| Who writes monitor expressions and signs off the contract? | not a software question; record the contract hash, nothing more |
| Locked mode: refuse, or mark as not accredited? | refuse by default, configurable (built in M8: `"on_mismatch"`) |
| Is the REST clock offset enough, or is PTP needed? | REST estimate with uncertainty; PTP later, as an alternative source |
| Which watermark age means monitoring lost? | 100 ms default, configurable per lab |
| May a run continue after reconnection? | no: the run fails; a new run starts |
| Does the step sequence fit real standards (immunity, emissions, climatic, transients)? | the user checks this before M7's library is frozen; keep keywords parameterised |

## 8. Out of scope

- Merging the two daemons, or any shared-memory or non-REST command channel.
- Real Robot Framework compatibility: Python libraries, listeners, `log.html`.
- Report generation.
- Lab-specific instrument drivers beyond the supply. Those follow the existing driver pattern later.
- A UI for procedures. The rack shows `procedure.*` and `dut.*` channels with existing widgets.
