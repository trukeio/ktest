//go:build linux

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/contract"
	"github.com/trukeio/ktest/peer"
	"github.com/trukeio/ktest/record"
)

// The rules are refused, with a sentence, wherever they would do nothing or
// something doubtful.
func TestCheckRules(t *testing.T) {
	id := &contract.Identity{Name: "c", Monitors: []string{"abort", "alive"}}
	insts := map[string]bool{"vcan1": true, "psu1": true}
	parse := func(js string) *rules {
		t.Helper()
		var l limits
		if err := json.Unmarshal([]byte(`{"rules": `+js+`}`), &l); err != nil {
			t.Fatal(err)
		}
		return l.Rules
	}
	good := `{"monitoring_lost": {"watermark_age": "100ms", "action": "remove_stress"},
	          "dut_abort": {"monitor": "abort", "action": "remove_stress"}, "remove_stress": ["psu1"]}`
	ids := []*contract.Identity{id}
	if err := checkRules(parse(good), true, insts, ids); err != nil {
		t.Fatalf("good rules refused: %v", err)
	}
	// With several racks, the abort monitor must be in every contract.
	other := &contract.Identity{Name: "d", Monitors: []string{"alive"}}
	if err := checkRules(parse(good), true, insts, []*contract.Identity{id, other}); err == nil ||
		!strings.Contains(err.Error(), "contract d does not declare") {
		t.Fatalf("an abort monitor one of two contracts lacks: %v", err)
	}
	if err := checkRules(nil, false, insts, nil); err != nil {
		t.Fatalf("no rules and no peer: %v", err)
	}
	for _, c := range []struct {
		name string
		js   string
		peer bool
		want string
	}{
		{"a peer and no rules", "", true, "-peer needs"},
		{"rules and no peer", good, false, "there is no -peer"},
		{"no monitoring_lost", `{"remove_stress": ["psu1"]}`, true, `"monitoring_lost" is required`},
		{"no watermark age", `{"monitoring_lost": {"action": "remove_stress"}, "remove_stress": ["psu1"]}`, true, "watermark_age"},
		{"another action", `{"monitoring_lost": {"watermark_age": "1s", "action": "shrug"}, "remove_stress": ["psu1"]}`, true, `"shrug"`},
		{"nothing to remove", `{"monitoring_lost": {"watermark_age": "1s", "action": "remove_stress"}}`, true, "names no instrument"},
		{"an instrument not here", `{"monitoring_lost": {"watermark_age": "1s", "action": "remove_stress"}, "remove_stress": ["gen1"]}`, true, `"gen1"`},
		{"a monitor the contract lacks", `{"monitoring_lost": {"watermark_age": "1s", "action": "remove_stress"},
		  "dut_abort": {"monitor": "panic", "action": "remove_stress"}, "remove_stress": ["psu1"]}`, true, `"panic"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var r *rules
			if c.js != "" {
				r = parse(c.js)
			}
			err := checkRules(r, c.peer, insts, ids)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want a refusal naming %s", err, c.want)
			}
		})
	}
}

// A mirrored record is the DUT rack's, shifted behind peer_t and restated,
// with each field where its offset says and each guard pointing at its field.
func TestMirrorSchema(t *testing.T) {
	from := record.Schema("mon.alive", 8*24, []logb.Field{
		{Name: "flag", BitOffset: 64, BitWidth: 1, Type: logb.TypeBool},
		{Name: "state", BitOffset: 72, BitWidth: 8, Type: logb.TypeUint, Hold: true},
		{Name: "v", BitOffset: 128, BitWidth: 64, Type: logb.TypeFloat, Guarded: true, GuardField: 1, GuardValue: 1},
	}, nil)
	m, err := mirrorSchema("dut", from)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "dut.mon.alive" || m.RecordBits != from.RecordBits+8*mrOverhead {
		t.Fatalf("%s, %d bits", m.Name, m.RecordBits)
	}
	rec := make([]byte, m.RecordBytes())
	src := make([]byte, from.RecordBytes())
	src[8] = 1 // flag
	src[9] = 3 // state
	putUint64(rec[mrPeerT:], 1_763_000_000_123_456_789)
	rec[mrFlags] = 1
	copy(rec[mrRecord:], src[8:])
	b := &logb.Batch{Schema: m, Count: 1, Data: rec}
	for name, want := range map[string]any{"peer_t": int64(1_763_000_000_123_456_789), "restated": true,
		"flag": true, "state": uint64(3)} {
		f := peer.Field(m, name)
		got, err := b.Raw(0, f)
		if err != nil || got != want {
			t.Errorf("%s: %v (%T), %v; want %v", name, got, got, err, want)
		}
	}
	if g := m.Fields[len(m.Fields)-1]; g.Name != "v" || g.GuardField != 3 || m.Fields[g.GuardField].Name != "flag" {
		t.Errorf("v's guard points at field %d, %s", g.GuardField, m.Fields[g.GuardField].Name)
	}
	if _, err := mirrorSchema("dut", &logb.Schema{Name: "x", AxisField: 2, Fields: []logb.Field{{}, {}, {}}}); err == nil {
		t.Error("a stream with its axis elsewhere was mirrored")
	}
}
