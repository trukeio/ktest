package monitor

import (
	"fmt"
	"math"
	"slices"
	"sort"

	"github.com/rveen/ktest/expr"
)

// State is a monitor's state.
type State string

const (
	OK       State = "ok"
	Dev      State = "dev"
	Unknown  State = "unknown"
	Inactive State = "inactive"
	Settling State = "settling"
)

// States is every state, in the order the recording numbers them.
var States = []State{OK, Dev, Unknown, Inactive, Settling}

// Transition is a monitor changing state.
type Transition struct {
	T       int64 // the sample's time, or the deadline's, in the evaluator's nanoseconds
	Monitor string
	From    State
	To      State
	Cause   string // what was evaluated: a stream's sample, or the deadline that ran out
}

// periods is how many of an input's latest intervals its observed period is
// the median of: enough that one late sample does not move it, few enough
// that a changed rate is followed within a second at 10 ms.
const periods = 8

// Evaluator runs a contract's monitors over samples in time order. It is not
// safe for concurrent use; the daemon gives it a goroutine of its own.
type Evaluator struct {
	c    *Contract
	now  int64
	init bool
	mons []*mon
	byNm map[string]*mon

	// feeds is, by stream name, every program input a record of that stream
	// gives a sample to.
	feeds  map[string][]feed
	inputs map[string]*inputState

	mode   string
	modeT  int64
	haveMd bool
}

type feed struct {
	m    *mon
	slot int
	name string
	ref  nameRef
}

type mon struct {
	m     *Monitor
	prog  *expr.Program
	state State
	// The raw answer while the monitor can be judged, and since when: the
	// debounce runs from it.
	judging  bool
	raw      bool
	rawSince int64
	// deps are the dependents' slots this monitor's state feeds.
	deps []feed
}

// inputState is an input's timing: when its stream last had a record, and
// the intervals before it.
type inputState struct {
	in        *Input
	n         int
	last      int64
	intervals []int64
	forgotten bool // gone stale, and its values made unknown
	// values are the program inputs that read the input's values, which go
	// unknown when it is stale; age() and rate() read only its timing.
	values []feed
}

// NewEvaluator makes an evaluator for c. Every monitor starts unknown.
func NewEvaluator(c *Contract) (*Evaluator, error) {
	e := &Evaluator{c: c, byNm: map[string]*mon{}, feeds: map[string][]feed{}, inputs: map[string]*inputState{}}
	for _, name := range c.Order {
		m := c.Monitors[name]
		p, err := c.compile(m)
		if err != nil {
			return nil, err
		}
		mn := &mon{m: m, prog: p, state: Unknown}
		e.mons = append(e.mons, mn)
		e.byNm[name] = mn
	}
	for _, mn := range e.mons {
		for i, n := range mn.prog.Inputs() {
			r := c.names[n]
			f := feed{m: mn, slot: i, name: n, ref: r}
			if r.monitor != "" {
				e.byNm[r.monitor].deps = append(e.byNm[r.monitor].deps, f)
				continue
			}
			s := r.ref.Stream()
			e.feeds[s] = append(e.feeds[s], f)
		}
	}
	for alias, in := range c.Inputs {
		e.inputs[alias] = &inputState{in: in}
	}
	for _, fs := range e.feeds {
		for _, f := range fs {
			if f.ref.ref.Field >= 0 {
				is := e.inputs[f.ref.alias]
				is.values = append(is.values, f)
			}
		}
	}
	return e, nil
}

// Streams are the streams whose records the evaluator reads.
func (e *Evaluator) Streams() []string {
	var out []string
	for s := range e.feeds {
		out = append(out, s)
	}
	for _, in := range e.inputs {
		if !slices.Contains(out, in.in.Ref.Stream()) {
			out = append(out, in.in.Ref.Stream())
		}
	}
	sort.Strings(out)
	return out
}

// Now is the time up to which the evaluator has emitted every transition:
// the watermark.
func (e *Evaluator) Now() int64 { return e.now }

// State is a monitor's current state.
func (e *Evaluator) State(name string) State {
	if m := e.byNm[name]; m != nil {
		return m.state
	}
	return Unknown
}

// Record gives the evaluator one record of a stream, taken at t. Records
// arrive in time order across every stream; one before the evaluator's time
// is refused, since the transitions up to then have been emitted.
func (e *Evaluator) Record(stream string, t int64, rec []byte) ([]Transition, error) {
	if err := e.at(t); err != nil {
		return nil, err
	}
	out := e.advance(t, false)
	for _, is := range e.inputs {
		if is.in.Ref.Stream() != stream {
			continue
		}
		if is.n > 0 {
			is.intervals = append(is.intervals, t-is.last)
			if len(is.intervals) > periods {
				is.intervals = is.intervals[1:]
			}
		}
		is.last = t
		is.n++
		is.forgotten = false
	}
	for _, f := range e.feeds[stream] {
		v := f.ref.ref.Decode(rec)
		if err := f.m.prog.Observe(f.slot, t, v); err != nil {
			return out, fmt.Errorf("%s: %v", f.name, err)
		}
	}
	e.now = t
	return append(out, e.evaluate(t, stream)...), nil
}

// Mode tells the evaluator the DUT entered a mode at t; "" for none.
func (e *Evaluator) Mode(t int64, mode string) ([]Transition, error) {
	if mode != "" && e.c.Modes[mode] == nil {
		return nil, fmt.Errorf("mode %q is not one the contract declares", mode)
	}
	if err := e.at(t); err != nil {
		return nil, err
	}
	out := e.advance(t, false)
	e.mode, e.modeT, e.haveMd = mode, t, true
	e.now = t
	return append(out, e.evaluate(t, "mode "+mode)...), nil
}

