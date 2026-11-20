package procedure

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/trukeio/ktest/expr"
)

// TeardownTimeout bounds a [Teardown] that runs after the run was stopped or
// failed: it gets this long whatever happened before it, and no longer.
var TeardownTimeout = time.Minute

// Result is how a test ended.
type Result struct {
	Test    string
	Outcome Outcome // ok, or the outcome that failed it
	Msg     string  // why it failed
	Pos     Pos     // where it failed
	Start   time.Time
	End     time.Time
}

// Run runs one test case to its end: setup, body, teardown. Cancelling ctx
// stops it, as DELETE /procedures/run does: the step under way ends stopped,
// and the [Teardown] still runs, with TeardownTimeout to do it in. hard,
// cancelled when the daemon itself is going, cuts the teardown short too.
func (s *Suite) Run(ctx, hard context.Context, test string, host Host) Result {
	r := &runner{s: s, host: host, conds: map[string]*condEntry{}}
	res := Result{Test: test, Start: time.Now()}
	t := s.test(test)
	if t == nil {
		res.Outcome, res.Msg, res.End = Fail, fmt.Sprintf("no test %q; the tests are %s", test, strings.Join(s.Tests(), ", ")), time.Now()
		return res
	}
	res.Test = t.Name
	fr := &frame{vars: map[string]Value{}}

	body := ctx
	if t.Timeout != "" {
		d, err := r.duration(fr, t.TimeoutP, "[Timeout]", t.Timeout)
		if err != nil {
			return r.fail(res, err)
		}
		var cancel context.CancelFunc
		body, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	var err error
	if t.Setup != nil {
		_, err = r.call(body, fr, t.Setup, 0)
	}
	if err == nil {
		err = r.stmts(body, fr, t.Body, 0)
	}
	if t.Teardown != nil {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(hard), TeardownTimeout)
		go func() {
			select {
			case <-hard.Done():
				cancel()
			case <-tctx.Done():
			}
		}()
		_, terr := r.call(tctx, &frame{vars: map[string]Value{}}, t.Teardown, 0)
		cancel()
		if err == nil {
			err = terr
		}
	}
	if err != nil {
		return r.fail(res, err)
	}
	res.Outcome, res.End = OK, time.Now()
	return res
}

// Call runs one keyword of the suite's own, with no arguments, as a step of
// its own: the way the DUT rack runs an action. ctx bounds it; there is no
// [Teardown] at this level, so hard is not needed.
func (s *Suite) Call(ctx context.Context, keyword string, host Host) Result {
	r := &runner{s: s, host: host, conds: map[string]*condEntry{}}
	res := Result{Test: keyword, Start: time.Now()}
	k, ok := s.kws[norm(keyword)]
	if !ok {
		res.Outcome, res.Msg, res.End = Fail, fmt.Sprintf("no keyword %q", keyword), time.Now()
		return res
	}
	res.Test = k.Name
	if _, err := r.call(ctx, &frame{vars: map[string]Value{}}, &Call{Pos: k.Pos, Name: k.Name}, 0); err != nil {
		return r.fail(res, err)
	}
	res.Outcome, res.End = OK, time.Now()
	return res
}

// Keywords are the suite's own keywords' names, as written.
func (s *Suite) Keywords() []string {
	var out []string
	for _, k := range s.kws {
		out = append(out, k.Name)
	}
	sort.Strings(out)
	return out
}

func (r *runner) fail(res Result, err error) Result {
	res.Outcome, res.End = outcomeOf(err, nil), time.Now()
	res.Msg = message(err)
	var se *stepError
	if errors.As(err, &se) {
		res.Pos = se.pos
	}
	var pe *Error
	if errors.As(err, &pe) {
		res.Pos, res.Msg = pe.Pos, pe.Msg
	}
	return res
}

// stepError carries where a failure happened up through the calls it ends.
type stepError struct {
	pos Pos
	err error
}

func (e *stepError) Error() string { return e.err.Error() }
func (e *stepError) Unwrap() error { return e.err }

type runner struct {
	s     *Suite
	host  Host
	mu    sync.Mutex
	id    uint32
	conds map[string]*condEntry
}

// frame is one test's or keyword call's variables.
type frame struct {
	vars map[string]Value
}

