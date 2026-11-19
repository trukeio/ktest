package expr

import (
	"fmt"
	"math"
)

// Program is a compiled expression and the state its time functions keep. It
// is not safe for concurrent use: a monitor's evaluator owns its programs, as
// the daemon's loop owns its recording.
type Program struct {
	src     string
	profile Profile
	root    node
	slots   []*slot
	byName  map[string]int

	// What can change the answer without a new sample: an age compared with a
	// constant, a stable, a window. Deadline consults them.
	ages    []*ageCmp
	stables []*stableNode
	windows []*window
}

// Compile compiles src for profile, resolving every name through r. Every
// expression is a predicate, whatever its profile: a monitor is ok or not, a
// limit holds or not, a condition is true or false.
func Compile(src string, profile Profile, r Resolver) (*Program, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &Program{src: src, profile: profile, byName: map[string]int{}}
	c := &compiler{p: p, r: r, toks: toks}
	root, err := c.expr()
	if err != nil {
		return nil, err
	}
	if t := c.peek(); t.kind != tEOF {
		if t.kind == tOp && isCmp(t.text) {
			return nil, errAt(t.col, "%s: comparisons do not chain; write a < x and x < b", t.text)
		}
		return nil, errAt(t.col, "%q was not expected here", t.text)
	}
	if root.t.Kind != Bool {
		return nil, errAt(1, "a %s expression must be true or false, and this is %s", profile, root.t)
	}
	for _, a := range c.agesSeen {
		if !a.compared {
			return nil, errAt(a.col, "age() must be compared directly with a constant duration, as in age(x) < 30ms, "+
				"so that the moment its answer changes is known")
		}
	}
	p.root = root.n
	return p, nil
}

// Source is the expression as written.
func (p *Program) Source() string { return p.src }

// Inputs are the names the program reads, by slot: Observe(i, ...) feeds
// Inputs()[i].
func (p *Program) Inputs() []string {
	out := make([]string, len(p.slots))
	for i, s := range p.slots {
		out[i] = s.name
	}
	return out
}

// Slot is the slot of an input, by name.
func (p *Program) Slot(name string) (int, bool) {
	i, ok := p.byName[name]
	return i, ok
}

// Observe gives the program a sample of input i taken at t, in nanoseconds.
// A number is in the unit the input's type states. Samples of one input
// arrive in time order; one that does not is refused rather than reordered,
// since the history would then depend on when it was noticed.
func (p *Program) Observe(i int, t int64, v Value) error {
	s := p.slots[i]
	if s.n > 0 && t < s.lastT {
		return fmt.Errorf("%s: a sample at %d ns, before the one at %d ns", s.name, t, s.lastT)
	}
	if v.Known {
		switch s.typ.Kind {
		case Number:
			v.Num *= s.typ.Unit.Scale
			if math.IsNaN(v.Num) {
				v = Unknown
			}
		case Enum:
			n, ok := s.typ.Enum.index(v.Str)
			if !ok {
				return fmt.Errorf("%s: %q is not a value of %s", s.name, v.Str, s.typ.Enum.Name)
			}
			v.Num = float64(n)
		}
	}
	changed := s.n == 0 || !same(s.cur, v, s.typ.Kind)
	s.prev, s.cur, s.lastT = s.cur, v, t
	s.n++
	if changed {
		s.changeT = t
	}
	for _, w := range s.windows {
		w.push(t, v)
	}
	return nil
}

// Eval is the program's answer at now, in nanoseconds. It changes nothing, so
// asking twice gives the same answer twice.
func (p *Program) Eval(now int64) Value { return p.root.eval(now) }

