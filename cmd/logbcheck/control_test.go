package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// The control check is exercised on records made up for the purpose, because
// a daemon that keeps to its limits cannot be made to produce the recording
// that ought to fail it. Each case breaks one rule and the check must say so;
// the first breaks none and the check must pass it, since a checker that
// fails everything proves as little as one that fails nothing.
func TestControlCheck(t *testing.T) {
	good := func() *recording {
		return &recording{
			meta: map[string]string{"limits.cyclic.min_period": "1ms"},
			control: []ctlRec{
				{t: 0.0, seq: 1, request: rqLeaseTake, leased: true, ttl: 10},
				{t: 0.1, seq: 2, request: rqArm, armed: true, leased: true, ttl: 10},
				{t: 0.2, seq: 3, request: rqCyclicSet, period: 0.010, armed: true, leased: true, ttl: 10},
				{t: 0.3, seq: 4, request: rqCyclicSet, verdict: vBelowFloor, period: 0.0005, armed: true, leased: true, ttl: 10},
				{t: 1.0, seq: 5, request: rqDisarm, leased: true, ttl: 10},
				{t: 2.0, seq: 6, request: rqLeaseRelease},
			},
			cyclic: map[string][]cycRec{
				"400": {
					{t: 0.2, seq: 3, event: evStart, active: true},
					{t: 1.001, seq: 5, event: evDisarmed},
				},
			},
			tasks: []string{"400"},
		}
	}

	for _, tc := range []struct {
		name   string
		mutate func(r *recording)
		want   string // "" for a recording that must pass
	}{
		{"a recording that keeps to its limits", func(r *recording) {}, ""},
		{"a send accepted unarmed", func(r *recording) {
			r.control = append(r.control[:2:2], ctlRec{t: 0.15, seq: 9, request: rqSend, leased: true, ttl: 10})
			r.control = append(r.control, good().control[2:]...)
		}, "accepted unarmed"},
		{"a period below the floor accepted", func(r *recording) { r.control[2].period = 0.0005 }, "under a floor"},
		{"a refusal the state does not justify", func(r *recording) { r.control[3].period = 0.002 }, "refused under a floor"},
		{"a refusal as unarmed while armed", func(r *recording) {
			r.control[3].verdict = vNotArmed
		}, "refused as not armed"},
		{"a disarm that left a task running", func(r *recording) { r.cyclic["400"][1].t = 1.2 }, "not stopped"},
		{"a task stopped under another number", func(r *recording) { r.cyclic["400"][1].seq = 4 }, "not stopped"},
		{"a lease that lapsed without expiring", func(r *recording) {
			r.control[0].ttl = 0.5
		}, "is not its expiry"},
		{"a lease that expired late", func(r *recording) {
			r.control[0].ttl = 0.5
			for i := 1; i < len(r.control); i++ {
				r.control[i].ttl = 0.5
			}
			r.control[4] = ctlRec{t: 1.0, seq: 0, request: rqLeaseExpired}
			r.cyclic["400"][1].seq = 0
		}, "expired only at"},
		{"a task running while the bus was disarmed", func(r *recording) { r.cyclic["400"][0].t = 0.05 }, "running at"},
	} {
		t.Run(tc.name, func(t *testing.T) { checkPlanted(t, good, tc.mutate, tc.name, tc.want) })
	}
}

