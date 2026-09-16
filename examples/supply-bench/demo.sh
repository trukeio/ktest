#!/bin/sh
# The supply bench demo: a simulated supply, and ktestd serving the rack on
# https://localhost:8443/. Ctrl-C stops both.
set -e
cd "$(dirname "$0")"

if ! ip link show vcan0 >/dev/null 2>&1; then
    echo "vcan0 is missing; create it with:" >&2
    echo "  sudo modprobe vcan && sudo ip link add dev vcan0 type vcan && sudo ip link set up vcan0" >&2
    exit 1
fi
(cd ../.. && GO111MODULE=on go build -o examples/supply-bench/bin/ ./cmd/ktestd ./cmd/psusim)

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

bin/psusim -addr 127.0.0.1:5025 &
sim=$!
trap 'kill $sim 2>/dev/null' EXIT
# After ktestd's own start-up lines, so that it is the last thing on screen.
(sleep 1; printf '\n  Open https://localhost:8443/ in Chrome and log in with this token:\n\n    %s\n\n' "$(cat token)") &
bin/ktestd -https :8443 -tokens tokens -panel rack.json -limits limits.json \
    -psu psu1=127.0.0.1:5025,2 -sock /tmp/ktest-demo.sock -o demo.logb vcan0