var (
	errBreak    = errors.New("BREAK")
	errContinue = errors.New("CONTINUE")
)

type returned struct{ v Value }

func (returned) Error() string { return "RETURN" }

func (r *runner) lookup(fr *frame, rf *ref) (Value, error) {
	if rf.num != nil {
		return *rf.num, nil
	}
	v, ok := fr.vars[rf.name]
	if !ok {
		v, ok = r.s.vars[rf.name]
	}
	if !ok {
		v, ok = builtinVars[rf.name]
	}
	if !ok {
		return Value{}, Failf(Fail, "${%s} is not defined", rf.name)
	}
	if len(rf.fields) > 0 {
		x, err := field(v, rf)
		if err != nil {
			return Value{}, Failf(Fail, "%v", err)
		}
		v = x
	}
	return v, nil
}

// value resolves a cell.
func (r *runner) value(fr *frame, p Pos, raw string) (Value, *cell, error) {
	cl, err := r.s.cell(p, raw)
	if err != nil {
		return Value{}, nil, err
	}
	if rf := cl.whole(); rf != nil {
		v, err := r.lookup(fr, rf)
		return v, cl, err
	}
	var b strings.Builder
	for _, pt := range cl.parts {
		if pt.ref == nil {
			b.WriteString(pt.text)
			continue
		}
		v, err := r.lookup(fr, pt.ref)
		if err != nil {
			return Value{}, cl, err
		}
		b.WriteString(v.String())
	}
	return Str(b.String()), cl, nil
}

// values resolves cells, expanding @{list}.
func (r *runner) values(fr *frame, p Pos, raws []string) ([]Value, []string, error) {
	var vs []Value
	var texts []string
	for _, raw := range raws {
		v, cl, err := r.value(fr, p, raw)
		if err != nil {
			return nil, nil, err
		}
		if cl.list {
			if v.Kind != VList {
				return nil, nil, Failf(Fail, "%s is not a list", raw)
			}
			for _, e := range v.List {
				vs = append(vs, e)
				texts = append(texts, e.String())
			}
			continue
		}
		vs = append(vs, v)
		texts = append(texts, v.String())
	}
	return vs, texts, nil
}

