# A DUT rack that is not ktestd

`library-rack` is a DUT rack written as a program of its own on package
[`secondary`](../../secondary), the library side of the
[peer protocol](../../doc/peer-protocol.md). A ktestd lab rack drives it as
its peer exactly as it drives a ktestd DUT rack: same contract check, same
lease, modes, actions, monitors, watermark and protection rules.

| | runs | on |
|---|---|---|
| the DUT rack | `library-rack`: the simulated DUT of package `dut`, in-process, powered from psusim's channel 1 | https://127.0.0.1:8445/ |
| the lab rack | the [two-rack demo](../two-racks)'s lab rack, unchanged: its limits, rules and procedures | `vcan1`, https://localhost:8443/ |

    ./demo.sh

It builds the programs, makes tokens the first time (with `ktestd -new-token`,
whose file `library-rack` reads too), copies this rack's contract as the lab
rack's own (`lab-contract/`), and runs until Ctrl-C, recording `dut.logb` and
`lab/lab.logb`. It needs `vcan1` for the lab rack's ktestd.

## What it takes to be a DUT rack

Everything is in [`main.go`](main.go), in about 300 lines:

1. **Declare the streams** you record of the DUT, with `record.Schema`: here
   `DutStatus`, `EngineData` and `DutSupply`. The contract's inputs name them.
2. **Implement the actions** as a `secondary.Actions`. Here they are Go
   functions; [`actions.txt`](actions.txt) is their resource, which the
   contract names and both racks hash, so that what an action does is part of
   the contract.
3. **Say who may connect**: an `Auth`. Here, ktestd's tokens file.
4. **Make the server** with `secondary.New`, serve its `Handler` over TLS, and
   call `Run`.
5. **Write each record** as you read it, with `Server.Write`, stamped with the
   time you read it. Write it within the server's reorder window (20 ms by
   default) of that time: a record later than that is a gap, and every
   monitor is `unknown` until its inputs are known again.

The server does the rest: the monitors, the watermark, the recording, the
subscription stream and the REST interface.

## Try it

With the lab rack's local socket:

    L="curl -s --unix-socket /tmp/ktest-lab.sock"
    $L -d '{"file":"sweep.robot","test":"Sweep"}' http://ktest/procedures/run   # dips, each classified
    $L -d '{"file":"hold.robot","test":"Hold"}' http://ktest/procedures/run     # 13.5 V, held

Freeze the DUT rack while `Hold` runs (`kill -STOP` the `library-rack`
process): its watermark stops, and the lab rack removes the stress about
100 ms later. `BEHAVIOUR=glitch`, `restore`, `reset` (the default) or `never`
sets what the DUT does below 9 V, as in the two-rack demo.

## Checking a run

    bin/logbcheck -control -judgements -psu psu1=lab/psusim.txt -dutsim dut.txt lab/lab.logb /dev/null
    bin/monitor-check -live dut-interface.json dut.logb

The first holds the lab rack's recording to its limits, its rules, psusim's
transcript and the DUT's own account of what it did; the second replays this
rack's recording through the evaluator and holds every transition it recorded
live to the replay, to the nanosecond.
