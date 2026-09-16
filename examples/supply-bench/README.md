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

- **Take control**, then **Arm**: the widgets unlock.
- ch1 voltage 5, ch1 output **ON**: measured 5 V, 0.5 A.
- ch1 current limit 0.2: the supply goes constant-current, 2 V and 0.2 A.
- ch1 voltage 20: refused — `limits.json` caps ch1 at 12 V.
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

Afterwards, `logbcheck -control demo.logb /dev/null` checks every decision in the
recording against the limits it declares.
