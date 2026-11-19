# Two racks, one DUT

A lab rack and a DUT rack testing one simulated device, on one machine, with
no hardware (doc/two-racks-handoff.md):

| | runs | on |
|---|---|---|
| the DUT rack | `dutsim`, the simulated DUT, and a ktestd running the DUT's contract (`dut/dut-interface.json`): its monitors live, its modes and actions | `vcan0`, https://127.0.0.1:8445/ |
| the lab rack | a ktestd driving `psusim`, the stress, and the DUT rack as a peer | `vcan1`, https://localhost:8443/ |

The DUT is powered from psusim's channel 1, so the lab rack's stress is the
DUT's supply. dutsim reports what it reads in `DutSupply`, and the contract's
`abort` monitor watches it.

    ./demo.sh

It builds the programs, makes tokens the first time, copies the DUT rack's
contract as the lab rack's own (`lab/contract/`), and runs until Ctrl-C,
recording `dut/dut.logb` and `lab/lab.logb`. It needs `vcan0` and `vcan1`, and
says how to make them if they are missing.

## What the lab rack does about the DUT rack

`lab/limits.json`'s rules remove the stress, switching `psu1` off and
disarming it, whatever any client asks:

- **monitoring_lost**, when the DUT rack's watermark is more than 100 ms old,
  or the link to it is down;
- **dut_abort**, when the DUT rack's `abort` monitor is `dev`: the DUT reads
  its supply at 16 V or more.

Each firing is a control decision in `psu1.control` (request 15, the rule's
name in `channel`), and a record in `rules` with its cause. A procedure under
way fails with the rule as its cause.

Try them, with the lab rack's local socket:

    L="curl -s --unix-socket /tmp/ktest-lab.sock"
    $L -d '{"file":"hold.robot","test":"Hold"}' http://ktest/procedures/run   # 13.5 V, held

- **Kill the DUT rack** (`kill -9` its ktestd): the stress is off within a few
  milliseconds, as the link breaks, and the run fails `link_lost`.
- **Freeze it** (`kill -STOP`): the link stays up and the watermark stops. The
  stress is off about 100 ms later, and the rule clears after `kill -CONT`.
- **Overvoltage**: lease and arm `psu1` yourself, and set 17 V. The DUT
  reports it, `abort` goes `dev`, and the stress is off.

## Classifying the DUT

`lab/procedures/sweep.robot` puts the DUT through supply dips and classifies
each by ISO 16750-1's functional status:

    $L -d '{"file":"sweep.robot","test":"Sweep"}' http://ktest/procedures/run       # 12 V down to 3 V
    $L -d '{"file":"sweep.robot","test":"Threshold"}' http://ktest/procedures/run   # the lowest dip it copes with

The DUT copes with anything down to 9 V (dutsim's `-vmin`), so the dips to 12,
10 and 9.5 V are class A. Below that, the class depends on what dutsim is told
to do, which `BEHAVIOUR=` sets:

| `BEHAVIOUR=` | the DUT, below 9 V | class |
|---|---|---|
| `glitch` | reports a wrong speed, and recovers | B |
| `restore` | stops, and reboots when the supply returns | C |
| `reset` (the default) | stops until the reset action | D |
| `never` | stops for good | E, and the sweep ends there |

    BEHAVIOUR=restore ./demo.sh

`Threshold` finds 9 V. Each class is a step result in `procedure.result`.

## What the lab rack records of the DUT rack

| stream | holds |
|---|---|
| `dut.mon.*`, `dut.mode`, `dut.action.*` | the DUT rack's records, received at this rack's time, with the DUT rack's own time in `peer_t` |
| `dut.link` | connected or not, and why |
| `dut.clock` | the DUT rack's clock less this one, with its uncertainty |
| `rules` | each rule taking hold and clearing |

## The contract is checked

The lab rack refuses to start unless the DUT rack runs its own copy of the
contract, byte for byte, its resource included. Edit
`lab/contract/dut-interface.json` or `lab/contract/actions.robot`, and run
again, to see the refusal; delete `lab/contract` to start from the DUT rack's
again.

## Checking a run

    bin/logbcheck -control -judgements -psu psu1=lab/psusim.txt -dutsim dut/dutsim.txt lab/lab.logb /dev/null

holds the lab rack's recording to its limits and rules and to psusim's own
transcript, replays every judgement of the DUT's monitors from the mirrored
states, and holds every class and level to dutsim's own account of what the
DUT did; and

    bin/monitor-check -live dut/dut-interface.json dut/dut.logb

holds the DUT rack's live monitors to a replay of its recording.
