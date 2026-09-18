# Supply bench demo

The rack driving a bench supply, with no hardware: `psusim` plays a two-channel
supply (ch1 30 V / 3 A into 10 Ω, ch2 6 V / 5 A into 100 Ω), and ktestd serves
the panel in `panel.json` over HTTPS.

    ./demo.sh

It builds ktestd and psusim, makes a login token the first time, and runs until
Ctrl-C, recording to `demo.logb`. The token is printed with the URL a second
after start, below ktestd's own lines, on every run; it is also in the file
`token`. It needs `vcan0`, and says how to make it if it is missing.

Open `https://localhost:8443/` in Chrome (or `https://<host>:8443/` from another
machine). The certificate is self-signed: *Advanced → Proceed*. Paste the token.

Then:

- **Take control**, then **Arm**: the panel's widgets unlock. Both are the
  panel's, as the stop is — a rack is one bench.
- The supply starts at 0 V with its current limit at 0 A, so raise **ch1
  current limit** first — drag the knob, or click it and use the arrow keys —
  to about 0.5 A, and switch **ch1 output** on.
- Now turn **ch1 voltage** to 6. The gauges read **5 V and 0.5 A**, not 6 V:
  6 V across the simulated 10 Ω load would draw 0.6 A, more than the limit
  allows, so the supply regulates current instead and the voltage lands where
  the limit puts it. The knobs hold what was asked for; the gauges show what
  the supply measures at its terminals, and this is exactly where the two part.
- The knob's needle is the setpoint the supply *holds*; what was asked for is
  the separate mark on the rim, shown only while the two differ (§4) — which is
  what a setpoint clamped to the channel's rating looks like. The knob's scale
  stops at 12 V because that is what `limits.json` allows: a scale says what is
  worth asking for, and enforces nothing.
- ch1 voltage 20, typed into **ch1 voltage, typed**: refused — `limits.json`
  caps ch1 at 12 V, and the refusal is in the recording with everything else.
- **ch1 preset** offers four values worth naming and nothing else. Pick *3V3
  logic* and the knob moves with it, since both are bound to the same channel
  and both read it back from the recording rather than from each other. Turn
  the knob to something off the list and the preset shows no choice at all,
  and says what is held: a selector that rounded to the nearest entry would be
  claiming the supply holds a value it does not.
- **ch2 voltage** is a slider — the same thing the knob is, laid out straight.
  It sends on release, not on every pixel, so a drag is one decision in the
  recording rather than two hundred.
- **ch2 output, while held** is momentary: on while the mouse is down or the
  space bar is held, off the moment it is let go — and off too if the pointer
  leaves it, the window loses focus or the tab is hidden, because a press that
  cannot be released is the failure this widget must not have. It is still not
  an interlock: the arming and STOP are (§7), since a browser that dies holds
  nothing.
- ch2 voltage 12: shown as set 12 V, held 6 V — the channel is rated 6 V and the
  supply clamps it.
- **STOP**: every instrument disarmed, every output off.

The rack is also its own editor:

- **Edit panel**: drag a widget to move it, drag its lower right corner to
  resize it, click it to change its label or channel, ✕ to remove it; **Add**
  puts a new one below the rest. The widgets keep drawing live data meanwhile.
- **Save**: the panel is checked as the daemon checks the file at start, written
  back to `rack.json`, and embedded in the recording again. Any other page
  showing the rack changes with it.
- Edit in two tabs and save both: the second is refused, since it was edited
  from a panel no longer in force, and says so.

`rack.json` is made from `panel.json` on the first run and kept after it; delete
it to go back to the original.

The daemon also runs procedures, from `procedures/`. `ramp.robot` ramps
channel 1 through 2, 4, 6 and 8 V, checks at each level that the terminals
measure what was set, and records each step:

    K="curl -s --cacert $HOME/.local/state/ktestd/tls-cert.pem -H \"Authorization: Bearer $(cat token)\""
    eval $K -d "'{\"file\":\"ramp.robot\",\"test\":\"Ramp\"}'" https://localhost:8443/procedures/run
    eval $K -X DELETE https://localhost:8443/procedures/run     # stop it

- **Ramp** takes psu1's lease and arms it for you, runs, and releases it. Your
  browser must not hold psu1's lease meanwhile, or the run is refused.
- **Over The Limit** asks for 14 V; `limits.json` caps channel 1 at 12 V, so
  the run fails as refused, and its teardown still switches the output off.
- The run carries on if the client disconnects. Every keyword call is in
  `procedure.step`, every result in `procedure.result`.

Afterwards, `logbcheck -control demo.logb /dev/null` checks every decision in the
recording against the limits it declares, the procedure's included.