// Deadline is the earliest time after now at which the answer may change with
// no new sample: an age crossing its threshold, a value becoming stable, a
// sample leaving a window. ok is false when there is none. An answer that
// does not in fact change there costs one evaluation and nothing else.
func (p *Program) Deadline(now int64) (t int64, ok bool) {
	t = math.MaxInt64
	take := func(c int64) {
		if c > now && c < t {
			t, ok = c, true
		}
	}
	for _, a := range p.ages {
		if a.s.n > 0 {
			take(a.s.lastT + a.c)
			take(a.s.lastT + a.c + 1)
		}
	}
	for _, st := range p.stables {
		if st.s.n > 0 {
			take(st.s.changeT + st.w)
		}
	}
	for _, w := range p.windows {
		if o, has := w.oldest(now); has {
			take(o + w.w)
		}
		if w.lostUntil > now-w.w {
			take(w.lostUntil + w.w)
		}
	}
	if !ok {
		return 0, false
	}
	return t, true
}

func same(a, b Value, k Kind) bool {
	if a.Known != b.Known {
		return false
	}
	if !a.Known {
		return true
	}
	switch k {
	case Bool:
		return a.Bool == b.Bool
	case String:
		return a.Str == b.Str
	}
	return a.Num == b.Num
}

// slot is one input and the history the time functions read from it.
type slot struct {
	name      string
	typ       Type
	n         int // samples seen
	cur, prev Value
	lastT     int64 // when cur was taken
	changeT   int64 // when the value last became what it is
	windows   []*window
}

// cnode is a node while it is compiled: its type, and two facts the
// compiler needs about it.
type cnode struct {
	n   node
	t   Type
	col int

	// bare is a number written without a unit, or made only of such numbers,
	// waiting to take the unit of what it meets.
	bare bool
	// konst is made of literals only, and is folded to one.
	konst bool
	// ref is the slot of a plain reference, -1 otherwise: the time functions
	// read a channel's history, not a value computed from it.
	ref int
	// age is set on a bare age() call, until a comparison claims it.
	age *ageSite
}

type ageSite struct {
	col      int
	s        *slot
	compared bool
}

type compiler struct {
	p        *Program
	r        Resolver
	toks     []token
	i        int
	agesSeen []*ageSite
}

func (c *compiler) peek() token { return c.toks[c.i] }
func (c *compiler) next() token { t := c.toks[c.i]; c.i++; return t }

func isCmp(op string) bool {
	switch op {
	case "==", "!=", "<", "<=", ">", ">=":
		return true
	}
	return false
}

func (c *compiler) expr() (cnode, error) { return c.or() }

func (c *compiler) or() (cnode, error) {
	l, err := c.and()
	if err != nil {
		return l, err
	}
	for c.peek().kind == tOr {
		op := c.next()
		r, err := c.and()
		if err != nil {
			return r, err
		}
		if err := wantBool(op, l, r); err != nil {
			return l, err
		}
		l = c.fold(cnode{n: &orNode{l.n, r.n}, t: Type{Kind: Bool}, col: l.col, konst: l.konst && r.konst, ref: -1})
	}
	return l, nil
}

func (c *compiler) and() (cnode, error) {
	l, err := c.not()
	if err != nil {
		return l, err
	}
	for c.peek().kind == tAnd {
		op := c.next()
		r, err := c.not()
		if err != nil {
			return r, err
		}
		if err := wantBool(op, l, r); err != nil {
			return l, err
		}
		l = c.fold(cnode{n: &andNode{l.n, r.n}, t: Type{Kind: Bool}, col: l.col, konst: l.konst && r.konst, ref: -1})
	}
	return l, nil
}

func wantBool(op token, ns ...cnode) error {
	for _, n := range ns {
		if n.t.Kind != Bool {
			return errAt(n.col, "%s needs true or false on each side, and this is %s", op.text, n.t)
		}
	}
	return nil
}

func (c *compiler) not() (cnode, error) {
	if c.peek().kind != tNot {
		return c.cmp()
	}
	op := c.next()
	x, err := c.cmp()
	if err != nil {
		return x, err
	}
	if err := wantBool(op, x); err != nil {
		return x, err
	}
	return c.fold(cnode{n: &notNode{x.n}, t: Type{Kind: Bool}, col: op.col, konst: x.konst, ref: -1}), nil
}