func (r *runner) stmts(ctx context.Context, fr *frame, body []Stmt, parent uint32) error {
	for _, st := range body {
		if err := ctx.Err(); err != nil {
			return &stepError{st.pos(), err}
		}
		if err := r.stmt(ctx, fr, st, parent); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) stmt(ctx context.Context, fr *frame, st Stmt, parent uint32) error {
	switch s := st.(type) {
	case *Call:
		v, err := r.call(ctx, fr, s, parent)
		if err != nil {
			return err
		}
		return r.assign(fr, s, v)

	case *For:
		return r.forLoop(ctx, fr, s, parent)

	case *If:
		for _, br := range s.Branches {
			if br.Cond != "" {
				ok, err := r.cond(fr, br.Pos, br.Cond)
				if err != nil {
					return &stepError{br.Pos, err}
				}
				if !ok {
					continue
				}
			}
			return r.stmts(ctx, fr, br.Body, parent)
		}
		return nil

	case *While:
		lv, _, err := r.value(fr, s.Pos, s.Limit)
		if err != nil {
			return &stepError{s.Pos, err}
		}
		limit, err := parseLimit(s.Pos, lv)
		if err != nil {
			return err
		}
		start := time.Now()
		for n := 0; ; n++ {
			ok, err := r.cond(fr, s.Pos, s.Cond)
			if err != nil {
				return &stepError{s.Pos, err}
			}
			if !ok {
				return nil
			}
			if limit.Unit.Dimensionless() && n >= int(limit.Num) ||
				!limit.Unit.Dimensionless() && time.Since(start) >= time.Duration(limit.Num*1e9) {
				return &stepError{s.Pos, Failf(Fail, "WHILE reached its limit of %s with its condition still true", limit.String())}
			}
			err = r.stmts(ctx, fr, s.Body, parent)
			switch {
			case errors.Is(err, errBreak):
				return nil
			case errors.Is(err, errContinue):
			case err != nil:
				return err
			}
		}

	case *Break:
		return errBreak
	case *Continue:
		return errContinue
	case *Return:
		vs, _, err := r.values(fr, s.Pos, s.Values)
		if err != nil {
			return &stepError{s.Pos, err}
		}
		switch len(vs) {
		case 0:
			return returned{}
		case 1:
			return returned{vs[0]}
		}
		return returned{Value{Kind: VList, List: vs}}
	}
	return nil
}

func (r *runner) forLoop(ctx context.Context, fr *frame, s *For, parent uint32) error {
	var items []Value
	if s.Range {
		vs, _, err := r.values(fr, s.Pos, s.Items)
		if err != nil {
			return &stepError{s.Pos, err}
		}
		nums := make([]float64, len(vs))
		for i, v := range vs {
			n, err := convert(Quantity("FOR ... IN RANGE", ""), v)
			if err != nil {
				return &stepError{s.Pos, Failf(Fail, "FOR ... IN RANGE: %v", err)}
			}
			nums[i] = n.Num
		}
		start, end, step := 0.0, nums[0], 1.0
		if len(nums) > 1 {
			start, end = nums[0], nums[1]
		}
		if len(nums) > 2 {
			step = nums[2]
		}
		if step == 0 {
			return &stepError{s.Pos, Failf(Fail, "FOR ... IN RANGE: a step of zero never ends")}
		}
		for x := start; step > 0 && x < end || step < 0 && x > end; x += step {
			items = append(items, Number(x, expr.None))
		}
	} else {
		vs, _, err := r.values(fr, s.Pos, s.Items)
		if err != nil {
			return &stepError{s.Pos, err}
		}
		items = vs
		if len(items)%len(s.Vars) != 0 {
			return &stepError{s.Pos, Failf(Fail, "FOR with %d variables has %d items", len(s.Vars), len(items))}
		}
	}
	for i := 0; i+len(s.Vars) <= len(items); i += len(s.Vars) {
		for j, v := range s.Vars {
			fr.vars[v] = items[i+j]
		}
		err := r.stmts(ctx, fr, s.Body, parent)
		switch {
		case errors.Is(err, errBreak):
			return nil
		case errors.Is(err, errContinue):
		case err != nil:
			return err
		}
	}
	return nil
}

func (r *runner) assign(fr *frame, s *Call, v Value) error {
	switch len(s.Assign) {
	case 0:
		return nil
	case 1:
		fr.vars[s.Assign[0]] = v
		return nil
	}
	if v.Kind != VList || len(v.List) != len(s.Assign) {
		return &stepError{s.Pos, Failf(Fail, "%d variables are assigned, and %s returned %s", len(s.Assign), s.Name, v.String())}
	}
	for i, n := range s.Assign {
		fr.vars[n] = v.List[i]
	}
	return nil
}

func (r *runner) nextID() uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.id++
	return r.id
}

// call runs one keyword call, recorded as a step.
func (r *runner) call(ctx context.Context, fr *frame, s *Call, parent uint32) (Value, error) {
	id := r.nextID()
	ev := StepEvent{ID: id, Parent: parent, Pos: s.Pos, Keyword: s.Name, Start: time.Now()}
	end := func(v Value, err error) (Value, error) {
		ev.End, ev.Stop = true, time.Now()
		ev.Outcome = outcomeOf(err, ctx)
		if err != nil {
			ev.Msg = message(err)
		} else if v.Kind != VNone {
			ev.Msg = v.String()
			if v.Kind == VString && (v.Str == string(NOK) || v.Str == string(OK)) {
				ev.Outcome = Outcome(v.Str)
			}
		}
		r.host.Step(ev)
		if err != nil {
			var se *stepError
			if !errors.As(err, &se) {
				err = &stepError{s.Pos, err}
			}
		}
		return v, err
	}

	if k, ok := r.s.kws[norm(s.Name)]; ok {
		_, texts, err := r.values(fr, s.Pos, s.Args)
		ev.Args = texts
		r.host.Step(ev)
		if err != nil {
			return end(Value{}, err)
		}
		return end(r.userCall(ctx, fr, s, k, id))
	}
	k, ok := r.s.Lib.Keyword(s.Name)
	if !ok {
		r.host.Step(ev)
		return end(Value{}, Failf(Fail, "no keyword named %q", s.Name))
	}
	args, conds, texts, err := r.bindLib(fr, s, k)
	ev.Args = texts
	r.host.Step(ev)
	if err != nil {
		return end(Value{}, err)
	}
	v, err := k.Run(&Ctx{Context: ctx, Host: r.host, conds: conds, run: r}, args)
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return end(v, err)
}

