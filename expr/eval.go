package expr

import "math"

// node is a compiled expression. eval reads state and never changes it.
type node interface {
	eval(now int64) Value
}

type constNode struct{ v Value }

func (n constNode) eval(int64) Value { return n.v }

type refNode struct{ s *slot }

func (n *refNode) eval(int64) Value { return n.s.cur }

type scaleNode struct {
	x node
	k float64
}

func (n *scaleNode) eval(now int64) Value {
	v := n.x.eval(now)
	v.Num *= n.k
	return v
}

type notNode struct{ x node }

func (n *notNode) eval(now int64) Value {
	v := n.x.eval(now)
	v.Bool = !v.Bool
	return v
}

type andNode struct{ l, r node }

func (n *andNode) eval(now int64) Value {
	l := n.l.eval(now)
	if l.Known && !l.Bool {
		return l
	}
	r := n.r.eval(now)
	if r.Known && !r.Bool {
		return r
	}
	if l.Known && r.Known {
		return BoolValue(true)
	}
	return Unknown
}

type orNode struct{ l, r node }

func (n *orNode) eval(now int64) Value {
	l := n.l.eval(now)
	if l.Known && l.Bool {
		return l
	}
	r := n.r.eval(now)
	if r.Known && r.Bool {
		return r
	}
	if l.Known && r.Known {
		return BoolValue(false)
	}
	return Unknown
}

type cmpNode struct {
	op   string
	kind Kind
	l, r node
}

func (n *cmpNode) eval(now int64) Value {
	l, r := n.l.eval(now), n.r.eval(now)
	if !l.Known || !r.Known {
		return Unknown
	}
	switch n.kind {
	case Bool:
		return BoolValue(l.Bool == r.Bool == (n.op == "=="))
	case String:
		return BoolValue(l.Str == r.Str == (n.op == "=="))
	}
	return BoolValue(order(n.op, l.Num, r.Num))
}

func order[T int64 | float64](op string, a, b T) bool {
	switch op {
	case "==":
		return a == b
	case "!=":
		return a != b
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	}
	return a >= b
}

type arithNode struct {
	op   byte
	l, r node
}

func (n *arithNode) eval(now int64) Value {
	l, r := n.l.eval(now), n.r.eval(now)
	if !l.Known || !r.Known {
		return Unknown
	}
	switch n.op {
	case '+':
		return Num(l.Num + r.Num)
	case '-':
		return Num(l.Num - r.Num)
	case '*':
		return Num(l.Num * r.Num)
	}
	// Infinity is not a measurement. A division by zero is a value nobody
	// knows, and a monitor reading it is unknown rather than deviating.
	if r.Num == 0 {
		return Unknown
	}
	return Num(l.Num / r.Num)
}

type negNode struct{ x node }

func (n *negNode) eval(now int64) Value {
	v := n.x.eval(now)
	v.Num = -v.Num
	return v
}

type absNode struct{ x node }

func (n *absNode) eval(now int64) Value {
	v := n.x.eval(now)
	v.Num = math.Abs(v.Num)
	return v
}

type minMaxNode struct {
	max bool
	xs  []node
}

func (n *minMaxNode) eval(now int64) Value {
	var out Value
	for i, x := range n.xs {
		v := x.eval(now)
		if !v.Known {
			return Unknown
		}
		if i == 0 || n.max && v.Num > out.Num || !n.max && v.Num < out.Num {
			out = v
		}
	}
	return out
}

// ageNode is age() as a number. Compile refuses it anywhere but beside a
// comparison with a constant, which becomes an ageCmp; it is evaluated only
// while a constant is folded, which it never is.
type ageNode struct{ s *slot }

func (n *ageNode) eval(now int64) Value {
	if n.s.n == 0 {
		return Unknown
	}
	return Num(float64(now-n.s.lastT) / 1e9)
}

// ageCmp is age(x) op c, in whole nanoseconds, with age on the left.
type ageCmp struct {
	s  *slot
	op string
	c  int64
}

func (n *ageCmp) eval(now int64) Value {
	if n.s.n == 0 {
		return Unknown
	}
	return BoolValue(order(n.op, now-n.s.lastT, n.c))
}