func (c *compiler) cmp() (cnode, error) {
	l, err := c.sum()
	if err != nil {
		return l, err
	}
	t := c.peek()
	if t.kind != tOp || !isCmp(t.text) {
		return l, nil
	}
	c.next()
	r, err := c.sum()
	if err != nil {
		return r, err
	}
	if t2 := c.peek(); t2.kind == tOp && isCmp(t2.text) {
		return l, errAt(t2.col, "%s: comparisons do not chain; write a < x and x < b", t2.text)
	}
	return c.compare(t, l, r)
}

func (c *compiler) compare(op token, l, r cnode) (cnode, error) {
	out := cnode{t: Type{Kind: Bool}, col: l.col, konst: l.konst && r.konst, ref: -1}
	ordered := op.text != "==" && op.text != "!="

	// An enumeration against a string literal: the literal must be one of its
	// values, and becomes its position.
	if l.t.Kind == Enum && r.t.Kind == String || l.t.Kind == String && r.t.Kind == Enum {
		e, s := l, r
		if l.t.Kind == String {
			e, s = r, l
		}
		if !s.konst {
			return out, errAt(s.col, "%s compares with one of its values, written as a string", e.t)
		}
		v := s.n.eval(0)
		n, ok := e.t.Enum.index(v.Str)
		if !ok {
			return out, errAt(s.col, "%q is not a value of %s, which has %q", v.Str, e.t.Enum.Name, e.t.Enum.Values)
		}
		s = cnode{n: constNode{Value{Known: true, Num: float64(n)}}, t: e.t, col: s.col, konst: true, ref: -1}
		if l.t.Kind == String {
			l = s
		} else {
			r = s
		}
	}

	if l.t.Kind != r.t.Kind {
		return out, errAt(op.col, "%s compares %s with %s", op.text, l.t, r.t)
	}
	switch l.t.Kind {
	case Bool, String:
		if ordered {
			return out, errAt(op.col, "%s orders, and %s has no order; use == or !=", op.text, l.t)
		}
	case Enum:
		if l.t.Enum != r.t.Enum {
			return out, errAt(op.col, "%s compares %s with %s", op.text, l.t, r.t)
		}
	case Number:
		var err error
		if l, r, err = unify(op, l, r); err != nil {
			return out, err
		}
		// age() against a constant is the one form whose moment of change
		// is known, so it is compared in whole nanoseconds against an exact
		// threshold rather than in floating seconds.
		if l.age != nil && r.konst || r.age != nil && l.konst {
			a, k, o := l, r, op.text
			if r.age != nil {
				a, k, o = r, l, flip(o)
			}
			a.age.compared = true
			v := k.n.eval(0)
			if !v.Known {
				return out, errAt(k.col, "the duration age() is compared with is not a number")
			}
			n := &ageCmp{s: a.age.s, op: o, c: int64(math.Round(v.Num * 1e9))}
			c.p.ages = append(c.p.ages, n)
			out.n, out.konst = n, false
			return out, nil
		}
	}
	out.n = &cmpNode{op: op.text, kind: l.t.Kind, l: l.n, r: r.n}
	return c.fold(out), nil
}

func flip(op string) string {
	switch op {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	}
	return op
}

// unify makes two numbers comparable or addable: a bare one takes the
// other's unit, and two with units must share a dimension.
func unify(op token, l, r cnode) (cnode, cnode, error) {
	switch {
	case l.bare && r.bare:
	case l.bare:
		l = adopt(l, r.t.Unit)
	case r.bare:
		r = adopt(r, l.t.Unit)
	case !l.t.Unit.SameDimension(r.t.Unit):
		return l, r, errAt(op.col, "%s: %s and %s are not the same kind of quantity", op.text, l.t.Unit, r.t.Unit)
	}
	return l, r, nil
}

// adopt gives a bare number u: 30 beside a channel in mV is 30 mV.
func adopt(n cnode, u Unit) cnode {
	n.bare, n.t.Unit = false, u
	if u.Scale != 1 {
		if n.konst {
			v := n.n.eval(0)
			v.Num *= u.Scale
			n.n = constNode{v}
		} else {
			n.n = &scaleNode{n.n, u.Scale}
		}
	}
	return n
}

