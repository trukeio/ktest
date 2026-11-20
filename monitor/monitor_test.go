package monitor

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/channel"
)

const ms = int64(1e6)

// fld is a test field: float64 unless bool, at 8-byte slots after the axis.
type fld struct {
	name, unit string
	bool, held bool
	text       bool
}

func schema(name string, fs ...fld) *logb.Schema {
	s := &logb.Schema{Name: name, AxisKind: logb.AxisTime, AxisMode: logb.AxisExplicit, AxisExp: -9, AxisUnit: "s",
		AxisScale: logb.TickVal(1),
		Fields:    []logb.Field{{Name: "t", BitOffset: 0, BitWidth: 64, Type: logb.TypeSint, Unit: "ns"}}}
	for i, f := range fs {
		lf := logb.Field{Name: f.name, BitOffset: uint32(64 * (i + 1)), BitWidth: 64, Type: logb.TypeFloat, Unit: f.unit, Hold: f.held}
		if f.bool {
			lf.Type, lf.BitWidth, lf.Unit = logb.TypeBool, 1, ""
		}
		if f.text {
			lf.Type, lf.Unit = logb.TypeString, ""
		}
		s.Fields = append(s.Fields, lf)
	}
	s.RecordBits = uint32(64 * (len(fs) + 1))
	return s
}

// rec makes a record of s with the given values, in field order after t.
func rec(s *logb.Schema, vals ...any) []byte {
	b := make([]byte, s.RecordBytes())
	for i, v := range vals {
		at := 8 * (i + 1)
		switch x := v.(type) {
		case float64:
			binary.LittleEndian.PutUint64(b[at:], math.Float64bits(x))
		case int:
			binary.LittleEndian.PutUint64(b[at:], math.Float64bits(float64(x)))
		case bool:
			if x {
				b[at] = 1
			}
		case string:
			copy(b[at:at+8], x)
		}
	}
	return b
}

var (
	meas   = schema("psu1.ch1.meas", fld{name: "v", unit: "V"}, fld{name: "i", unit: "A"})
	setpt  = schema("psu1.ch1", fld{name: "v.set", unit: "V", held: true})
	status = schema("DutStatus", fld{name: "counter"}, fld{name: "mode", text: true})
	engine = schema("EngineData", fld{name: "EngineSpeed", unit: "rpm"})
)

func table() *channel.Table {
	t := channel.NewTable()
	for _, s := range []*logb.Schema{meas, setpt, status, engine} {
		t.Stream(s)
	}
	return t
}

func load(t *testing.T, js string) (*Contract, error) {
	t.Helper()
	return parse("dut-interface.json", []byte(js), table())
}

