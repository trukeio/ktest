package expr

import (
	"math/rand/v2"
	"strings"
	"testing"
)

const ms = int64(1e6)

var class = &EnumType{Name: "class", Values: []string{"I", "II", "III", "IV", "V"}}

// names is the vocabulary of the tests: the contract example's inputs, plus a
// channel in mV, an enum, a string and a procedure variable.
var names = Names{
	"Vx":            NumberIn("V"),
	"Vmv":           NumberIn("mV"),
	"Speed":         NumberIn("rpm"),
	"Alive":         NumberIn(""),
	"Alive.counter": NumberIn(""),
	"I":             NumberIn("A"),
	"T":             NumberIn("°C"),
	"cls":           {Kind: Enum, Enum: class},
	"mode":          {Kind: String},
	"ok":            {Kind: Bool},
	"$level":        NumberIn("V"),
	"$n":            NumberIn(""),
}

func compile(t *testing.T, src string, p Profile) *Program {
	t.Helper()
	prog, err := Compile(src, p, names)
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	return prog
}

func observe(t *testing.T, p *Program, name string, at int64, v Value) {
	t.Helper()
	i, ok := p.Slot(name)
	if !ok {
		t.Fatalf("%s does not read %s", p.Source(), name)
	}
	if err := p.Observe(i, at, v); err != nil {
		t.Fatal(err)
	}
}

func TestPrecedenceAndUnits(t *testing.T) {
	in := map[string]Value{
		"Vx": Num(1.2), "Vmv": Num(600), "Speed": Num(3020), "I": Num(0.5), "T": Num(25),
		"cls": StrValue("II"), "mode": StrValue("active"), "ok": BoolValue(true), "$level": Num(12), "$n": Num(3),
	}
	for _, c := range []struct {
		src  string
		want bool
	}{
		// Precedence, as the grammar orders it.
		{"1 + 2 * 3 == 7", true},
		{"(1 + 2) * 3 == 9", true},
		{"10 - 2 - 3 == 5", true},
		{"8 / 2 / 2 == 2", true},
		{"-2 * 3 == -6", true},
		{"not 1 < 2", false},
		{"True or False and False", true},
		{"(True or False) and False", false},
		{"not False and False", false},
		// Units, normalised to SI.
		{"500mV == 0.5V", true},
		{"1kOhm == 1000Ohm", true},
		{"1kΩ == 1000Ohm", true},
		{"1V * 1A == 1W", true},
		{"1V / 2A == 0.5Ohm", true},
		{"1 / 1ms == 1kHz", true},
		{"2 * 15ms == 30ms", true},
		{"1.5e3mV == 1.5V", true},
		{"50% == 50", true},
		{"-3dB < 0dB", true},
		{"T > 20°C", true},
		// A bare number takes the unit, and the scale, of what it meets.
		{"abs(Speed - 3000) <= 30", true},
		{"abs(Speed - 3000) <= 10", false},
		{"Vmv > 500", true},
		{"Vmv > 0.5V", true},
		{"Vmv < 0.7V and Vmv > 599mV", true},
		{"Vx > 1.0V", true},
		{"Vx * I == 0.6W", true},
		{"max(Vx, 1, 500mV) == Vx", true},
		{"min(Vx, 1) == 1V", true},
		// Enums order by declaration; strings and bools only compare.
		{`cls <= "II"`, true},
		{`cls < "II"`, false},
		{`"III" > cls`, true},
		{`mode == "active"`, true},
		{`mode != 'active'`, false},
		{"ok == True", true},
		{"ok", true},
		// Variables, both ways Robot writes them.
		{"$level >= 12V", true},
		{"${level} >= 12V", true},
		{"$n == 3", true},
	} {
		p := compile(t, c.src, Monitor)
		for i, n := range p.Inputs() {
			if err := p.Observe(i, 0, in[n]); err != nil {
				t.Fatal(err)
			}
		}
		got := p.Eval(0)
		if !got.Known || got.Bool != c.want {
			t.Errorf("%s = %+v, want %v", c.src, got, c.want)
		}
	}
}