func (c *compiler) noArith(op token) error {
	if c.p.profile == Condition {
		return errAt(op.col, "%s: a procedure condition compares and combines, and does no arithmetic; "+
			"compute the value in a keyword", op.text)
	}
	return nil
}

func (c *compiler) sum() (cnode, error) {
	l, err := c.prod()
	if err != nil {
		return l, err
	}
	for t := c.peek(); t.kind == tOp && (t.text == "+" || t.text == "-"); t = c.peek() {
		op := c.next()
		if err := c.noArith(op); err != nil {
			return l, err
		}
		r, err := c.prod()
		if err != nil {
			return r, err
		}
		if err := wantNumber(op, l, r); err != nil {
			return l, err
		}
		if l, r, err = unify(op, l, r); err != nil {
			return l, err
		}
		u := l.t.Unit
		if l.bare {
			u = r.t.Unit
		}
		l = c.fold(cnode{n: &arithNode{op: op.text[0], l: l.n, r: r.n}, t: Type{Kind: Number, Unit: u}, col: l.col,
			bare: l.bare && r.bare, konst: l.konst && r.konst, ref: -1})
	}
	return l, nil
}

func wantNumber(op token, ns ...cnode) error {
	for _, n := range ns {
		if n.t.Kind != Number {
			if op.kind == tName {
				return errAt(n.col, "%s() takes numbers, and this is %s", op.text, n.t)
			}
			return errAt(n.col, "%s needs a number on each side, and this is %s", op.text, n.t)
		}
	}
	return nil
}

func (c *compiler) prod() (cnode, error) {
	l, err := c.unary()
	if err != nil {
		return l, err
	}
	for t := c.peek(); t.kind == tOp && (t.text == "*" || t.text == "/"); t = c.peek() {
		op := c.next()
		if err := c.noArith(op); err != nil {
			return l, err
		}
		r, err := c.unary()
		if err != nil {
			return r, err
		}
		if err := wantNumber(op, l, r); err != nil {
			return l, err
		}
		sign := 1
		if op.text == "/" {
			sign = -1
		}
		lu, ru := l.t.Unit, r.t.Unit
		if l.bare {
			lu = None
		}
		if r.bare {
			ru = None
		}
		l = c.fold(cnode{n: &arithNode{op: op.text[0], l: l.n, r: r.n}, t: Type{Kind: Number, Unit: product(lu, ru, sign)},
			col: l.col, bare: l.bare && r.bare, konst: l.konst && r.konst, ref: -1})
	}
	return l, nil
}

func (c *compiler) unary() (cnode, error) {
	t := c.peek()
	if t.kind != tOp || t.text != "-" {
		return c.atom()
	}
	op := c.next()
	x, err := c.atom()
	if err != nil {
		return x, err
	}
	// A negative literal is a number, not arithmetic, in a condition too.
	if !x.konst {
		if err := c.noArith(op); err != nil {
			return x, err
		}
	}
	if err := wantNumber(op, x); err != nil {
		return x, err
	}
	x.n, x.ref, x.age = &negNode{x.n}, -1, nil
	return c.fold(x), nil
}

func (c *compiler) atom() (cnode, error) {
	t := c.next()
	switch t.kind {
	case tNum:
		n := cnode{t: Type{Kind: Number, Unit: None}, col: t.col, konst: true, bare: t.unit == "", ref: -1}
		v := t.num
		if t.unit != "" {
			u, ok := lookupUnit(t.unit)
			if !ok {
				return n, errAt(t.col, "%s: unknown unit %q; units are %s. "+
					"A number written bare takes the unit of what it is compared with", t.text, t.unit, unitList)
			}
			n.t.Unit, v = u, v*u.Scale
		}
		n.n = constNode{Num(v)}
		return n, nil
	case tStr:
		return cnode{n: constNode{StrValue(t.text)}, t: Type{Kind: String}, col: t.col, konst: true, ref: -1}, nil
	case tTrue, tFalse:
		return cnode{n: constNode{BoolValue(t.kind == tTrue)}, t: Type{Kind: Bool}, col: t.col, konst: true, ref: -1}, nil
	case tLParen:
		x, err := c.expr()
		if err != nil {
			return x, err
		}
		if e := c.next(); e.kind != tRParen {
			return x, errAt(e.col, "expected ) to close the ( at column %d", t.col)
		}
		x.col = t.col
		return x, nil
	case tVar:
		return c.ref(t, "$"+t.text)
	case tName:
		if c.peek().kind == tLParen {
			return c.call(t)
		}
		return c.ref(t, t.text)
	case tEOF:
		return cnode{}, errAt(t.col, "the expression ends where a value was expected")
	}
	return cnode{}, errAt(t.col, "%q where a value was expected", t.text)
}