// deltaNode is the difference between the last two samples, wrapped into
// [0, mod) when a modulus is given, so that a rolling counter going from 15
// to 0 has moved by one.
type deltaNode struct {
	s   *slot
	mod float64
}

func (n *deltaNode) eval(int64) Value {
	if n.s.n < 2 || !n.s.cur.Known || !n.s.prev.Known {
		return Unknown
	}
	d := n.s.cur.Num - n.s.prev.Num
	if n.mod > 0 {
		d = math.Mod(d, n.mod)
		if d < 0 {
			d += n.mod
		}
	}
	return Num(d)
}

// stableNode is true once the input has held its value for w. Before the
// first sample nobody knows; after it, a value seen for less than w has not
// been seen to be stable.
type stableNode struct {
	s *slot
	w int64
}

func (n *stableNode) eval(now int64) Value {
	if n.s.n == 0 {
		return Unknown
	}
	return BoolValue(now-n.s.changeT >= n.w)
}

// window is the samples of one input in the last w, for one call site. A
// sample at t is in the window at now when now-w < t <= now: it enters when
// it is taken and leaves w later.
type window struct {
	w    int64
	ring []sample
	head int // index of the oldest
	size int

	// lostUntil is when the newest sample pushed out of a full ring was
	// taken. While that is inside the window, the window is not whole.
	lostUntil int64
	lost      bool
}

type sample struct {
	t int64
	v Value
}

func (w *window) push(t int64, v Value) {
	// Samples that can no longer be in any window this site will be asked
	// about are let go first. Eval never asks about a time before the newest
	// sample less w, since samples arrive in order and are evaluated after.
	for w.size > 0 && w.ring[w.head].t <= t-w.w {
		w.head = (w.head + 1) % len(w.ring)
		w.size--
	}
	if w.size == len(w.ring) {
		if len(w.ring) < MaxWindowSamples {
			w.grow()
		} else {
			w.lostUntil, w.lost = w.ring[w.head].t, true
			w.head = (w.head + 1) % len(w.ring)
			w.size--
		}
	}
	w.ring[(w.head+w.size)%len(w.ring)] = sample{t, v}
	w.size++
}

func (w *window) grow() {
	n := min(max(2*len(w.ring), 16), MaxWindowSamples)
	ring := make([]sample, n)
	for i := 0; i < w.size; i++ {
		ring[i] = w.ring[(w.head+i)%len(w.ring)]
	}
	w.ring, w.head = ring, 0
}

// whole reports whether every sample in the window at now is still held.
func (w *window) whole(now int64) bool { return !w.lost || w.lostUntil <= now-w.w }

// each calls f for every sample in the window at now, oldest first.
func (w *window) each(now int64, f func(sample)) {
	for i := 0; i < w.size; i++ {
		s := w.ring[(w.head+i)%len(w.ring)]
		if s.t > now {
			return
		}
		if s.t > now-w.w {
			f(s)
		}
	}
}

func (w *window) oldest(now int64) (int64, bool) {
	t, ok := int64(0), false
	w.each(now, func(s sample) {
		if !ok {
			t, ok = s.t, true
		}
	})
	return t, ok
}

// rateNode counts the samples in the window: events per window.
type rateNode struct{ w *window }

func (n *rateNode) eval(now int64) Value {
	if !n.w.whole(now) {
		return Unknown
	}
	c := 0
	n.w.each(now, func(sample) { c++ })
	return Num(float64(c))
}

// meanNode is the mean of the samples in the window. Of the samples, not over
// time: it changes only when a sample enters or leaves, so the moment it
// changes is known, which a time-weighted mean of a held value would not be.
type meanNode struct{ w *window }

func (n *meanNode) eval(now int64) Value {
	if !n.w.whole(now) {
		return Unknown
	}
	sum, c, known := 0.0, 0, true
	n.w.each(now, func(s sample) {
		if !s.v.Known {
			known = false
		}
		sum += s.v.Num
		c++
	})
	if c == 0 || !known {
		return Unknown
	}
	return Num(sum / float64(c))
}