func TestUnknown(t *testing.T) {
	for _, c := range []struct {
		src   string
		known bool
		want  bool
	}{
		{"Vx > 1V", false, false},
		{"Vx > 1V or True", true, true},
		{"True or Vx > 1V", true, true},
		{"Vx > 1V and False", true, false},
		{"Vx > 1V and True", false, false},
		{"not Vx > 1V", false, false},
		{"1V / (Vx - Vx) > 0", false, false},
	} {
		p := compile(t, c.src, Monitor)
		got := p.Eval(0)
		if got.Known != c.known || got.Known && got.Bool != c.want {
			t.Errorf("%s = %+v, want known=%v %v", c.src, got, c.known, c.want)
		}
	}
	// Division by zero, once Vx is known.
	p := compile(t, "1V / (Vx - Vx) > 0", Monitor)
	observe(t, p, "Vx", 0, Num(5))
	if got := p.Eval(0); got.Known {
		t.Errorf("division by zero = %+v, want unknown", got)
	}
}

// TestRefusals holds every refusal to its sentence: a refusal is read by the
// person who wrote the contract, and must say what to do.
func TestRefusals(t *testing.T) {
	for _, c := range []struct {
		src     string
		profile Profile
		want    string
	}{
		{"Vx > 1V && Vx < 5V", Monitor, "column 9: &&: write and"},
		{"Vx > 1V || True", Monitor, "||: write or"},
		{"!ok", Monitor, "!: write not"},
		{"ok = True", Monitor, "=: there is no assignment; to compare, write =="},
		{"ok == true", Monitor, "true: write True"},
		{"ok AND ok", Monitor, "the logic operators are written and, or, not"},
		{"Vx ** 2 > 1", Monitor, "there is no power operator"},
		{"Vx % 2 > 1", Monitor, "%: there is no remainder operator"},
		{"Vx > 1V # c", Monitor, `"#" is not part of an expression`},
		{`mode == "active`, Monitor, "the string that starts here is not closed"},
		{"${level >= 1V", Monitor, "${level is not closed with }"},
		{"$ > 1", Monitor, "a $ must be followed by a variable name"},
		{"Vy > 1V", Monitor, "column 1: Vy is not a known name"},
		{"Vx > 1mm", Monitor, `unknown unit "mm"`},
		{"Speed > 3000rpm", Monitor, "A number written bare takes the unit"},
		{"Vx > 30ms", Monitor, "column 4: >: V and ms are not the same kind of quantity"},
		{"Vx + I > 1V", Monitor, "+: V and A are not the same kind of quantity"},
		{"Vx * I > 1V", Monitor, "not the same kind of quantity"},
		{"T > 20dB", Monitor, "°C and dB are not the same kind"},
		{"0 < Vx < 5", Monitor, "comparisons do not chain; write a < x and x < b"},
		{"Vx", Monitor, "a monitor expression must be true or false, and this is a number in V"},
		{"Vx > 1V and 3", Monitor, "and needs true or false on each side, and this is a number"},
		{"ok + 1 > 2", Monitor, "+ needs a number on each side, and this is true or false"},
		{`mode < "b"`, Monitor, "< orders, and a string has no order; use == or !="},
		{`cls == "VI"`, Monitor, `"VI" is not a value of class`},
		{"cls == Vx", Monitor, "compares a value of class with a number in V"},
		{"(Vx > 1V", Monitor, "expected ) to close the ( at column 1"},
		{"Vx >", Monitor, "the expression ends where a value was expected"},
		{"Vx > 1V Vx", Monitor, `"Vx" was not expected here`},
		{"sqrt(Vx) > 1", Monitor, "sqrt() is not a function"},
		{"abs(Vx, I) > 1V", Monitor, "abs() takes 1 arguments, and has 2"},
		{"max(Vx) > 1V", Monitor, "max() takes 2 or more arguments, and has 1"},
		{"delta(Vx + 1V) > 1V", Monitor, "its first argument is a name"},
		{"age(Alive) * 2 < 30ms", Monitor, "age() must be compared directly with a constant duration"},
		{"age(Alive) < Vx", Monitor, "not the same kind of quantity"},
		{"rate(Alive, 100) > 5", Monitor, "rate()'s window needs a unit, as in 500ms"},
		{"rate(Alive, Vx) > 5", Monitor, "rate()'s window is a constant duration"},
		{"mean(Vx, 1V) > 5V", Monitor, "mean()'s window is a duration, and this is in V"},
		{"stable(Vx, 0s)", Monitor, "stable()'s window must be longer than zero"},
		{"mean(mode, 1s) > 5V", Monitor, "mean() takes numbers, and this is a string"},
		{"delta(Alive.counter, Vx) == 1", Monitor, "delta()'s modulus is a constant"},
		{"delta(Alive.counter, 0) == 1", Monitor, "delta()'s modulus must be more than zero"},
		// The condition profile: comparisons and logic, nothing computed.
		{"$level + 1V > 12V", Condition, "+: a procedure condition compares and combines, and does no arithmetic"},
		{"$level * 2 > 12V", Condition, "does no arithmetic"},
		{"-$level < 0V", Condition, "does no arithmetic"},
		{"abs($level) > 12V", Condition, "abs(): a procedure condition calls no functions"},
		{"$n", Condition, "a condition expression must be true or false"},
	} {
		_, err := Compile(c.src, c.profile, names)
		if err == nil {
			t.Errorf("%s: compiled, want %q", c.src, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.src, err, c.want)
		}
	}
	// What the condition profile does allow.
	// A bare number beside age() is in seconds, as beside any duration.
	if p := compile(t, "age(Alive) < 30", Monitor); p.Eval(0).Known {
		t.Error("age() known before any sample")
	}
	for _, src := range []string{"$level >= -12V", `cls == "I" or not ok`, "Vx < 16V"} {
		compile(t, src, Condition)
	}
}

