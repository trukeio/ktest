#!/bin/sh
# One lab rack, two DUT racks, two DUTs on one supply (doc/peer-protocol.md §9.7).
#
#   DUT rack a: ktestd and dutsim on vcan0, https://127.0.0.1:8445/; the DUT
#               stops below 9 V until it is reset (as in ../two-racks)
#   DUT rack b: library-rack, https://127.0.0.1:8447/; its DUT glitches below
#               9 V and recovers by itself (BEHAVIOUR_B to change it)
#   lab rack:   ktestd on vcan1, https://localhost:8443/, driving psusim, whose
#               channel 1 powers both DUTs, and both DUT racks as -peer a and b
#
# The lab rack removes the stress when either rack's watermark is more than
# 100 ms old, or either's abort monitor is dev. Ctrl-C stops all.
set -e
cd "$(dirname "$0")"

for i in vcan0 vcan1; do
    if ! ip link show $i >/dev/null 2>&1; then
        echo "$i is missing; create it with:" >&2
        echo "  sudo modprobe vcan && sudo ip link add dev $i type vcan && sudo ip link set up $i" >&2
        exit 1
    fi
done
(cd ../.. && go build -o examples/two-duts/bin/ ./cmd/ktestd ./cmd/psusim ./cmd/dutsim ./cmd/logbcheck \
    ./cmd/monitor-check ./examples/library-rack)

mkdir -p a b lab
# Tokens, made once: each DUT rack admits the lab rack as "master"; the lab
# rack admits you.
for r in a b; do
    if [ ! -s $r/tokens ]; then
        (umask 077 && bin/ktestd -new-token master:operate 2>&1 >$r/tokens | tail -n 1 > lab/$r.token)
    fi
done
if [ ! -s lab/tokens ]; then
    (umask 077 && bin/ktestd -new-token demo:operate 2>&1 >lab/tokens | tail -n 1 > lab/token)
fi
# The lab rack's own copies of the two contracts, made the first time.
if [ ! -d lab-contract ]; then
    mkdir -p lab-contract/a lab-contract/b
    cp ../two-racks/dut/dut-interface.json ../two-racks/dut/actions.robot lab-contract/a/
    cp ../library-rack/dut-interface.json ../library-rack/actions.txt lab-contract/b/
fi

pids=""
trap 'kill $pids 2>/dev/null' EXIT
bin/psusim -addr 127.0.0.1:5025 -transcript lab/psusim.txt & pids="$pids $!"
bin/dutsim -supply 127.0.0.1:5025 -behaviour reset -transcript a/dut.txt vcan0 & pids="$pids $!"
bin/ktestd -https 127.0.0.1:8445 -state a/state -tokens a/tokens -limits ../two-racks/dut/limits.json \
    -dbc ../../dut/dutsim.dbc -contract ../two-racks/dut/dut-interface.json -sock /tmp/ktest-dut-a.sock \
    -o a/dut.logb vcan0 & pids="$pids $!"
bin/library-rack -https 127.0.0.1:8447 -tokens b/tokens -cert b/cert.pem -o b/dut.logb \
    -contract ../library-rack/dut-interface.json -behaviour "${BEHAVIOUR_B:-glitch}" -transcript b/dut.txt &
pids="$pids $!"
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
    [ -s a/state/tls-cert.pem ] && [ -s b/cert.pem ] &&
        curl -s -o /dev/null --cacert a/state/tls-cert.pem https://127.0.0.1:8445/ &&
        curl -s -o /dev/null --cacert b/cert.pem https://127.0.0.1:8447/clock && break
    sleep 0.3
done
(sleep 1; printf '\n  The lab rack: https://localhost:8443/, token %s\n\n' "$(cat lab/token)") &
bin/ktestd -https :8443 -state lab/state -tokens lab/tokens -limits ../two-racks/lab/limits.json \
    -psu psu1=127.0.0.1:5025,2 -procedures lab/procedures \
    -peer a=https://127.0.0.1:8445,token=lab/a.token,cacert=a/state/tls-cert.pem,contract=lab-contract/a/dut-interface.json \
    -peer b=https://127.0.0.1:8447,token=lab/b.token,cacert=b/cert.pem,contract=lab-contract/b/dut-interface.json \
    -sock /tmp/ktest-lab.sock -o lab/lab.logb vcan1