// Advance tells the evaluator that time has reached t with no record: every
// deadline up to t is evaluated. A record or a mode at t may still follow.
func (e *Evaluator) Advance(t int64) ([]Transition, error) {
	if err := e.at(t); err != nil {
		return nil, err
	}
	out := e.advance(t, true)
	e.now = t
	return out, nil
}

func (e *Evaluator) at(t int64) error {
	if e.init && t < e.now {
		return fmt.Errorf("a sample at %d ns, after the evaluator has reached %d ns", t, e.now)
	}
	if !e.init {
		e.init, e.now = true, t
	}
	return nil
}

// advance evaluates every deadline before t, or up to and including t.
func (e *Evaluator) advance(t int64, incl bool) []Transition {
	var out []Transition
	for {
		d, cause, ok := e.deadline(e.now)
		if !ok || d > t || d == t && !incl {
			return out
		}
		e.now = d
		out = append(out, e.evaluate(d, cause)...)
	}
}

// deadline is the earliest time after now at which a state may change with
// no new sample, and why.
func (e *Evaluator) deadline(now int64) (int64, string, bool) {
	best, cause := int64(math.MaxInt64), ""
	take := func(d int64, why string) {
		if d > now && d < best {
			best, cause = d, why
		}
	}
	for _, mn := range e.mons {
		if d, ok := mn.prog.Deadline(now); ok {
			take(d, mn.m.Name+": a time function's deadline")
		}
		if mn.judging && mn.m.Debounce > 0 {
			take(mn.rawSince+int64(mn.m.Debounce), mn.m.Name+": debounce")
		}
	}
	if e.haveMd && e.mode != "" {
		take(e.modeT+int64(e.c.Modes[e.mode].Settle), "mode "+e.mode+" settled")
	}
	for _, alias := range sortedInputs(e.inputs) {
		if d, ok := e.inputs[alias].staleAt(); ok {
			take(d, "input "+alias+" stale")
		}
	}
	return best, cause, best != math.MaxInt64
}

func sortedInputs(m map[string]*inputState) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// maxAge is how old the input's latest sample may be; ok is false while
// there is no telling: a sampled input seen fewer than twice, with no
// max_age given.
func (is *inputState) maxAge() (int64, bool) {
	if is.in.MaxAge > 0 {
		return int64(is.in.MaxAge), true
	}
	if len(is.intervals) == 0 {
		return 0, false
	}
	iv := slices.Clone(is.intervals)
	slices.Sort(iv)
	return 3 * iv[len(iv)/2], true
}

// staleAt is when the input goes stale if no record comes: the first
// nanosecond its latest sample is older than its maximum age.
func (is *inputState) staleAt() (int64, bool) {
	if is.in.Ref.Held || is.n == 0 {
		return 0, false
	}
	a, ok := is.maxAge()
	if !ok {
		return 0, false
	}
	return is.last + a + 1, true
}

// stale says whether the input has no usable sample at t: none yet, or one
// older than its maximum age. A held input is never stale once seen.
func (is *inputState) stale(t int64) bool {
	if is.n == 0 {
		return true
	}
	if is.in.Ref.Held {
		return false
	}
	a, ok := is.maxAge()
	if !ok {
		return true // seen once: its period is not yet known
	}
	return t-is.last > a
}

// evaluate takes every monitor, in dependency order, to its state at t.
//
// An input that has gone stale first has its values made unknown: not the
// monitors reading it, which answer by Kleene's logic as ever. Staleness is
// a fact about the input's values, and a monitor that watches for the input
// stopping — age(Alive) < 30ms and delta(Alive.counter) == 1 — must go on
// saying dev while it has stopped, rather than unknown.
func (e *Evaluator) evaluate(t int64, cause string) []Transition {
	for _, alias := range sortedInputs(e.inputs) {
		is := e.inputs[alias]
		if is.forgotten || is.n == 0 || !is.stale(t) {
			continue
		}
		is.forgotten = true
		for _, f := range is.values {
			f.m.prog.Forget(f.slot)
		}
	}
	var out []Transition
	for _, mn := range e.mons {
		s := e.judge(mn, t)
		if s == mn.state {
			continue
		}
		out = append(out, Transition{T: t, Monitor: mn.m.Name, From: mn.state, To: s, Cause: cause})
		mn.state = s
		// A monitor read by another is true while ok, false while dev,
		// and unknown otherwise: an inactive or settling monitor vouches
		// for nothing.
		v := expr.Unknown
		switch s {
		case OK:
			v = expr.BoolValue(true)
		case Dev:
			v = expr.BoolValue(false)
		}
		for _, d := range mn.deps {
			_ = d.m.prog.Observe(d.slot, t, v)
		}
	}
	return out
}

// judge is a monitor's state at t.
func (e *Evaluator) judge(mn *mon, t int64) State {
	m := mn.m
	if len(m.Modes) > 0 {
		if !slices.Contains(m.Modes, e.mode) {
			mn.judging = false
			return Inactive
		}
		if t < e.modeT+int64(e.c.Modes[e.mode].Settle) {
			mn.judging = false
			return Settling
		}
	}
	v := mn.prog.Eval(t)
	if !v.Known {
		mn.judging = false
		return Unknown
	}
	if !mn.judging || mn.raw != v.Bool {
		mn.judging, mn.raw, mn.rawSince = true, v.Bool, t
	}
	want := Dev
	if v.Bool {
		want = OK
	}
	if t-mn.rawSince >= int64(m.Debounce) {
		return want
	}
	// Still within the debounce: a judged monitor keeps its judgement, and
	// one not yet judged stays unknown until the debounce has run.
	if mn.state == OK || mn.state == Dev {
		return mn.state
	}
	return Unknown
}