// TestAgeDeadline: age() changes its answer at a known moment, to the
// nanosecond, and Deadline says when.
func TestAgeDeadline(t *testing.T) {
	for _, c := range []struct {
		src      string
		deadline int64
		before   bool // the answer just before the deadline, and its opposite at it
	}{
		{"age(Alive) < 30ms", 30 * ms, true},
		{"age(Alive) <= 30ms", 30*ms + 1, true},
		{"age(Alive) >= 30ms", 30 * ms, false},
		{"30ms > age(Alive)", 30 * ms, true},
	} {
		p := compile(t, c.src, Monitor)
		if _, ok := p.Deadline(0); ok {
			t.Errorf("%s: a deadline before any sample", c.src)
		}
		if p.Eval(0).Known {
			t.Errorf("%s: known before any sample", c.src)
		}
		observe(t, p, "Alive", 5*ms, Num(1))
		d, ok := p.Deadline(5 * ms)
		// Deadline offers both sides of the threshold; the first is the one
		// for < and >=, the second for <= and >.
		if !ok || d != 5*ms+30*ms {
			t.Errorf("%s: deadline %d, %v", c.src, d, ok)
		}
		at := 5*ms + c.deadline
		if got := p.Eval(at - 1); got.Bool != c.before {
			t.Errorf("%s at deadline-1 = %v", c.src, got.Bool)
		}
		if got := p.Eval(at); got.Bool == c.before {
			t.Errorf("%s at deadline = %v", c.src, got.Bool)
		}
	}
}

func TestDelta(t *testing.T) {
	p := compile(t, "delta(Alive.counter, 16) == 1", Monitor)
	q := compile(t, "delta(Alive.counter) == 1", Monitor)
	seq := []float64{13, 14, 15, 0, 1, 3}
	want := []Value{Unknown, BoolValue(true), BoolValue(true), BoolValue(true), BoolValue(true), BoolValue(false)}
	wantQ := []Value{Unknown, BoolValue(true), BoolValue(true), BoolValue(false), BoolValue(true), BoolValue(false)}
	for i, v := range seq {
		at := int64(i) * 10 * ms
		observe(t, p, "Alive.counter", at, Num(v))
		observe(t, q, "Alive.counter", at, Num(v))
		if got := p.Eval(at); got != want[i] {
			t.Errorf("modular, sample %d (%g): %+v, want %+v", i, v, got, want[i])
		}
		if got := q.Eval(at); got != wantQ[i] {
			t.Errorf("plain, sample %d (%g): %+v, want %+v", i, v, got, wantQ[i])
		}
	}
}

func TestStable(t *testing.T) {
	p := compile(t, "stable(Vx, 100ms)", Monitor)
	observe(t, p, "Vx", 0, Num(5))
	observe(t, p, "Vx", 50*ms, Num(5)) // the same value is not a change
	if p.Eval(99 * ms).Bool {
		t.Error("stable before 100ms")
	}
	if d, _ := p.Deadline(60 * ms); d != 100*ms {
		t.Errorf("deadline %d, want 100ms", d)
	}
	if !p.Eval(100 * ms).Bool {
		t.Error("not stable at 100ms")
	}
	observe(t, p, "Vx", 120*ms, Num(6))
	if p.Eval(120 * ms).Bool {
		t.Error("stable just after a change")
	}
	if !p.Eval(220 * ms).Bool {
		t.Error("not stable 100ms after the change")
	}
}