// bindLib resolves a library call's arguments and converts each to what its
// parameter takes.
func (r *runner) bindLib(fr *frame, s *Call, k *Keyword) (Args, map[string]*Condition, []string, error) {
	// Conditions are not cells: they are passed as written.
	names, defs := paramNames(k.Params)
	b, err := bind(s.Pos, k.Name, names, defs, r.expand(fr, s), false)
	if err != nil {
		return nil, nil, s.Args, err
	}
	args := Args{}
	conds := map[string]*Condition{}
	var texts []string
	for i, prm := range k.Params {
		if prm.Kind == PCondition {
			conds[prm.Name] = &Condition{r: r, fr: fr, pos: s.Pos, src: b[i].raw}
			texts = append(texts, b[i].raw)
			continue
		}
		v, _, err := r.value(fr, s.Pos, b[i].raw)
		if err != nil {
			return nil, nil, texts, err
		}
		cv, err := convert(prm, v)
		if err != nil {
			return nil, nil, texts, Failf(Fail, "%s: %s: %v", k.Name, prm.Name, err)
		}
		args[prm.Name] = cv
		if b[i].given {
			texts = append(texts, prm.Name+"="+cv.String())
		}
	}
	return args, conds, texts, nil
}

// expand turns @{list} cells into one cell per item, written as references
// to the items so that each keeps its type.
func (r *runner) expand(fr *frame, s *Call) []string {
	var out []string
	for _, raw := range s.Args {
		cl, err := r.s.cell(s.Pos, raw)
		if err != nil || !cl.list {
			out = append(out, raw)
			continue
		}
		v, err := r.lookup(fr, cl.whole())
		if err != nil || v.Kind != VList {
			out = append(out, raw)
			continue
		}
		for i := range v.List {
			name := fmt.Sprintf("@%s.%d", cl.whole().name, i)
			fr.vars[norm(name)] = v.List[i]
			out = append(out, "${"+name+"}")
		}
	}
	return out
}

