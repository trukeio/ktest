#!/bin/sh
# The scope bench demo: a simulated oscilloscope, and ktestd serving the rack on
# https://localhost:8444/. Ctrl-C stops both.
#
# It is milestone 4's slices 1 and 2 end to end: scopesim speaks the Siglent
# dialect, ktestd arms it and reads it, the recording gets a min/max envelope of
# every acquisition and every sample of C1 at full rate, and the rack draws the
# envelope as the band it is, with the settings the acquisition was taken under
# beside it and the trigger marked.
set -e
cd "$(dirname "$0")"

if ! ip link show vcan0 >/dev/null 2>&1; then
    echo "vcan0 is missing; create it with:" >&2
    echo "  sudo modprobe vcan && sudo ip link add dev vcan0 type vcan && sudo ip link set up vcan0" >&2
    exit 1
fi
(cd ../.. && GO111MODULE=on go build -o examples/scope-bench/bin/ ./cmd/ktestd ./cmd/scopesim)

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

# -transcript is the simulator's own account of the run, in a form nothing that
# writes Logb wrote. It is what logbcheck -scope holds the recording to:
#
#   bin/logbcheck -control -scope scope1=scope.log demo.logb ref.log
bin/scopesim -addr 127.0.0.1:5026 -transcript scope.log \
    -channels 'sine:amp=2,freq=1000,noise=0.02;square:amp=1,freq=250' -on 1,2 &
sim=$!
trap 'kill $sim 2>/dev/null' EXIT
# After ktestd's own start-up lines, so that it is the last thing on screen.
(sleep 1; printf '\n  Open https://localhost:8444/ in Chrome and log in with this token:\n\n    %s\n\n' "$(cat token)") &
bin/ktestd -https :8444 -tokens tokens -panel rack.json -limits limits.json \
    -scope scope1=127.0.0.1:5026,2,full=ch1 -sock /tmp/ktest-scope-demo.sock -o demo.logb vcan0