func TestWindows(t *testing.T) {
	r := compile(t, "rate(Alive, 100ms) >= 10", Monitor)
	m := compile(t, "mean(Vx, 30ms) > 2V", Monitor)
	for i := int64(0); i < 10; i++ {
		observe(t, r, "Alive", i*10*ms, Num(1))
	}
	// Ten samples, at 0..90ms. At 90ms all ten are inside (-10ms, 90ms].
	if got := r.Eval(90 * ms); !got.Bool {
		t.Errorf("rate at 90ms = %+v", got)
	}
	// The one at 0 leaves at 100ms, and Deadline says so.
	if d, _ := r.Deadline(90 * ms); d != 100*ms {
		t.Errorf("rate deadline %d, want 100ms", d)
	}
	if got := r.Eval(100 * ms); got.Bool {
		t.Errorf("rate at 100ms = %+v", got)
	}

	for i, v := range []float64{1, 2, 3, 4} { // at 0, 10, 20, 30ms
		observe(t, m, "Vx", int64(i)*10*ms, Num(v))
	}
	// At 30ms the window (0, 30ms] holds 2, 3, 4: mean 3.
	if got := m.Eval(30 * ms); !got.Bool {
		t.Errorf("mean at 30ms = %+v", got)
	}
	if got := compile(t, "mean(Vx, 30ms) > 2V", Monitor).Eval(0); got.Known {
		t.Errorf("mean of an empty window = %+v, want unknown", got)
	}
}

// TestWindowOverflow: a window that lost samples answers unknown until they
// have left it, and is whole again afterwards.
func TestWindowOverflow(t *testing.T) {
	defer func(n int) { MaxWindowSamples = n }(MaxWindowSamples)
	MaxWindowSamples = 16
	p := compile(t, "rate(Alive, 100ms) > 0", Monitor)
	for i := int64(0); i < 20; i++ { // 20 samples within 19ms
		observe(t, p, "Alive", i*ms, Num(1))
	}
	if got := p.Eval(19 * ms); got.Known {
		t.Errorf("an overflowed window = %+v, want unknown", got)
	}
	// The newest lost sample was at 3ms, so the window is whole from 103ms,
	// before the oldest held one (4ms) leaves at 104ms.
	if d, _ := p.Deadline(19 * ms); d != 103*ms {
		t.Errorf("deadline %d, want 103ms", d)
	}
	if got := p.Eval(102 * ms); got.Known {
		t.Errorf("at 102ms = %+v, want unknown", got)
	}
	if got := p.Eval(103 * ms); !got.Known || !got.Bool {
		t.Errorf("at 103ms = %+v, want true", got)
	}
}

func TestObserveRefusals(t *testing.T) {
	p := compile(t, `cls == "I" and Vx > 1V`, Monitor)
	observe(t, p, "Vx", 10*ms, Num(2))
	i, _ := p.Slot("Vx")
	if err := p.Observe(i, 5*ms, Num(2)); err == nil || !strings.Contains(err.Error(), "before the one at") {
		t.Errorf("an earlier sample: %v", err)
	}
	j, _ := p.Slot("cls")
	if err := p.Observe(j, 10*ms, StrValue("VI")); err == nil || !strings.Contains(err.Error(), `"VI" is not a value of class`) {
		t.Errorf("an enum value it lacks: %v", err)
	}
}

