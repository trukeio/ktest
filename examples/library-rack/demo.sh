#!/bin/sh
# A DUT rack that is not ktestd, driven by a ktestd lab rack (doc/peer-protocol.md).
#
#   the DUT rack: library-rack, a program built on package secondary, running the
#                 simulated DUT in-process, on https://127.0.0.1:8445/
#   the lab rack: the two-rack demo's lab rack, unchanged: a ktestd on vcan1 that
#                 drives psusim, the stress, and this DUT rack as its peer, on
#                 https://localhost:8443/
#
# BEHAVIOUR=glitch, restore, reset (the default) or never: what the DUT does
# when its supply dips below 9 V. Ctrl-C stops all.
set -e
cd "$(dirname "$0")"

if ! ip link show vcan1 >/dev/null 2>&1; then
    echo "vcan1 is missing; create it with:" >&2
    echo "  sudo modprobe vcan && sudo ip link add dev vcan1 type vcan && sudo ip link set up vcan1" >&2
    exit 1
fi
(cd ../.. && go build -o examples/library-rack/bin/ ./cmd/ktestd ./cmd/psusim ./cmd/logbcheck ./cmd/monitor-check \
    ./examples/library-rack)

# Tokens, made once with ktestd's -new-token, whose file library-rack reads too.
if [ ! -s tokens ]; then
    (umask 077 && bin/ktestd -new-token master:operate 2>&1 >tokens | tail -n 1 > master.token)
fi
mkdir -p lab
if [ ! -s lab/tokens ]; then
    (umask 077 && bin/ktestd -new-token demo:operate 2>&1 >lab/tokens | tail -n 1 > lab/token)
fi
# The lab rack's own copy of the contract, made from this rack's the first time.
if [ ! -d lab-contract ]; then
    mkdir lab-contract && cp dut-interface.json actions.txt lab-contract/
fi

pids=""
trap 'kill $pids 2>/dev/null' EXIT
bin/psusim -addr 127.0.0.1:5025 -transcript lab/psusim.txt & pids="$pids $!"
bin/library-rack -https 127.0.0.1:8445 -tokens tokens -cert cert.pem -o dut.logb \
    -behaviour "${BEHAVIOUR:-reset}" -transcript dut.txt & pids="$pids $!"
for _ in 1 2 3 4 5 6 7 8 9 10; do
    [ -s cert.pem ] && curl -s -o /dev/null --cacert cert.pem https://127.0.0.1:8445/clock && break
    sleep 0.3
done
(sleep 1; printf '\n  The lab rack: https://localhost:8443/, token %s\n\n' "$(cat lab/token)") &
bin/ktestd -https :8443 -state lab/state -tokens lab/tokens -limits ../two-racks/lab/limits.json \
    -psu psu1=127.0.0.1:5025,2 -procedures ../two-racks/lab/procedures \
    -peer dut=https://127.0.0.1:8445,token=master.token,cacert=cert.pem,contract=lab-contract/dut-interface.json \
    -sock /tmp/ktest-lab.sock -o lab/lab.logb vcan1