// TestControlCheckIdentities is the same exercise for the network listener:
// who may act, and whether the name using a lease is the name that took it.
func TestControlCheckIdentities(t *testing.T) {
	good := func() *recording {
		return &recording{
			meta: map[string]string{
				"limits.cyclic.min_period": "1ms",
				"auth.identities":          "dash:view, eve:operate, rolf:operate",
			},
			control: []ctlRec{
				{t: 0.0, seq: 1, request: rqLeaseTake, leased: true, ttl: 10, holder: "rolf", by: "rolf", via: viaTLS},
				{t: 0.1, seq: 2, request: rqArm, armed: true, leased: true, ttl: 10, holder: "rolf", by: "rolf", via: viaTLS},
				{t: 0.2, seq: 3, request: rqSend, armed: true, leased: true, ttl: 10, holder: "rolf", by: "rolf", via: viaTLS},
				{t: 0.3, seq: 4, request: rqSend, verdict: vForbidden, armed: true, leased: true, ttl: 10,
					holder: "rolf", by: "dash", via: viaTLS},
				{t: 0.4, seq: 5, request: rqDisarm, leased: true, ttl: 10, holder: "rolf", by: "dash", via: viaTLS},
				{t: 0.5, seq: 6, request: rqLeaseRelease, by: "rolf", via: viaTLS},
			},
		}
	}

	for _, tc := range []struct {
		name   string
		mutate func(r *recording)
		want   string
	}{
		{"a recording that keeps to its roles", func(r *recording) {}, ""},
		{"a view name's send accepted", func(r *recording) { r.control[3].verdict = vAccepted }, "whose role is view"},
		{"an operate name refused as not permitted", func(r *recording) { r.control[3].by = "eve" }, "refused as not permitted"},
		{"a name the recording does not list", func(r *recording) { r.control[2].by = "mallory" }, "does not list"},
		{"a lease used by a name that does not hold it", func(r *recording) { r.control[2].by = "eve" }, "under a lease held by"},
		{"a role refusal on the local socket", func(r *recording) { r.control[3].via = 1 }, "no roles"},
	} {
		t.Run(tc.name, func(t *testing.T) { checkPlanted(t, good, tc.mutate, tc.name, tc.want) })
	}
}

// TestCyclicSetsAsideSingleSends: a single send of a task's message is the
// daemon's, recorded in tx.applied, and not a frame the task put on the bus.
// Without the echo that says so, the same frame is one the task has to answer
// for, and the check must fail it.
func TestCyclicSetsAsideSingleSends(t *testing.T) {
	good := func() *recording {
		return &recording{
			raw: []frame{
				{t: 0.10, id: 0x100, data: []byte{1}, local: true}, // the single send
				{t: 0.20, id: 0x100, data: []byte{2}, local: true}, // the task, every 10 ms
				{t: 0.21, id: 0x100, data: []byte{2}, local: true},
				{t: 0.22, id: 0x100, data: []byte{2}, local: true},
			},
			applied: []txRec{{frame: frame{t: 0.10, id: 0x100, data: []byte{1}}, seq: 1}},
			cyclic: map[string][]cycRec{"100": {
				{t: 0.195, seq: 2, id: 0x100, event: evStart, active: true, data: []byte{2}, period: 0.01,
					applied: true, aData: []byte{2}, aPeriod: 0.01},
				{t: 0.225, seq: 3, id: 0x100, event: evStop, data: []byte{2}, period: 0.01},
			}},
			tasks: []string{"100"},
		}
	}
	if _, err := compareCyclic(good(), true); err != nil {
		t.Fatalf("the single send was counted against the task:\n%v", err)
	}
	r := good()
	r.applied = nil
	if _, err := compareCyclic(r, true); err == nil || !strings.Contains(err.Error(), "before the task began") {
		t.Fatalf("a frame with no echo to account for it: %v, want it laid at the task's door", err)
	}
}

