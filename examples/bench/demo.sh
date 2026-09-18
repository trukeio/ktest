#!/bin/sh
# The bench demo: a simulated supply and a simulated oscilloscope on one
# timeline, and ktestd serving the rack on https://localhost:8445/. Ctrl-C
# stops all three.
#
# The supply bench and the scope bench each show one instrument. This one
# shows what §1 says the whole thing is for: two instruments of different
# kinds, each with its own lease and its own arming, recorded into one file
# against one clock, and drawn on one panel.
set -e
cd "$(dirname "$0")"

if ! ip link show vcan0 >/dev/null 2>&1; then
    echo "vcan0 is missing; create it with:" >&2
    echo "  sudo modprobe vcan && sudo ip link add dev vcan0 type vcan && sudo ip link set up vcan0" >&2
    exit 1
fi
(cd ../.. && GO111MODULE=on go build -o examples/bench/bin/ ./cmd/ktestd ./cmd/psusim ./cmd/scopesim)

# A login token, made once. ktestd keeps only its hash, in tokens; for the
# demo's sake the token itself is kept too, in token, readable by you alone,
# so that it can be shown on every run. A real bench would not keep it.
if [ ! -f tokens ] || [ ! -s token ]; then
    (umask 077 && bin/ktestd -new-token demo:operate 2>&1 >tokens | tail -n 1 > token)
fi

# The rack saves its edits to the panel it was started with, so it is given a
# copy: rack.json, made from panel.json on the first run. Delete it to start
# again from panel.json.
[ -f rack.json ] || cp panel.json rack.json

# Each simulator keeps its own transcript, in a form nothing that writes Logb
# wrote. Together they are what logbcheck holds the recording to:
#
#   bin/logbcheck -control -psu psu1=psu.log -scope scope1=scope.log demo.logb /dev/null
bin/psusim -addr 127.0.0.1:5025 -transcript psu.log &
psu=$!
bin/scopesim -addr 127.0.0.1:5026 -transcript scope.log \
    -channels 'sine:amp=2,freq=1000,noise=0.02;square:amp=1,freq=250' -on 1,2 &
scope=$!
trap 'kill $psu $scope 2>/dev/null' EXIT
# After ktestd's own start-up lines, so that it is the last thing on screen.
(sleep 1; printf '\n  Open https://localhost:8445/ in Chrome and log in with this token:\n\n    %s\n\n' "$(cat token)") &
bin/ktestd -https :8445 -tokens tokens -panel rack.json -limits limits.json \
    -psu psu1=127.0.0.1:5025,2 -scope scope1=127.0.0.1:5026,2,full=ch1 \
    -sock /tmp/ktest-bench-demo.sock -o demo.logb vcan0