func mustLoad(t *testing.T, js string) *Contract {
	t.Helper()
	c, err := load(t, js)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func evaluator(t *testing.T, c *Contract) *Evaluator {
	t.Helper()
	e, err := NewEvaluator(c)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// script is a replay: records and modes in time order, collecting every
// transition.
type step struct {
	t    int64
	s    *logb.Schema
	vals []any
	mode *string
	adv  bool
}

func play(t *testing.T, e *Evaluator, steps []step) []Transition {
	t.Helper()
	var out []Transition
	for _, st := range steps {
		var tr []Transition
		var err error
		switch {
		case st.adv:
			tr, err = e.Advance(st.t)
		case st.mode != nil:
			tr, err = e.Mode(st.t, *st.mode)
		default:
			tr, err = e.Record(st.s.Name, st.t, rec(st.s, st.vals...))
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tr...)
	}
	return out
}

func str(s string) *string { return &s }

// brief writes transitions as "t monitor from>to", t in ns.
func brief(tr []Transition) []string {
	var out []string
	for _, x := range tr {
		out = append(out, fmt.Sprintf("%d %s %s>%s", x.T, x.Monitor, x.From, x.To))
	}
	return out
}

func expectTr(t *testing.T, got []Transition, want ...string) {
	t.Helper()
	if g := brief(got); strings.Join(g, "\n") != strings.Join(want, "\n") {
		t.Errorf("transitions\n got %s\nwant %s", strings.Join(g, "\n     "), strings.Join(want, "\n     "))
	}
}

// The handoff's example contract (§4.2), with the alive message a decoded
// stream and its counter wrapping at 16.
const example = `{
  "contract": "acme-bcm-v3",
  "schema": 1,
  "inputs":   {"Vx": "psu1.ch1.meas.v", "Speed": "EngineData.EngineSpeed", "Alive": "DutStatus"},
  "monitors": {
    "vx":    {"ok": "Vx > 1.0V"},
    "speed": {"ok": "abs(Speed - 3000) <= 30", "debounce": "20ms", "modes": ["active"]},
    "alive": {"ok": "age(Alive) < 30ms and delta(Alive.counter, 16) == 1"},
    "abort": {"ok": "Vx < 16V"},
    "ready": {"ok": "vx and speed and alive"}
  },
  "modes":    {"active": {"action": "enter_active", "settle": "500ms"}},
  "resource": "actions.robot",
  "actions":  {"enter_active": "Enter Active", "recover": "Recover"},
  "enums":    {"class": ["I", "II", "III", "IV", "V"]},
  "timeouts": {"ready": "10s", "recover": "5s"}
}`

const actions = `*** Keywords ***
Enter Active
    Log    active
Recover
    Log    recover
`

func TestExampleContract(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "dut-interface.json")
	os.WriteFile(p, []byte(example), 0o644)
	os.WriteFile(filepath.Join(dir, "actions.robot"), []byte(actions), 0o644)
	c, err := Load(p, table())
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "acme-bcm-v3" || len(c.Hash) != 64 || len(c.ResourceHash) != 64 {
		t.Errorf("%s %s %s", c.Name, c.Hash, c.ResourceHash)
	}
	// ready reads the three it is made of, so it comes after them.
	ri := indexOf(c.Order, "ready")
	for _, d := range []string{"vx", "speed", "alive"} {
		if indexOf(c.Order, d) > ri {
			t.Errorf("order %v: %s after ready", c.Order, d)
		}
	}
	if got := c.Monitors["alive"].Inputs; !reflect.DeepEqual(got, []string{"Alive"}) {
		t.Errorf("alive reads %v", got)
	}
	if c.Timeouts["ready"].Seconds() != 10 || c.Modes["active"].Settle.Milliseconds() != 500 {
		t.Errorf("%v %v", c.Timeouts, c.Modes["active"])
	}
	e := evaluator(t, c)
	if got := e.Streams(); !reflect.DeepEqual(got, []string{"DutStatus", "EngineData", "psu1.ch1.meas"}) {
		t.Errorf("streams %v", got)
	}
}

func indexOf(s []string, x string) int {
	for i, v := range s {
		if v == x {
			return i
		}
	}
	return -1
}

func TestContractRefusals(t *testing.T) {
	base := func(mons string) string {
		return `{"contract": "c", "schema": 1, "inputs": {"Vx": "psu1.ch1.meas.v", "Set": "psu1.ch1.v.set", "Alive": "DutStatus"}, "monitors": {` + mons + `}}`
	}
	for _, c := range []struct{ name, js, want string }{
		{"unknown key", `{"contract": "c", "schema": 1, "inputz": {}}`, `unknown field "inputz"`},
		{"schema", `{"contract": "c", "schema": 2, "inputs": {"Vx": "psu1.ch1.meas.v"}, "monitors": {"a": {"ok": "True"}}}`, `"schema" is 2, and this reads schema 1`},
		{"no name", `{"schema": 1}`, `"contract" is required`},
		{"no monitors", `{"contract": "c", "schema": 1, "inputs": {"Vx": "psu1.ch1.meas.v"}}`, `no "monitors" would watch nothing`},
		{"unknown input", `{"contract": "c", "schema": 1, "inputs": {"Vx": "psu9.ch1.meas.v"}, "monitors": {"a": {"ok": "True"}}}`,
			"input Vx: psu9.ch1.meas.v is not a channel here"},
		{"unknown input key", `{"contract": "c", "schema": 1, "inputs": {"Vx": {"chanel": "x"}}, "monitors": {"a": {"ok": "True"}}}`, `unknown field "chanel"`},
		{"unknown name", base(`"a": {"ok": "Vy > 1V"}`), "Vy is not an input or a monitor of this contract; the inputs are Alive, Set, Vx"},
		{"units", base(`"a": {"ok": "Vx > 30ms"}`), `monitor a: "Vx > 30ms": column 4: >: V and ms are not the same kind of quantity`},
		{"cycle", base(`"a": {"ok": "b and Vx > 1V"}, "b": {"ok": "c"}, "c": {"ok": "a"}`), "monitors read each other in a circle: a → b → c → a"},
		{"self", base(`"a": {"ok": "a"}`), "in a circle: a → a"},
		{"reserved", base(`"watermark": {"ok": "True"}`), "mon.watermark is the evaluator's own channel"},
		{"input's name", base(`"Vx": {"ok": "True"}`), "monitor Vx has the name of an input"},
		{"no ok", base(`"a": {"debounce": "1ms"}`), `monitor a: "ok" is required`},
		{"debounce", base(`"a": {"ok": "True", "debounce": "-1ms"}`), "a duration here is longer than zero"},
		{"debounce unit", base(`"a": {"ok": "True", "debounce": "5V"}`), `"5V" is not a duration`},
		{"unknown mode", base(`"a": {"ok": "True", "modes": ["active"]}`), `monitor a watches mode "active", which "modes" does not declare`},
		{"stream as value", base(`"a": {"ok": "Alive > 1"}`), "compares a stream"},
		{"no such field", base(`"a": {"ok": "Alive.count == 1"}`), "Alive.count: Alive is DutStatus, which has no field count"},
		{"max_age on held", `{"contract": "c", "schema": 1, "inputs": {"Set": {"channel": "psu1.ch1.v.set", "max_age": "1s"}}, "monitors": {"a": {"ok": "True"}}}`,
			"psu1.ch1.v.set is held"},
		{"enum dup", `{"contract": "c", "schema": 1, "inputs": {"Vx": "psu1.ch1.meas.v"}, "monitors": {"a": {"ok": "True"}}, "enums": {"e": ["a", "a"]}}`,
			`enum e has "a" twice`},
		{"enum on a number", `{"contract": "c", "schema": 1, "inputs": {"Vx": {"channel": "psu1.ch1.meas.v", "enum": "e"}}, "monitors": {"a": {"ok": "True"}}, "enums": {"e": ["a"]}}`,
			"enum e is read from text, and psu1.ch1.meas.v is a number in V"},
		{"actions without resource", `{"contract": "c", "schema": 1, "inputs": {"Vx": "psu1.ch1.meas.v"}, "monitors": {"a": {"ok": "True"}}, "actions": {"x": "X"}}`,
			`"actions" are keywords of a "resource" file`},
		{"mode action", `{"contract": "c", "schema": 1, "inputs": {"Vx": "psu1.ch1.meas.v"}, "monitors": {"a": {"ok": "True"}}, "modes": {"m": {"action": "go"}}}`,
			`mode m enters with action "go", which "actions" does not declare`},
		{"timeout name", `{"contract": "c", "schema": 1, "inputs": {"Vx": "psu1.ch1.meas.v"}, "monitors": {"a": {"ok": "True"}}, "timeouts": {"zz": "1s"}}`,
			"timeout zz: a timeout is for a monitor, a mode or an action"},
		{"not a predicate", base(`"a": {"ok": "Vx"}`), "a monitor expression must be true or false"},
	} {
		_, err := load(t, c.js)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s:\n got %v\nwant %q", c.name, err, c.want)
		}
	}
}

