#!/bin/sh
# Two racks, one DUT, on one machine and no hardware (handoff §1).
#
#   the DUT rack: dutsim on vcan0, powered by psusim's channel 1, and a ktestd
#                 running its contract's monitors, on https://127.0.0.1:8445/
#   the lab rack: a ktestd on vcan1 that drives psusim, the stress, and drives
#                 the DUT rack as a peer, on https://localhost:8443/
#
# The lab rack removes the stress when the DUT rack's watermark is more than
# 100 ms old, or its abort monitor is dev (lab/limits.json). Ctrl-C stops all.
set -e
cd "$(dirname "$0")"

for i in vcan0 vcan1; do
    if ! ip link show $i >/dev/null 2>&1; then
        echo "$i is missing; create it with:" >&2
        echo "  sudo modprobe vcan && sudo ip link add dev $i type vcan && sudo ip link set up $i" >&2
        exit 1
    fi
done
(cd ../.. && GO111MODULE=on go build -o examples/two-racks/bin/ ./cmd/ktestd ./cmd/psusim ./cmd/dutsim ./cmd/logbcheck ./cmd/monitor-check)

# Tokens, made once. The DUT rack admits the lab rack as "master" and you as
# "demo"; the lab rack admits you. ktestd keeps only their hashes in tokens;
# the tokens themselves are kept beside them for the demo's sake, readable by
# you alone. A real bench would not keep them.
if [ ! -s dut/tokens ]; then
    (umask 077
     bin/ktestd -new-token master:operate 2>&1 >dut/tokens | tail -n 1 > lab/dut.token
     bin/ktestd -new-token demo:operate 2>&1 >>dut/tokens | tail -n 1 > dut/token)
fi
if [ ! -s lab/tokens ]; then
    (umask 077 && bin/ktestd -new-token demo:operate 2>&1 >lab/tokens | tail -n 1 > lab/token)
fi
# The lab rack's own copy of the contract, made from the DUT rack's the first
# time. The lab rack refuses to start if the two ever differ: edit one to see.
if [ ! -d lab/contract ]; then
    mkdir lab/contract && cp dut/dut-interface.json dut/actions.robot lab/contract/
fi

pids=""
trap 'kill $pids 2>/dev/null' EXIT
bin/psusim -addr 127.0.0.1:5025 -transcript lab/psusim.txt & pids="$pids $!"
bin/dutsim -supply 127.0.0.1:5025 -behaviour reset -transcript dut/dutsim.txt vcan0 & pids="$pids $!"
bin/ktestd -https 127.0.0.1:8445 -state dut/state -tokens dut/tokens -limits dut/limits.json \
    -dbc ../../dut/dutsim.dbc -contract dut/dut-interface.json -sock /tmp/ktest-dut.sock -o dut/dut.logb vcan0 &
pids="$pids $!"
# The lab rack checks the DUT rack's contract as it starts, so the DUT rack
# answers first.
for _ in 1 2 3 4 5 6 7 8 9 10; do
    [ -s dut/state/tls-cert.pem ] && curl -s -o /dev/null --cacert dut/state/tls-cert.pem https://127.0.0.1:8445/ && break
    sleep 0.3
done
(sleep 1; printf '\n  The lab rack: https://localhost:8443/, token %s\n  The DUT rack: https://127.0.0.1:8445/, token %s\n\n' \
    "$(cat lab/token)" "$(cat dut/token)") &
bin/ktestd -https :8443 -state lab/state -tokens lab/tokens -limits lab/limits.json \
    -psu psu1=127.0.0.1:5025,2 -procedures lab/procedures \
    -peer dut=https://127.0.0.1:8445,token=lab/dut.token,cacert=dut/state/tls-cert.pem,contract=lab/contract/dut-interface.json \
    -sock /tmp/ktest-lab.sock -o lab/lab.logb vcan1