// TestControlCheckSupply is the same exercise for a supply: its own lease and
// arming, its limits, its outputs off at a stop, and a stop recorded for every
// instrument at once.
func TestControlCheckSupply(t *testing.T) {
	good := func() *recording {
		return &recording{
			meta: map[string]string{
				"limits.cyclic.min_period": "1ms",
				"psu.psu1.channels":        "1",
				"limits.psu1.ch1.v_max":    "12",
				"limits.psu1.ch1.i_max":    "none",
				// An output has no maximum, and the recording says so rather
				// than leaving the key out: "none" and "the daemon forgot to
				// state one" are different, and a checker has to tell them
				// apart.
				"limits.psu1.ch1.out_max": "none",
			},
			control: []ctlRec{
				{t: 1.0, seq: 6, request: rqStop},
			},
			controls: map[string][]ctlRec{"psu1": {
				{t: 0.0, seq: 1, request: rqLeaseTake, leased: true, ttl: 10},
				{t: 0.1, seq: 2, request: rqArm, armed: true, leased: true, ttl: 10},
				{t: 0.2, seq: 3, request: rqChannelSet, armed: true, leased: true, ttl: 10, channel: "psu1.ch1.v.set", value: 5},
				{t: 0.3, seq: 4, request: rqChannelSet, verdict: vAboveLimit, armed: true, leased: true, ttl: 10,
					channel: "psu1.ch1.v.set", value: 20},
				{t: 0.4, seq: 5, request: rqChannelSet, armed: true, leased: true, ttl: 10, channel: "psu1.ch1.out.set", value: 1},
				{t: 1.0, seq: 6, request: rqStop, leased: true, ttl: 10},
				{t: 2.0, seq: 7, request: rqLeaseRelease},
			}},
			psu: map[string][]psuRec{"psu1.ch1": {
				{t: 0.0, event: psConnected, connected: true},
				{t: 0.2, seq: 3, event: psRequested, connected: true},
				{t: 0.201, seq: 3, event: psApplied, connected: true},
				{t: 0.4, seq: 5, event: psRequested, connected: true},
				{t: 0.401, seq: 5, event: psApplied, connected: true, outApplied: true},
				{t: 1.001, seq: 6, event: psOff, connected: true},
			}},
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(r *recording)
		want   string
	}{
		{"a recording that keeps to its limits", func(r *recording) {}, ""},
		{"a write accepted above its limit", func(r *recording) { r.controls["psu1"][2].value = 13 }, "above its limit"},
		{"a refusal under the limit", func(r *recording) { r.controls["psu1"][3].value = 11 }, "does not state"},
		{"a refusal where there is no limit", func(r *recording) {
			r.controls["psu1"][3].channel = "psu1.ch1.i.set"
		}, "does not state"},
		{"a write to a channel with no stated limit", func(r *recording) {
			delete(r.meta, "limits.psu1.ch1.v_max")
		}, "states no limit"},
		{"a write accepted unarmed", func(r *recording) { r.controls["psu1"][2].armed = false }, "accepted unarmed"},
		{"a stop that left an output on", func(r *recording) { r.psu["psu1.ch1"][5].t = 2.5 }, "not off within"},
		{"an output switched off by a lease release", func(r *recording) {
			r.psu["psu1.ch1"] = append(r.psu["psu1.ch1"], psuRec{t: 2.001, seq: 7, event: psOff, connected: true})
		}, "no disarm or stop"},
		{"a write the control plane never accepted", func(r *recording) { r.psu["psu1.ch1"][2].seq = 4 }, "accepted no write"},
		{"an accepted write never requested", func(r *recording) { r.psu["psu1.ch1"][1].seq = 0 }, "never recorded as requested"},
		{"a stop missing from the bus", func(r *recording) { r.control[0].seq = 9 }, "not for"},
	} {
		t.Run(tc.name, func(t *testing.T) { checkPlanted(t, good, tc.mutate, tc.name, tc.want) })
	}
}

// TestControlCheckPanel is the same exercise for the rack's panel: a revision
// for each save and none for a refusal, saves only from operate names, and
// the recording ending with the panel its last record names.
func TestControlCheckPanel(t *testing.T) {
	a, b := []byte(`{"widgets":[]}`), []byte(`{"title":"b","widgets":[]}`)
	sa, sb := sha256.Sum256(a), sha256.Sum256(b)
	shaA, shaB := hex.EncodeToString(sa[:]), hex.EncodeToString(sb[:])
	good := func() *recording {
		return &recording{
			meta: map[string]string{
				"limits.cyclic.min_period": "1ms",
				"auth.identities":          "dash:view, rolf:operate",
				"panel.source":             "bench.json",
			},
			control: []ctlRec{{t: 9, request: rqShutdown}},
			panel: []panelRec{
				{t: 0, rev: 1, sha: shaA},
				{t: 1, seq: 1, via: viaTLS, rev: 2, sha: shaB, savedBy: "rolf", by: "rolf"},
				{t: 2, seq: 2, verdict: vForbidden, via: viaTLS, rev: 2, sha: shaB, savedBy: "rolf", by: "dash"},
				{t: 3, seq: 3, verdict: pnStale, via: viaTLS, rev: 2, sha: shaB, savedBy: "rolf", by: "rolf"},
			},
			panelAttach: b,
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(r *recording)
		want   string
	}{
		{"a recording that keeps to its rules", func(r *recording) {}, ""},
		{"a view name's save accepted", func(r *recording) {
			r.panel[2].verdict, r.panel[2].rev = vAccepted, 3
		}, "whose role is view"},
		{"an operate name refused as not permitted", func(r *recording) { r.panel[2].by = "rolf" }, "refused as not permitted"},
		{"a refusal that changed the panel", func(r *recording) { r.panel[3].sha = shaA }, "changed the panel in force"},
		{"a save that skipped a revision", func(r *recording) {
			for i := 1; i < len(r.panel); i++ {
				r.panel[i].rev = 3
			}
		}, "made revision 3 of revision 1"},
		{"a save credited to another name", func(r *recording) {
			for i := 1; i < len(r.panel); i++ {
				r.panel[i].savedBy = "dash"
			}
		}, "recorded as saved by"},
		{"a recording ending with another panel", func(r *recording) { r.panelAttach = a }, "the panel.json the recording ends with"},
		{"a panel in force and none attached", func(r *recording) { r.panelAttach = nil }, "carries no panel.json"},
		{"a start revision the recording does not declare", func(r *recording) {
			delete(r.meta, "panel.source")
		}, "the daemon started with revision 1"},
		{"a save with no number", func(r *recording) { r.panel[1].seq = 0 }, "no request number"},
	} {
		t.Run(tc.name, func(t *testing.T) { checkPlanted(t, good, tc.mutate, tc.name, tc.want) })
	}
}

// TestSupplyTranscript: the recording's account of what the supply carried
// out, against the supply's own transcript, with one fault planted at a time.
func TestSupplyTranscript(t *testing.T) {
	r := &recording{
		meta: map[string]string{"psu.psu1.channels": "2"},
		controls: map[string][]ctlRec{"psu1": {
			{seq: 3, request: rqChannelSet, channel: "psu1.ch1.v.set", value: 5},
			{seq: 4, request: rqChannelSet, channel: "psu1.ch2.out.set", value: 1},
			{seq: 5, request: rqStop},
		}},
		psu: map[string][]psuRec{
			"psu1.ch1": {{t: 0.1, seq: 3, event: psApplied}, {t: 0.5, seq: 5, event: psOff}},
			"psu1.ch2": {{t: 0.2, seq: 4, event: psApplied}, {t: 0.51, seq: 5, event: psOff}},
		},
	}
	tr := []scpiWrite{
		{what: "v", ch: 1, value: 5, line: 3},
		{what: "out", ch: 2, value: 1, line: 9},
		{what: "out", ch: 1, value: 0, line: 12},
		{what: "out", ch: 2, value: 0, line: 17},
	}
	if n, err := compareSupply(r, "psu1", tr, true); err != nil || n != 4 {
		t.Fatalf("a transcript that agrees: %d, %v", n, err)
	}
	for _, tc := range []struct {
		name string
		tr   []scpiWrite
		want string
	}{
		{"a value the supply was told differently", []scpiWrite{tr[0], {what: "out", ch: 2, value: 0, line: 9}, tr[2], tr[3]}, "write 2"},
		{"a write the recording does not know of", append(tr[:4:4], scpiWrite{what: "v", ch: 1, value: 9, line: 20}), "does not say"},
		{"a write the supply never received", tr[:3], "no more writes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compareSupply(r, "psu1", tc.tr, true)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want a failure naming %q", err, tc.want)
			}
		})
	}
}

// checkPlanted runs the control check on good() with one fault planted, and
// asserts it names that fault — or passes, when want is empty.
func checkPlanted(t *testing.T, good func() *recording, mutate func(*recording), name, want string) {
	t.Helper()
	r := good()
	mutate(r)
	_, _, err := compareControl(r, true)
	switch {
	case want == "" && err != nil:
		t.Fatalf("the check failed a recording that keeps to its limits:\n%v", err)
	case want != "" && err == nil:
		t.Fatalf("the check passed a recording with %s", name)
	case want != "" && !strings.Contains(err.Error(), want):
		t.Fatalf("the check failed, but not for the planted fault %q:\n%v", want, err)
	}
}