const one = `{"contract": "c", "schema": 1, "inputs": {"Vx": {"channel": "psu1.ch1.meas.v", "max_age": "1s"}},
  "monitors": {"vx": {"ok": "Vx > 1V", "debounce": "20ms"}}}`

// TestDebounce at its edges: a deviation must outlast its debounce, and so
// must the return to ok, each stamped at the moment the debounce ran out.
func TestDebounce(t *testing.T) {
	e := evaluator(t, mustLoad(t, one))
	tr := play(t, e, []step{
		{t: 0, s: meas, vals: []any{5.0}},
		{t: 20 * ms, s: meas, vals: []any{5.0}}, // ok once true for 20 ms
		// Low for 20 ms less a nanosecond: no deviation.
		{t: 100 * ms, s: meas, vals: []any{0.5}},
		{t: 120*ms - 1, s: meas, vals: []any{5.0}},
		// Low, and a high sample exactly at the end of the debounce: the
		// sample at that instant counts, so the deviation did not outlast it.
		{t: 200 * ms, s: meas, vals: []any{0.5}},
		{t: 220 * ms, s: meas, vals: []any{5.0}},
		// Low, and the next sample a nanosecond after: dev, stamped at the
		// deadline, not at the sample.
		{t: 300 * ms, s: meas, vals: []any{0.5}},
		{t: 320*ms + 1, s: meas, vals: []any{5.0}},
		// Back to ok once high for 20 ms, with no sample at that instant.
		{t: 400 * ms, adv: true},
	})
	expectTr(t, tr,
		"20000000 vx unknown>ok",
		"320000000 vx ok>dev",
		"340000001 vx dev>ok")
}