// TestPurity is the property the monitor's replay rests on: the same input
// history gives the same answers, whoever evaluates it and however often.
func TestPurity(t *testing.T) {
	srcs := []string{
		"age(Alive) < 30ms and delta(Alive.counter, 16) == 1",
		"abs(Speed - 3000) <= 30 and stable(Speed, 50ms)",
		"rate(Alive, 100ms) >= 8 or mean(Vx, 40ms) > 2V",
	}
	for _, src := range srcs {
		for seed := uint64(0); seed < 20; seed++ {
			run := func() []Value {
				rng := rand.New(rand.NewPCG(seed, 1))
				p := compile(t, src, Monitor)
				var out []Value
				now := int64(0)
				for k := 0; k < 500; k++ {
					now += int64(rng.IntN(int(15 * ms)))
					i := rng.IntN(len(p.Inputs()))
					v := Num(float64(rng.IntN(4000)) / 1000)
					switch p.Inputs()[i] {
					case "Speed":
						v = Num(2950 + float64(rng.IntN(100)))
					case "Alive.counter":
						v = Num(float64(k % 16))
					}
					if rng.IntN(50) == 0 {
						v = Unknown
					}
					if err := p.Observe(i, now, v); err != nil {
						t.Fatal(err)
					}
					// Evaluating twice, and at a deadline, changes nothing.
					a, b := p.Eval(now), p.Eval(now)
					if a != b {
						t.Fatalf("%s: two evaluations differ: %+v, %+v", src, a, b)
					}
					out = append(out, a)
					if d, ok := p.Deadline(now); ok {
						out = append(out, p.Eval(d))
					}
				}
				return out
			}
			a, b := run(), run()
			if len(a) != len(b) {
				t.Fatalf("%s seed %d: %d answers, then %d", src, seed, len(a), len(b))
			}
			for i := range a {
				if a[i] != b[i] {
					t.Fatalf("%s seed %d: answer %d differs: %+v, %+v", src, seed, i, a[i], b[i])
				}
			}
		}
	}
}

func TestParseQuantity(t *testing.T) {
	for _, c := range []struct {
		in   string
		want float64
	}{
		{"20ms", 0.02}, {"1.5V", 1.5}, {"-3dB", -3}, {"10s", 10}, {"500mV", 0.5}, {"2", 2},
	} {
		v, _, err := ParseQuantity(c.in)
		if err != nil || v != c.want {
			t.Errorf("%s = %g, %v; want %g", c.in, v, err, c.want)
		}
	}
	for _, in := range []string{"20 parsecs", "fast", "20xs", "1V 2V"} {
		if _, _, err := ParseQuantity(in); err == nil {
			t.Errorf("%s: no error", in)
		}
	}
}

func TestParseUnit(t *testing.T) {
	if u := ParseUnit("rpm"); u.Scale != 1 || u.SameDimension(ParseUnit("V")) || !u.SameDimension(ParseUnit("rpm")) {
		t.Errorf("rpm = %+v", u)
	}
	if u := ParseUnit("mV"); u.Scale != 1e-3 || !u.SameDimension(ParseUnit("kV")) {
		t.Errorf("mV = %+v", u)
	}
	if !ParseUnit("W").SameDimension(product(ParseUnit("V"), ParseUnit("A"), 1)) {
		t.Error("W is not V·A")
	}
	if !ParseUnit("").Dimensionless() || !ParseUnit("%").Dimensionless() {
		t.Error("no unit, or %, has a dimension")
	}
}

// Benchmarks: the handoff asks for one evaluation of a typical monitor in
// under 1 µs.
func BenchmarkEval(b *testing.B) {
	for _, src := range []string{
		"Vx > 1.0V",
		"abs(Speed - 3000) <= 30",
		"age(Alive) < 30ms and delta(Alive.counter, 16) == 1",
		"rate(Alive, 100ms) >= 9 and mean(Vx, 50ms) > 1V",
	} {
		b.Run(src, func(b *testing.B) {
			p, err := Compile(src, Monitor, names)
			if err != nil {
				b.Fatal(err)
			}
			// A window's worth of history, at a 10 ms period.
			for k := int64(0); k < 20; k++ {
				for i := range p.Inputs() {
					if err := p.Observe(i, k*10*ms, Num(float64(k%16))); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.ResetTimer()
			for b.Loop() {
				p.Eval(195 * ms)
			}
		})
	}
}

func BenchmarkObserveEval(b *testing.B) {
	p, err := Compile("age(Alive) < 30ms and delta(Alive.counter, 16) == 1", Monitor, names)
	if err != nil {
		b.Fatal(err)
	}
	a, _ := p.Slot("Alive")
	c, _ := p.Slot("Alive.counter")
	t := int64(0)
	for b.Loop() {
		t += 10 * ms
		_ = p.Observe(a, t, Num(1))
		_ = p.Observe(c, t, Num(float64(t/(10*ms)%16)))
		p.Eval(t)
	}
}