func (c *compiler) ref(t token, name string) (cnode, error) {
	i, ok := c.p.byName[name]
	if !ok {
		typ, err := c.r.Resolve(name)
		if err != nil {
			return cnode{}, errAt(t.col, "%v", err)
		}
		if typ.Kind == Enum && typ.Enum == nil || typ.Kind < Number || typ.Kind > Enum {
			return cnode{}, errAt(t.col, "%s has no type the language knows", name)
		}
		if typ.Kind == Number && typ.Unit.Scale == 0 {
			typ.Unit.Scale = 1
		}
		i = len(c.p.slots)
		c.p.slots = append(c.p.slots, &slot{name: name, typ: typ})
		c.p.byName[name] = i
	}
	s := c.p.slots[i]
	return cnode{n: &refNode{s}, t: s.typ, col: t.col, ref: i}, nil
}

var funcs = map[string]bool{"abs": true, "min": true, "max": true,
	"age": true, "delta": true, "rate": true, "mean": true, "stable": true}

func (c *compiler) call(name token) (cnode, error) {
	c.next() // (
	if !funcs[name.text] {
		return cnode{}, errAt(name.col, "%s() is not a function; the functions are abs, min, max, age, delta, rate, mean and stable",
			name.text)
	}
	if c.p.profile == Condition {
		return cnode{}, errAt(name.col, "%s(): a procedure condition calls no functions; compute the value in a keyword",
			name.text)
	}
	var args []cnode
	if c.peek().kind != tRParen {
		for {
			a, err := c.expr()
			if err != nil {
				return a, err
			}
			args = append(args, a)
			if c.peek().kind != tComma {
				break
			}
			c.next()
		}
	}
	if e := c.next(); e.kind != tRParen {
		return cnode{}, errAt(e.col, "expected , or ) in the arguments of %s()", name.text)
	}
	arity := func(lo, hi int) error {
		if len(args) < lo || len(args) > hi {
			want := fmt.Sprint(lo)
			switch {
			case hi > 100:
				want = fmt.Sprintf("%d or more", lo)
			case hi != lo:
				want = fmt.Sprintf("%d or %d", lo, hi)
			}
			return errAt(name.col, "%s() takes %s arguments, and has %d", name.text, want, len(args))
		}
		return nil
	}
	out := cnode{col: name.col, ref: -1}
	switch name.text {
	case "abs":
		if err := arity(1, 1); err != nil {
			return out, err
		}
		if err := wantNumber(name, args[0]); err != nil {
			return out, err
		}
		a := args[0]
		return c.fold(cnode{n: &absNode{a.n}, t: a.t, col: name.col, bare: a.bare, konst: a.konst, ref: -1}), nil

	case "min", "max":
		if err := arity(2, math.MaxInt); err != nil {
			return out, err
		}
		if err := wantNumber(name, args...); err != nil {
			return out, err
		}
		// Every argument meets every other, as in a sum.
		acc := args[0]
		for i := 1; i < len(args); i++ {
			var err error
			if acc, args[i], err = unify(name, acc, args[i]); err != nil {
				return out, err
			}
			if acc.bare && !args[i].bare {
				acc.t.Unit, acc.bare = args[i].t.Unit, false
			}
		}
		konst := true
		ns := make([]node, len(args))
		for i, a := range args {
			if a.bare && !acc.bare {
				a = adopt(a, acc.t.Unit)
			}
			ns[i], konst = a.n, konst && a.konst
		}
		return c.fold(cnode{n: &minMaxNode{max: name.text == "max", xs: ns}, t: acc.t, col: name.col,
			bare: acc.bare, konst: konst, ref: -1}), nil
	}

	// The time functions read the history of one input.
	if len(args) == 0 || args[0].ref < 0 {
		return out, errAt(name.col, "%s() reads the history of a channel, so its first argument is a name, "+
			"not a value computed from one", name.text)
	}
	s := c.p.slots[args[0].ref]
	switch name.text {
	case "age":
		if err := arity(1, 1); err != nil {
			return out, err
		}
		a := &ageSite{col: name.col, s: s}
		c.agesSeen = append(c.agesSeen, a)
		out.n, out.t, out.age = &ageNode{s}, Type{Kind: Number, Unit: seconds}, a
		return out, nil

	case "delta":
		if err := arity(1, 2); err != nil {
			return out, err
		}
		if err := wantNumber(name, args[0]); err != nil {
			return out, err
		}
		d := &deltaNode{s: s}
		if len(args) == 2 {
			m := args[1]
			if !m.konst {
				return out, errAt(m.col, "delta()'s modulus is a constant, the count at which the value wraps")
			}
			if m.bare {
				m = adopt(m, s.typ.Unit)
			} else if !m.t.Unit.SameDimension(s.typ.Unit) {
				return out, errAt(m.col, "delta()'s modulus is in %s, and %s is in %s", m.t.Unit, s.name, s.typ.Unit)
			}
			v := m.n.eval(0)
			if !v.Known || !(v.Num > 0) {
				return out, errAt(m.col, "delta()'s modulus must be more than zero")
			}
			d.mod = v.Num
		}
		out.n, out.t = d, s.typ
		return out, nil

	case "rate", "mean", "stable":
		if err := arity(2, 2); err != nil {
			return out, err
		}
		w, err := window2(name, args[1])
		if err != nil {
			return out, err
		}
		switch name.text {
		case "stable":
			n := &stableNode{s: s, w: w}
			c.p.stables = append(c.p.stables, n)
			out.n, out.t = n, Type{Kind: Bool}
		case "rate":
			win := c.window(s, w)
			out.n, out.t = &rateNode{win}, Type{Kind: Number, Unit: None}
		case "mean":
			if err := wantNumber(name, args[0]); err != nil {
				return out, err
			}
			win := c.window(s, w)
			out.n, out.t = &meanNode{win}, s.typ
		}
		return out, nil
	}
	panic("unreachable")
}

// window2 checks a window argument: a constant time, more than zero.
func window2(name token, a cnode) (int64, error) {
	if !a.konst {
		return 0, errAt(a.col, "%s()'s window is a constant duration, such as 500ms", name.text)
	}
	if a.bare {
		return 0, errAt(a.col, "%s()'s window needs a unit, as in 500ms", name.text)
	}
	if !a.t.Unit.SameDimension(seconds) {
		return 0, errAt(a.col, "%s()'s window is a duration, and this is in %s", name.text, a.t.Unit)
	}
	v := a.n.eval(0)
	w := int64(math.Round(v.Num * 1e9))
	if w <= 0 {
		return 0, errAt(a.col, "%s()'s window must be longer than zero", name.text)
	}
	return w, nil
}

func (c *compiler) window(s *slot, w int64) *window {
	win := &window{w: w}
	s.windows = append(s.windows, win)
	c.p.windows = append(c.p.windows, win)
	return win
}

// fold replaces a node made of literals only by its value, so that 2*15ms
// costs nothing at run time and can serve as age()'s threshold.
func (c *compiler) fold(n cnode) cnode {
	if n.konst {
		if _, ok := n.n.(constNode); !ok {
			n.n = constNode{n.n.eval(0)}
		}
	}
	return n
}
