# Scope bench demo

The rack driving an oscilloscope, with no hardware: `scopesim` plays a
two-channel Siglent SDS1202X-E — a 2 V 1 kHz sine with a little noise on C1, a
1 V 250 Hz square on C2 — and ktestd arms it, reads it and serves the panel in
`panel.json` over HTTPS.

    ./demo.sh

It builds ktestd and scopesim, makes a login token the first time, and runs
until Ctrl-C, recording to `demo.logb`. The token is printed with the URL a
second after start, below ktestd's own lines, on every run; it is also in the
file `token`. It needs `vcan0`, and says how to make it if it is missing.

Open `https://localhost:8444/` in Chrome (or `https://<host>:8444/` from another
machine). The certificate is self-signed: *Advanced → Proceed*. Paste the token.

## What is on screen

Each scope widget draws one acquisition as a **band**, not a line. That is the
point of the reduction (§10.4): the daemon takes every sample of the
acquisition — 14 000 of them here, and up to 14 million on the instrument — and
keeps the least and greatest in each of three thousand columns, so a spike one
sample wide is still on the screen. A line through the middle of each column
would have thrown away exactly what the reduction was built to keep, and would
look perfectly plausible doing it.

The dashed red line is the trigger. Half the screen is before it, which is what
`-(tdiv × 14 / 2)` means, and a trace with no mark says nothing about which half
is which.

Under each band are the settings **that acquisition** was taken under, read out
of its own RUN frame (§5.4) rather than from whatever the instrument holds by
the time it is drawn.

## Things to try

- **Take control**, then **Arm**: the setpoints unlock. Acquisition needs
  neither — a scope is an input, and reading one harms nothing. Arming gates its
  settings, which are writes like any other (§4).
- C1 volts/div **0.3**: the trace rescales to 0.2. The instrument has only
  1-2-5 steps, so what was asked for and what it holds differ, and the widget
  says which is which — that is §4's whole point.
- Seconds/div **0.0002**: five cycles become one. A change of the sample
  interval opens a new segment, because `axis_step` lives in the schema and a
  schema is only restated at a segment boundary (§5.2); without that every
  sample after the change would decode to the wrong position and nothing would
  say so.
- **Display C2**: the second channel appears — and the sample rate halves, and
  the memory depth with it, because the two share one converter. The timebase
  readout changes although nothing was written to it, which is the instrument
  being coupled and the recording saying so.
- **STOP**: every instrument disarmed. The scope is left exactly as it is: a
  stop is for outputs (§7), and an acquisition in progress harms nothing by
  finishing.

## What the recording holds

    demo.logb
      scope1.ch1        every sample, f32 volts, on a uniform axis (full=ch1)
      scope1.ch1.env    the min/max envelope, ~3000 columns an acquisition
      scope1.ch2.env    C2's envelope; its samples are not kept at full rate
      scope1.ch1.vert   volts/div and offset, set beside applied
      scope1.timebase   seconds/div, delay, depth and the rate the scope chose
      scope1.acq        one record per channel per trigger, whatever became of it
      scope1.control    every decision the control plane took

Full rate is opt-in per channel — `full=ch1` in `demo.sh` — because continuous
full-rate capture is a decision someone made rather than a surprise disk fill
overnight (§10.4). C2 gets the envelope only.

`scope1.acq` has a record for every acquisition the daemon knew about, including
the ones the rack could not keep up with: §10.5 lets a waveform be dropped and
requires the drop to be in the recording, because a gap that does not say it is
a gap is the worst failure this system can produce.

## Checking it

`scope.log` is the simulator's own account of the run — every command it
received, what it holds after each setting, and a sha256 of the bytes of every
acquisition it handed over. Nothing that writes Logb wrote it, which is what
makes it able to falsify the recording rather than agree with it:

    (cd ../.. && go build -o examples/scope-bench/bin/ ./cmd/logbcheck)
    bin/logbcheck -control -scope scope1=scope.log demo.logb /dev/null

A sample is stored as `f32` volts (§13), so the check runs the recording's own
stated conversion backwards to recover the digitiser's codes and hashes those. A
daemon that converted with the wrong volts/div, the wrong offset or the other
sign rule — or that stated a conversion it did not use — ends up with different
bytes, and the hash says so.

`rack.json` is made from `panel.json` on the first run and kept after it; delete
it to go back to the original.
