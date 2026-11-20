//go:build linux

package main

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/trukeio/ktest/channel"
	"github.com/trukeio/ktest/expr"
	"github.com/trukeio/ktest/monitor"
	"github.com/trukeio/ktest/procedure"
	"github.com/trukeio/ktest/procedure/lib"
)

// The procedure streams' layouts are offsets written in one place and read
// in another, as the scope's are; each record is written as the daemon
// writes it and read back through logb (readBack, in scope_test.go).

func TestProcRunRecordLayout(t *testing.T) {
	s := procRunSchema()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	rec := make([]byte, s.RecordBytes())
	putUint32(rec[prSeq:], 7)
	putUint32(rec[prRun:], 3)
	rec[prEvent], rec[prOutcome], rec[prVia], rec[prState] = runEnd, outcomeCode(procedure.Timeout), 2, 1|2
	putStr(rec, prTest, 64, "Sweep")
	putStr(rec, prFile, 64, "ramp.robot")
	putStr(rec, prHash, 64, strings.Repeat("ab", 32))
	putStr(rec, prLibrary, 32, lib.Version)
	putStr(rec, prContract, 64, "")
	putStr(rec, prMode, 16, "locked")
	putStr(rec, prLock, 64, strings.Repeat("cd", 32))
	putStr(rec, prBy, 32, "rolf")
	putStr(rec, prWhere, 64, "ramp.robot:12")
	putStr(rec, prMessage, 160, "'x' did not hold within 2s")
	got := readBack(t, s, rec)
	for k, want := range map[string]any{
		"seq": uint64(7), "run": uint64(3), "event": uint64(runEnd), "outcome": uint64(3), "via": uint64(2),
		"running": true, "accredited": true, "lock": strings.Repeat("cd", 32), "test": "Sweep", "file": "ramp.robot", "hash": strings.Repeat("ab", 32),
		"library": lib.Version, "contract": "", "mode": "locked", "by": "rolf", "where": "ramp.robot:12",
		"message": "'x' did not hold within 2s",
	} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
}

func TestProcStepRecordLayout(t *testing.T) {
	s := procStepSchema()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	rec := make([]byte, s.RecordBytes())
	putUint32(rec[stepRun:], 2)
	putUint32(rec[stepID:], 41)
	putUint32(rec[stepParent:], 40)
	rec[stepEvent], rec[stepOutcome] = 2, outcomeCode(procedure.Refused)
	start := time.Date(2026, 11, 19, 12, 0, 0, 5, time.UTC)
	putUint64(rec[stepStart:], uint64(start.UnixNano()))
	putStr(rec, stepKeyword, 64, "Stress Apply")
	putStr(rec, stepWhere, 64, "ramp.robot:9")
	if putStr(rec, stepArgs, 192, strings.Repeat("é", 200)) {
		rec[stepFlags] |= 1
	}
	putStr(rec, stepMessage, 160, "psu1.ch1.v.set: interlock")
	got := readBack(t, s, rec)
	for k, want := range map[string]any{
		"run": uint64(2), "id": uint64(41), "parent": uint64(40), "event": uint64(2), "outcome": uint64(4),
		"args_truncated": true, "message_truncated": false, "t_start": start.UnixNano(),
		"keyword": "Stress Apply", "where": "ramp.robot:9", "message": "psu1.ch1.v.set: interlock",
	} {
		if got[k] != want {
			t.Errorf("%s = %v (%T), want %v", k, got[k], got[k], want)
		}
	}
	// Cut at a character, not inside one: 96 two-byte é in 192 bytes.
	if a := got["args"].(string); a != strings.Repeat("é", 96) {
		t.Errorf("args cut to %d bytes: %q", len(a), a)
	}
}

