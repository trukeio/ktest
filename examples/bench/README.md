# Bench demo: two instruments on one timeline

The supply bench demo drives a supply; the scope bench demo drives an
oscilloscope. This one runs both at once, with no hardware, which is what §1
says the whole thing is for: a supply and a scope of different kinds, each with
its own lease and its own arming, recorded into one file against one clock and
drawn on one panel.

    ./demo.sh

It builds ktestd, `psusim` and `scopesim`, makes a login token the first time,
and runs until Ctrl-C, recording to `demo.logb`. The token is printed with the
URL a second after start, below ktestd's own lines, on every run; it is also in
the file `token`. It needs `vcan0`, and says how to make it if it is missing.

Open `https://localhost:8445/` in Chrome (or `https://<host>:8445/` from another
machine). The certificate is self-signed: *Advanced → Proceed*. Paste the token.

`psusim` plays a two-channel supply (ch1 30 V / 3 A into 10 Ω, ch2 6 V / 5 A
into 100 Ω) on port 5025; `scopesim` plays a Siglent SDS1202X-E on 5026, a 2 V
1 kHz sine with a little noise on C1 and a 1 V 250 Hz square on C2. They know
nothing of each other — the scope is not measuring the supply's output, and the
demo does not pretend it is. What they share is the recording and the clock it
is written against, which is the part that has to work before a scope can be
pointed at a supply rail and believed.

## Two instruments, two leases

The panel has both in front of you, and they are governed apart:

- **Take control** of the supply, **Arm** it, and the supply's widgets unlock —
  the scope's do not. Each instrument has its own lease and its own arming
  (§7), so holding one is not holding the other, and two people can drive two
  instruments on the same bench without either being able to touch the other's.
- **Acquisition needs neither.** The scope's bands are drawn from the moment
  the daemon starts: reading an input harms nothing. Its *settings* are writes
  like any other, and those need the lease and the arming.
- **STOP** disarms everything, bus and both instruments, and turns the supply's
  outputs off. The scope is left exactly as it is — a stop is for outputs, and
  an acquisition in progress harms nothing by finishing (§7).

## Things to try

The supply, as in `../supply-bench`:

- ch1 voltage 5, ch1 output **ON**: measured 5 V, 0.5 A.
- ch1 current limit 0.2: the supply goes constant-current, 2 V and 0.2 A.
- ch1 voltage 20: refused — `limits.json` caps ch1 at 12 V, and the refusal is
  in the recording with everything else.

The scope, as in `../scope-bench`:

- C1 volts/div **0.3**: the trace rescales to 0.2, since the instrument has only
  1-2-5 steps, and the widget shows what was asked beside what is held.
- Seconds/div **0.0002**: five cycles become one, and a new segment opens,
  because `axis_step` lives in the schema (§5.2).
- **Display C2**: the sample rate halves and the depth with it — two channels
  share one converter.

And the thing neither single-instrument demo can show: turn the supply's output
on while the scope is acquiring, and the step in `psu1.ch1.meas.v` and the
acquisitions either side of it are in one file, in order, on one timeline. That
correlation is done by hand today, if at all (§1).

## What the recording holds

    demo.logb
      psu1.ch1.v.set      set beside applied, per channel
      psu1.ch1.meas.v     measured voltage and current, every 100 ms
      psu1.control        every decision taken for the supply
      scope1.ch1          every sample, f32 volts, on a uniform axis (full=ch1)
      scope1.ch1.env      the min/max envelope, ~3000 columns an acquisition
      scope1.ch2.env      C2's envelope; its samples are not kept at full rate
      scope1.ch1.vert     volts/div and offset, set beside applied
      scope1.timebase     seconds/div, delay, depth and the rate the scope chose
      scope1.acq          one record per channel per trigger, whatever became of it
      scope1.control      every decision taken for the scope
      rack.panel          the panel in force, and every save of it

Each instrument's control stream states its kind, so a rack that has just
connected knows `psu1` is a supply and `scope1` a scope before either has taken
a decision.

## Checking it

Each simulator keeps its own transcript — `psu.log` and `scope.log` — written by
something that knows nothing about Logb, which is what makes them able to
falsify the recording rather than agree with it:

    (cd ../.. && go build -o examples/bench/bin/ ./cmd/logbcheck)
    bin/logbcheck -control -psu psu1=psu.log -scope scope1=scope.log demo.logb /dev/null

That is one check over one file covering both instruments: every control
decision against the limits the recording declares, every write the supply
carried out against its transcript in order, and every acquisition against the
scope's, byte for byte by the sha256 of the codes recovered from the stored
volts.

A three-second headless run of this demo — both leases taken and armed, a
setpoint written to each, one refusal over the 12 V cap, then a stop — passes
it: 16 control decisions, 4 supply writes and 320 acquisitions, all accounted
for.

`rack.json` is made from `panel.json` on the first run and kept after it; delete
it to go back to the original.