func (r *runner) userCall(ctx context.Context, fr *frame, s *Call, k *Block, id uint32) (Value, error) {
	names := make([]string, len(k.Args))
	defs := make([]*string, len(k.Args))
	for i, a := range k.Args {
		names[i], defs[i] = strings.TrimSuffix(strings.TrimPrefix(a.Written, "${"), "}"), a.Default
	}
	b, err := bind(s.Pos, k.Name, names, defs, r.expand(fr, s), false)
	if err != nil {
		return Value{}, err
	}
	inner := &frame{vars: map[string]Value{}}
	for i, a := range k.Args {
		src := fr
		if b[i].isDef {
			src = inner // a default may use the arguments before it
		}
		v, _, err := r.value(src, s.Pos, b[i].raw)
		if err != nil {
			return Value{}, err
		}
		inner.vars[a.Name] = v
	}
	if k.Timeout != "" {
		d, err := r.duration(inner, k.TimeoutP, "[Timeout]", k.Timeout)
		if err != nil {
			return Value{}, err
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	var ret Value
	if k.Setup != nil {
		_, err = r.call(ctx, inner, k.Setup, id)
	}
	if err == nil {
		err = r.stmts(ctx, inner, k.Body, id)
		var rv returned
		if errors.As(err, &rv) {
			ret, err = rv.v, nil
		}
	}
	if k.Teardown != nil {
		_, terr := r.call(context.WithoutCancel(ctx), inner, k.Teardown, id)
		if err == nil {
			err = terr
		}
	}
	return ret, err
}

func (r *runner) duration(fr *frame, p Pos, what, raw string) (time.Duration, error) {
	v, _, err := r.value(fr, p, raw)
	if err != nil {
		return 0, err
	}
	d, err := convert(Duration(what), v)
	if err != nil {
		return 0, errAt(p, "%s: %v", what, err)
	}
	return time.Duration(math.Round(d.Num * 1e9)), nil
}

// Condition is a condition as a keyword receives it: compiled when it is
// evaluated, against what its variables then hold, and evaluated over the
// channels' latest values.
type Condition struct {
	r   *runner
	fr  *frame
	pos Pos
	src string
}

// Source is the condition as written.
func (c *Condition) Source() string { return c.src }

// Eval evaluates the condition now. An unknown answer — a channel with no
// value yet — is an error, since a procedure acting on it would be acting on
// nothing.
func (c *Condition) Eval() (bool, error) { return c.r.cond(c.fr, c.pos, c.src) }

// runResolver types a condition's names at run time.
type runResolver struct {
	r  *runner
	fr *frame
}

func (rr *runResolver) Resolve(name string) (expr.Type, error) {
	if strings.HasPrefix(name, "$") {
		rf, _ := parseRef(Pos{}, name, name[1:])
		v, err := rr.r.lookup(rr.fr, rf)
		if err != nil {
			return expr.Type{}, err
		}
		t, ok := exprType(v)
		if !ok {
			return expr.Type{}, fmt.Errorf("${%s} holds %s, which a condition cannot compare", rf.name, v.String())
		}
		return t, nil
	}
	return rr.r.host.Resolve(name)
}

// cond evaluates a condition. The program is compiled for the types its
// variables hold now, and kept by them: a loop does not recompile it, and the
// same condition over a variable holding volts and one holding text gets a
// program for each.
func (r *runner) cond(fr *frame, p Pos, src string) (bool, error) {
	r.mu.Lock()
	ce := r.conds[src]
	r.mu.Unlock()
	sig := func(inputs []string) (string, error) {
		var b strings.Builder
		for _, name := range inputs {
			if !strings.HasPrefix(name, "$") {
				continue
			}
			rf, _ := parseRef(p, name, name[1:])
			v, err := r.lookup(fr, rf)
			if err != nil {
				return "", err
			}
			ty, _ := exprType(v)
			fmt.Fprintf(&b, "%s=%d/%s/%p\x00", name, ty.Kind, ty.Unit.Name, ty.Enum)
		}
		return b.String(), nil
	}
	var prog *expr.Program
	if ce != nil {
		key, err := sig(ce.inputs)
		if err != nil {
			return false, err
		}
		prog = ce.progs[key]
	}
	if prog == nil {
		var err error
		prog, err = expr.Compile(src, expr.Condition, &runResolver{r, fr})
		if err != nil {
			return false, Failf(Fail, "condition %q: %v", src, err)
		}
		key, err := sig(prog.Inputs())
		if err != nil {
			return false, err
		}
		r.mu.Lock()
		if ce == nil {
			ce = &condEntry{inputs: prog.Inputs(), progs: map[string]*expr.Program{}}
			r.conds[src] = ce
		}
		ce.progs[key] = prog
		r.mu.Unlock()
	}
	now := int64(0)
	for i, name := range prog.Inputs() {
		var v expr.Value
		t := int64(0)
		if strings.HasPrefix(name, "$") {
			rf, _ := parseRef(p, name, name[1:])
			x, err := r.lookup(fr, rf)
			if err != nil {
				return false, err
			}
			v = exprValue(x)
		} else {
			var ok bool
			v, t, ok = r.host.Value(name)
			if !ok {
				return false, Failf(Fail, "condition %q: %s has no value yet", src, name)
			}
		}
		// Each evaluation feeds the inputs afresh. The condition profile has
		// no time functions, so only the latest value matters; a sample is
		// never given a time before the one the program last saw.
		t = max(t, prog.LastSample(i))
		if err := prog.Observe(i, t, v); err != nil {
			return false, Failf(Fail, "condition %q: %v", src, err)
		}
		now = max(now, t)
	}
	res := prog.Eval(now)
	if !res.Known {
		return false, Failf(Fail, "condition %q has no answer: a value it reads is not known", src)
	}
	return res.Bool, nil
}

// condEntry is one condition's programs, by the types of its variables.
type condEntry struct {
	inputs []string
	progs  map[string]*expr.Program
}

// Bare says, with a sentence when it cannot, whether a keyword of the suite's
// own can be called with no arguments, as Call calls it.
func (s *Suite) Bare(keyword string) error {
	k, ok := s.kws[norm(keyword)]
	if !ok {
		return fmt.Errorf("no keyword %q", keyword)
	}
	for _, a := range k.Args {
		if a.Default == nil {
			return fmt.Errorf("keyword %q takes %s, and an action is called with no arguments", k.Name, a.Written)
		}
	}
	return nil
}