func TestProcResultRecordLayout(t *testing.T) {
	s := procResultSchema()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		sr   lib.StepResult
		want map[string]any
	}{
		{lib.StepResult{Point: "dip", Level: procedure.Number(0.0135e3, expr.ParseUnit("V")), Result: "II"},
			map[string]any{"level.known": true, "threshold.known": false, "level": 13.5, "level_unit": "V",
				"point": "dip", "result": "II", "level_text": "13.5V"}},
		{lib.StepResult{Point: "x", Level: procedure.Str("high"), Result: "ok",
			Threshold: ptr(procedure.Number(0.25, expr.ParseUnit("mV")))},
			map[string]any{"level.known": false, "threshold.known": true, "threshold": 0.25, "threshold_unit": "mV",
				"level_text": "high"}},
	} {
		rec := make([]byte, s.RecordBytes())
		// What recordResult lays out, without a loop to write it.
		putStr(rec, rsPoint, 64, c.sr.Point)
		putStr(rec, rsResult, 64, c.sr.Result)
		putStr(rec, rsLevelText, 32, c.sr.Level.String())
		if c.sr.Level.Kind == procedure.VNumber {
			rec[rsFlags] |= 1
			putUint64(rec[rsLevel:], fbits(c.sr.Level.Num))
			putStr(rec, rsLevelUnit, 16, c.sr.Level.Unit.Name)
		}
		if th := c.sr.Threshold; th != nil {
			rec[rsFlags] |= 2
			putUint64(rec[rsThreshold:], fbits(th.Num))
			putStr(rec, rsThUnit, 16, th.Unit.Name)
		}
		got := readBack(t, s, rec)
		for k, want := range c.want {
			if got[k] != want {
				t.Errorf("%s = %v, want %v", k, got[k], want)
			}
		}
		// A guard that does not hold leaves the field out, rather than
		// reading as zero.
		if _, has := got["threshold"]; has != (c.sr.Threshold != nil) {
			t.Errorf("threshold present = %v", has)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// TestChanTable: names resolve to one stream and field, even where field
// names have dots in them, and a guarded field the latest record lacks has
// no value.
func TestChanTable(t *testing.T) {
	c := channel.NewTable()
	ch, meas := chanSchema("psu1", 1), measSchema("psu1", 1)
	c.Stream(ch)
	c.Stream(meas)

	for name, want := range map[string]expr.Type{
		"psu1.ch1.v.set":       expr.NumberIn("V"),
		"psu1.ch1.out.applied": {Kind: expr.Bool},
		"psu1.ch1.meas.v":      expr.NumberIn("V"),
		"psu1.ch1.meas.i":      expr.NumberIn("A"),
		"psu1.ch1.error":       {Kind: expr.String},
	} {
		got, err := c.Resolve(name)
		if err != nil || got.Kind != want.Kind || !got.Unit.SameDimension(want.Unit) {
			t.Errorf("%s: %v, %v", name, got, err)
		}
	}
	for _, name := range []string{"psu1.ch1.v", "psu2.ch1.meas.v", "psu1.ch1.t"} {
		if _, err := c.Resolve(name); err == nil || !strings.Contains(err.Error(), "is not a channel here") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A stream named whole is an event: samples, and no value.
	if typ, err := c.Resolve("psu1.ch1.meas"); err != nil || typ.Kind != expr.Event {
		t.Errorf("psu1.ch1.meas: %v, %v", typ, err)
	}
	if _, _, ok := c.Value("psu1.ch1.meas.v"); ok {
		t.Error("a value before any record")
	}

	rec := make([]byte, meas.RecordBytes())
	putUint64(rec[8:], fbits(11.5))
	at := time.Unix(100, 0)
	c.Record(meas, at, rec)
	rec[8] = 0xFF // the table keeps a copy, not the writer's buffer
	if v, ts, ok := c.Value("psu1.ch1.meas.v"); !ok || v.Num != 11.5 || ts != at.UnixNano() {
		t.Errorf("meas.v = %+v at %d, %v", v, ts, ok)
	}

	// A supply channel record with nothing set yet: v.set's guard is clear.
	crec := make([]byte, ch.RecordBytes())
	c.Record(ch, at, crec)
	if _, _, ok := c.Value("psu1.ch1.v.set"); ok {
		t.Error("v.set has a value nobody set")
	}
	if v, _, ok := c.Value("psu1.ch1.connected"); !ok || v.Bool {
		t.Errorf("connected = %+v, %v", v, ok)
	}

	// Two streams that both answer one name are refused, not guessed.
	a := measSchema("x", 1)
	a.Name = "x.y"
	a.Fields[1].Name = "z.v"
	b := measSchema("x", 1)
	b.Name = "x.y.z"
	c.Stream(a)
	c.Stream(b)
	if _, err := c.Resolve("x.y.z.v"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("x.y.z.v: %v", err)
	}
}

func fbits(v float64) uint64 { return math.Float64bits(v) }

// TestExampleProcedureLoads holds the supply bench's procedure to the daemon
// the demo starts: its channels, its supply, its library.
func TestExampleProcedureLoads(t *testing.T) {
	table := channel.NewTable()
	for ch := 1; ch <= 2; ch++ {
		table.Stream(chanSchema("psu1", ch))
		table.Stream(measSchema("psu1", ch))
	}
	d := &daemon{supplies: map[string]int{"psu1": 2}}
	p := newProcs(d, "../../examples/supply-bench/procedures", table)
	s, err := procedure.Load(p.dir+"/ramp.robot", procedure.Env{Lib: p.lib, Channels: table})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.Tests(), ","); got != "Ramp,Over The Limit" {
		t.Errorf("tests %s", got)
	}
	// And a target the daemon lacks is refused at load, by the daemon's own
	// account of its supplies.
	a := &rackAdapter{p: p}
	for target, want := range map[string]string{
		"psu1.ch3": "supply psu1 has channels ch1 to ch2",
		"psu2.ch1": "a stress target is a supply channel of this daemon",
		"psu1":     "a stress target is a supply channel of this daemon",
	} {
		if err := a.Stressor(target); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", target, err)
		}
	}
}

// TestTwoRacksProceduresLoad: the two-racks example's procedures load with the
// master's DUT keywords, checked against the example's own contract, and a
// name the contract lacks is refused by it.
func TestTwoRacksProceduresLoad(t *testing.T) {
	table := channel.NewTable()
	for ch := 1; ch <= 2; ch++ {
		table.Stream(chanSchema("psu1", ch))
		table.Stream(measSchema("psu1", ch))
	}
	id, err := monitor.Identify("../../examples/two-racks/dut/dut-interface.json")
	if err != nil {
		t.Fatal(err)
	}
	d := &daemon{supplies: map[string]int{"psu1": 2}}
	p := newProcs(d, "../../examples/two-racks/lab/procedures", table)
	dp := &dutPeer{v: newPeerView(), id: id}
	lib.AddDUT(p.lib, &rackAdapter{p: p}, dp)
	for file, tests := range map[string]string{"sweep.robot": "Sweep,Threshold", "hold.robot": "Hold"} {
		s, err := procedure.Load(p.dir+"/"+file, procedure.Env{Lib: p.lib, Channels: table})
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if got := strings.Join(s.Tests(), ","); got != tests {
			t.Errorf("%s: tests %s", file, got)
		}
	}
	if err := dp.Monitor("heart"); err == nil || !strings.Contains(err.Error(), "abort, alive, speed, supply") {
		t.Errorf("a monitor the contract lacks: %v", err)
	}
}