// TestStaleness: a sampled input goes stale at 3 × its median period, to the
// nanosecond, and its monitors go unknown; a held one never does.
func TestStaleness(t *testing.T) {
	c := mustLoad(t, `{"contract": "c", "schema": 1, "inputs": {"Vx": "psu1.ch1.meas.v", "Set": "psu1.ch1.v.set"},
	  "monitors": {"vx": {"ok": "Vx > 1V"}, "set": {"ok": "Set < 12V"}}}`)
	e := evaluator(t, c)
	tr := play(t, e, []step{
		{t: 0, s: setpt, vals: []any{5.0}},
		{t: 0, s: meas, vals: []any{5.0}}, // seen once: no period yet, so not usable
		{t: 10 * ms, s: meas, vals: []any{5.0}},
		{t: 20 * ms, s: meas, vals: []any{5.0}},
		{t: 31 * ms, s: meas, vals: []any{5.0}}, // intervals 10, 10, 11: median 10
		{t: 10_000 * ms, adv: true},
	})
	expectTr(t, tr,
		"0 set unknown>ok",
		"10000000 vx unknown>ok",
		"61000001 vx ok>unknown") // 31 ms + 3 × 10 ms, and a nanosecond past it
}

// TestStoppedStream: a monitor that watches a stream stop keeps saying dev
// while it has stopped. The stream going stale makes its values unknown; the
// age alone already answers false.
func TestStoppedStream(t *testing.T) {
	c := mustLoad(t, `{"contract": "c", "schema": 1, "inputs": {"Alive": "DutStatus"},
	  "monitors": {"alive": {"ok": "age(Alive) < 30ms and delta(Alive.counter, 16) == 1"}}}`)
	e := evaluator(t, c)
	var steps []step
	for i := 0; i < 6; i++ {
		steps = append(steps, step{t: int64(i) * 10 * ms, s: status, vals: []any{float64((14 + i) % 16)}})
	}
	steps = append(steps, step{t: time10s, adv: true})
	tr := play(t, e, steps)
	// The first message's values are not usable until a second gives the
	// stream a period, so the first delta is unknown: ok from the third. The
	// last is at 50 ms, so age reaches 30 ms at 80 ms exactly, and the
	// monitor stays dev after the stream goes stale.
	expectTr(t, tr, "20000000 alive unknown>ok", "80000000 alive ok>dev")
	if e.State("alive") != Dev {
		t.Errorf("alive is %s at 10 s", e.State("alive"))
	}
}

const time10s = 10_000 * ms

