# One lab rack, two DUT racks

Two devices under test share one supply rail, as they would in one climatic
chamber or on one harness, and each has its own DUT rack. One lab rack drives
both as its peers, applies one stress to both, and classifies each DUT from its
own rack's monitors.

| | runs | on |
|---|---|---|
| DUT rack `a` | ktestd and `dutsim` on CAN; the DUT stops below 9 V until it is reset | `vcan0`, https://127.0.0.1:8445/ |
| DUT rack `b` | `library-rack`, on package `secondary`; the DUT glitches below 9 V and recovers by itself | https://127.0.0.1:8447/ |
| the lab rack | ktestd driving psusim, whose channel 1 powers both DUTs, with `-peer a=… -peer b=…` | `vcan1`, https://localhost:8443/ |

    ./demo.sh

It needs `vcan0` and `vcan1`. `BEHAVIOUR_B=restore` (or `reset`, `never`)
changes what DUT b does below 9 V.

## What changes with several DUT racks

- **`-peer` is given once for each rack**, each with its own name, token,
  certificate and contract copy. Each rack's streams are recorded under its
  name: `a.mon.alive`, `b.link`, `b.clock`, `a.judgement`.
- **Every DUT keyword takes `dut=`.** A procedure for one rack may leave it out;
  with two, it is required, and the modes, actions and monitors a call names
  are checked at load against that rack's contract.
- **The protection rules cover every rack.** The stress is shared, so
  `monitoring_lost` holds while any rack's watermark is too old or its link is
  down, and `dut_abort` while any rack's `abort` is `dev`. The rule's message
  names the rack: `b: the DUT rack's watermark is 105ms old`. Every contract
  must declare the abort monitor.

## Try it

    curl -s --unix-socket /tmp/ktest-lab.sock -d '{"file":"pair.robot","test":"Pair"}' http://ktest/procedures/run

`lab/procedures/pair.robot` dips the shared supply to 12, 9.5, 8 and 6 V and
classifies each DUT at each level, recorded at `dip-a` and `dip-b`. With the
defaults, `a` is A, A, D, D and `b` is A, A, B, B. Freeze one DUT rack
(`kill -STOP` its process) and the lab rack takes `monitoring_lost`, naming it.

## Checking a run

    bin/logbcheck -control -judgements -psu psu1=lab/psusim.txt \
        -dutsim dip-a=a/dut.txt -dutsim dip-b=b/dut.txt lab/lab.logb /dev/null
    bin/monitor-check -live ../two-racks/dut/dut-interface.json a/dut.logb
    bin/monitor-check -live ../library-rack/dut-interface.json b/dut.logb

`-judgements` replays each judgement from its own rack's mirrored states, and
each `-dutsim point=transcript` holds one DUT's classes to its own account of
what it did.