// TestModes: inactive outside its modes, settling for the mode's settle,
// judged after, stamped at the settle's end.
func TestModes(t *testing.T) {
	c := mustLoad(t, `{"contract": "c", "schema": 1, "inputs": {"Speed": {"channel": "EngineData.EngineSpeed", "max_age": "1s"}},
	  "monitors": {"speed": {"ok": "abs(Speed - 3000) <= 30", "modes": ["active"]}, "any": {"ok": "Speed < 9000"}},
	  "modes": {"active": {"settle": "500ms"}, "idle": {}}}`)
	e := evaluator(t, c)
	tr := play(t, e, []step{
		{t: 0, s: engine, vals: []any{0.0}},
		{t: 100 * ms, mode: str("active")},
		{t: 200 * ms, s: engine, vals: []any{3010.0}},
		{t: 700 * ms, adv: true},
		{t: 800 * ms, s: engine, vals: []any{2900.0}},
		{t: 900 * ms, mode: str("idle")},
	})
	expectTr(t, tr,
		"0 any unknown>ok", // monitors are taken in dependency order, then by name
		"0 speed unknown>inactive",
		"100000000 speed inactive>settling",
		"600000000 speed settling>ok",
		"800000000 speed ok>dev",
		"900000000 speed dev>inactive")
}

// TestDependents: a monitor reading another sees it true while ok, false
// while dev, and unknown otherwise, in the same instant.
func TestDependents(t *testing.T) {
	c := mustLoad(t, `{"contract": "c", "schema": 1, "inputs": {"Vx": {"channel": "psu1.ch1.meas.v", "max_age": "1s"}},
	  "monitors": {"vx": {"ok": "Vx > 1V"}, "ready": {"ok": "vx"}}}`)
	e := evaluator(t, c)
	tr := play(t, e, []step{{t: 0, s: meas, vals: []any{5.0}}, {t: 5 * ms, s: meas, vals: []any{0.0}}})
	expectTr(t, tr, "0 vx unknown>ok", "0 ready unknown>ok", "5000000 vx ok>dev", "5000000 ready ok>dev")
}

// TestPurity: the same records give the same transitions, however often the
// evaluator is told time has passed in between.
func TestPurity(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "dut-interface.json")
	os.WriteFile(p, []byte(example), 0o644)
	os.WriteFile(filepath.Join(dir, "actions.robot"), []byte(actions), 0o644)
	c, err := Load(p, table())
	if err != nil {
		t.Fatal(err)
	}
	for seed := uint64(0); seed < 10; seed++ {
		rng := rand.New(rand.NewPCG(seed, 2))
		var steps []step
		now := int64(0)
		cnt := 0
		for k := 0; k < 2000; k++ {
			now += int64(rng.IntN(int(12 * ms)))
			switch rng.IntN(5) {
			case 0:
				steps = append(steps, step{t: now, s: meas, vals: []any{rng.Float64() * 20}})
			case 1:
				steps = append(steps, step{t: now, s: engine, vals: []any{2950 + rng.Float64()*100}})
			case 2:
				if rng.IntN(20) != 0 {
					cnt++
				}
				steps = append(steps, step{t: now, s: status, vals: []any{float64(cnt % 16)}})
			case 3:
				if rng.IntN(30) == 0 {
					steps = append(steps, step{t: now, mode: str([]string{"", "active"}[rng.IntN(2)])})
				}
			}
		}
		a := play(t, evaluator(t, c), steps)
		// The same, with an Advance before every step.
		var withAdv []step
		for _, s := range steps {
			withAdv = append(withAdv, step{t: s.t, adv: true}, s)
		}
		b := play(t, evaluator(t, c), withAdv)
		if !reflect.DeepEqual(brief(a), brief(b)) {
			t.Fatalf("seed %d: Advance changed the transitions:\n%v\n%v", seed, brief(a), brief(b))
		}
		if !reflect.DeepEqual(a, play(t, evaluator(t, c), steps)) {
			t.Fatalf("seed %d: two replays differ", seed)
		}
		if len(a) < 10 {
			t.Fatalf("seed %d: only %d transitions; the test exercises nothing", seed, len(a))
		}
		for i := 1; i < len(a); i++ {
			if a[i].T < a[i-1].T {
				t.Fatalf("seed %d: transitions out of order: %v then %v", seed, a[i-1], a[i])
			}
		}
	}
}

func TestOutOfOrder(t *testing.T) {
	e := evaluator(t, mustLoad(t, one))
	play(t, e, []step{{t: 10 * ms, s: meas, vals: []any{5.0}}})
	if _, err := e.Record(meas.Name, 5*ms, rec(meas, 5.0)); err == nil {
		t.Error("an earlier record was taken")
	}
}
