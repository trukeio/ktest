package procedure

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trukeio/ktest/expr"
)

// Env is what a procedure is loaded against.
type Env struct {
	Lib *Library
	// Channels types the channel names a condition may read; nil for none.
	Channels expr.Resolver
}

// Suite is a loaded procedure: its file, the resources it pulled in, and
// everything checked.
type Suite struct {
	Path string
	// Hash is the sha256 of every file the procedure was loaded from, the
	// main file first and then each resource and variable file in the order
	// they were read, each preceded by its path relative to the main file.
	Hash  string
	Files []string
	// Sums is the sha256 of each of Files, hex, by the same relative path:
	// what a locked daemon holds each file to (handoff §4.9). They are sums
	// of the bytes that were parsed, so nothing can change between the check
	// and the run.
	Sums map[string]string
	Lib  *Library
	Doc  string

	env   Env
	tests []*Block
	kws   map[string]*Block // user keywords, by normalised name
	vars  map[string]Value  // suite variables, by normalised name
	cells map[string]*cell
}

// Tests are the suite's test cases, in file order.
func (s *Suite) Tests() []string {
	out := make([]string, len(s.tests))
	for i, t := range s.tests {
		out[i] = t.Name
	}
	return out
}

func (s *Suite) test(name string) *Block {
	for _, t := range s.tests {
		if norm(t.Name) == norm(name) {
			return t
		}
	}
	return nil
}

// Load reads a procedure and everything it refers to, and checks all of it.
// Nothing runs until all of this has passed:
//
//   - every keyword is known, and none is refused;
//   - every argument name is one the keyword has, and none is missing;
//   - every argument with a unit has the right one;
//   - every variable is defined where it is used;
//   - every condition compiles, with the names it reads typed;
//   - no keyword calls itself, directly or through others.
func Load(path string, env Env) (*Suite, error) { return load(path, env, false) }

// LoadResource reads a resource file — keywords and variables, no test cases —
// and checks it as Load checks a procedure, every keyword in it whether or
// not anything calls it. Its keywords are run one at a time with Call: the
// DUT rack's actions are such keywords (handoff C9).
func LoadResource(path string, env Env) (*Suite, error) { return load(path, env, true) }

func load(path string, env Env, resource bool) (*Suite, error) {
	if env.Lib == nil {
		env.Lib = NewLibrary("")
	}
	s := &Suite{Path: path, Lib: env.Lib, env: env, kws: map[string]*Block{}, vars: map[string]Value{},
		cells: map[string]*cell{}, Sums: map[string]string{}}
	h := sha256.New()
	base := filepath.Dir(path)
	seen := map[string]bool{}
	hashFile := func(p string, b []byte) {
		rel, err := filepath.Rel(base, p)
		if err != nil {
			rel = p
		}
		s.Files = append(s.Files, rel)
		sum := sha256.Sum256(b)
		s.Sums[rel] = hex.EncodeToString(sum[:])
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), len(b))
		h.Write(b)
	}

	var load func(p string, from *Setting, main bool) error
	load = func(p string, from *Setting, main bool) error {
		abs, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		if seen[abs] {
			return nil
		}
		seen[abs] = true
		b, err := os.ReadFile(p)
		if err != nil {
			if from != nil {
				return errAt(from.Pos, "Resource %s: %v", from.Path, err)
			}
			return err
		}
		hashFile(p, b)
		// Positions are relative to the procedure's directory: shorter to
		// read, and the same wherever the procedure is installed, which is
		// what the step records carry.
		rel, err := filepath.Rel(base, p)
		if err != nil {
			rel = p
		}
		f, err := Parse(filepath.ToSlash(rel), string(b))
		if err != nil {
			return err
		}
		if main && resource {
			s.Doc = f.Doc
			if len(f.Tests) > 0 {
				return errAt(f.Tests[0].Pos, "a resource file has no test cases")
			}
		} else if main {
			s.Doc = f.Doc
			s.tests = f.Tests
			if len(f.Tests) == 0 {
				return errAt(Pos{p, 1}, "a procedure has at least one test case, under *** Test Cases ***")
			}
			seenT := map[string]Pos{}
			for _, t := range f.Tests {
				if was, dup := seenT[norm(t.Name)]; dup {
					return errAt(t.Pos, "test %q is already defined at %s", t.Name, was)
				}
				seenT[norm(t.Name)] = t.Pos
			}
		} else if len(f.Tests) > 0 {
			return errAt(f.Tests[0].Pos, "a resource file has no test cases; this one is used as a resource from %s", from.Pos)
		}
		for _, k := range f.Keywords {
			n := norm(k.Name)
			if was, dup := s.kws[n]; dup {
				return errAt(k.Pos, "keyword %q is already defined at %s", k.Name, was.Pos)
			}
			if _, lib := env.Lib.Keyword(k.Name); lib {
				return errAt(k.Pos, "keyword %q has the name of a library keyword; give it another", k.Name)
			}
			if msg, refused := refusal(k.Name); refused {
				return errAt(k.Pos, "keyword %q: %s", k.Name, msg)
			}
			s.kws[n] = k
		}
		for _, vf := range f.VarFiles {
			vp := filepath.Join(filepath.Dir(p), vf.Path)
			if err := s.varFile(vp, vf, hashFile); err != nil {
				return err
			}
		}
		if err := s.varSection(f.Vars); err != nil {
			return err
		}
		for _, r := range f.Resources {
			if err := load(filepath.Join(filepath.Dir(p), r.Path), &r, false); err != nil {
				return err
			}
		}
		return nil
	}
	if err := load(path, nil, true); err != nil {
		return nil, err
	}
	s.Hash = hex.EncodeToString(h.Sum(nil))

	c := &checker{s: s, memo: map[string]Static{}, checked: map[*Block]bool{}}
	for _, t := range s.tests {
		if _, err := c.block(t, nil, true); err != nil {
			return nil, err
		}
	}
	// A keyword nobody calls is still checked, so that a resource is refused
	// for a mistake in a keyword before the day someone calls it. What its
	// arguments are is not known, so a condition over them waits for a call.
	for _, n := range slices.Sorted(maps.Keys(s.kws)) {
		k := s.kws[n]
		if c.checked[k] {
			continue
		}
		args := map[string]Static{}
		for _, a := range k.Args {
			args[a.Name] = Static{}
		}
		if _, err := c.block(k, args, false); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// varSection evaluates a Variables section, in order: a value may use the
// variables above it.
func (s *Suite) varSection(defs []*VarDef) error {
	sc := &scope{vars: map[string]Static{}}
	for n, v := range s.vars {
		sc.vars[n] = staticOf(v)
	}
	for _, d := range defs {
		n := norm(d.Name)
		var vals []Value
		for _, raw := range d.Values {
			if d.Sigil == '&' {
				continue
			}
			c, err := s.cell(d.Pos, raw)
			if err != nil {
				return err
			}
			st, err := (&checker{s: s}).cellStatic(d.Pos, c, sc)
			if err != nil {
				return err
			}
			if !st.Known {
				return errAt(d.Pos, "%s: a suite variable's value is known when the procedure loads", raw)
			}
			if c.list {
				vals = append(vals, st.Val.List...)
			} else {
				vals = append(vals, st.Val)
			}
		}
		var v Value
		switch d.Sigil {
		case '$':
			v = Str("")
			if len(vals) == 1 {
				v = vals[0]
			}
		case '@':
			v = Value{Kind: VList, List: vals}
		case '&':
			v = Value{Kind: VDict, Dict: map[string]Value{}}
			for _, kv := range d.Values {
				k, raw, _ := strings.Cut(kv, "=")
				c, err := s.cell(d.Pos, raw)
				if err != nil {
					return err
				}
				st, err := (&checker{s: s}).cellStatic(d.Pos, c, sc)
				if err != nil {
					return err
				}
				if !st.Known {
					return errAt(d.Pos, "%s: a suite variable's value is known when the procedure loads", raw)
				}
				if _, dup := v.Dict[k]; !dup {
					v.Keys = append(v.Keys, k)
				}
				v.Dict[k] = st.Val
			}
		}
		s.vars[n] = v
		sc.vars[n] = staticOf(v)
	}
	return nil
}

// varFile reads a JSON variable file: each key a variable.
func (s *Suite) varFile(p string, from Setting, hashFile func(string, []byte)) error {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".json":
	case ".yaml", ".yml":
		return errAt(from.Pos, "Variables %s: YAML variable files are not read yet; write it as JSON", from.Path)
	default:
		return errAt(from.Pos, "Variables %s: a variable file is JSON", from.Path)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return errAt(from.Pos, "Variables %s: %v", from.Path, err)
	}
	hashFile(p, b)
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return errAt(from.Pos, "Variables %s: %v", from.Path, err)
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		v, err := fromJSON(m[k])
		if err != nil {
			return errAt(from.Pos, "Variables %s: %s: %v", from.Path, k, err)
		}
		s.vars[norm(k)] = v
	}
	return nil
}

func fromJSON(x any) (Value, error) {
	switch t := x.(type) {
	case string:
		return Str(t), nil
	case json.Number:
		f, err := t.Float64()
		return Number(f, expr.None), err
	case bool:
		return BoolV(t), nil
	case nil:
		return Value{}, nil
	case []any:
		v := Value{Kind: VList}
		for _, e := range t {
			ev, err := fromJSON(e)
			if err != nil {
				return v, err
			}
			v.List = append(v.List, ev)
		}
		return v, nil
	case map[string]any:
		v := Value{Kind: VDict, Dict: map[string]Value{}}
		for _, k := range slices.Sorted(maps.Keys(t)) {
			ev, err := fromJSON(t[k])
			if err != nil {
				return v, err
			}
			v.Keys = append(v.Keys, k)
			v.Dict[k] = ev
		}
		return v, nil
	}
	return Value{}, fmt.Errorf("%v is not a value a variable can hold", x)
}

// cell parses a cell once; the same text always parses the same way.
func (s *Suite) cell(p Pos, raw string) (*cell, error) {
	if c, ok := s.cells[raw]; ok {
		return c, nil
	}
	c, err := parseCell(p, raw)
	if err != nil {
		return nil, err
	}
	s.cells[raw] = c
	return c, nil
}

// Static is what is known about a value when the procedure loads: the value
// itself, when it is written in the procedure, or else what kind of value it
// is, when a keyword says so.
type Static struct {
	Known bool
	Val   Value
	Kind  VKind     // VNone when not known
	Typed bool      // T is its type in a condition
	T     expr.Type // a number's unit, among other things
}

func staticOf(v Value) Static {
	st := Static{Known: true, Val: v, Kind: v.Kind}
	st.T, st.Typed = exprType(v)
	return st
}

// StaticNumber is a number in u whose value is not known at load.
func StaticNumber(u expr.Unit) Static {
	return Static{Kind: VNumber, Typed: true, T: expr.Type{Kind: expr.Number, Unit: u}}
}

func sameType(a, b expr.Type) bool {
	return a.Kind == b.Kind && (a.Kind != expr.Number || a.Unit.SameDimension(b.Unit)) && a.Enum == b.Enum
}

// join is what is known of a value that is one of a or b.
func join(a, b Static) Static {
	if a.Known && b.Known && a.Val.Kind == b.Val.Kind && a.Val.String() == b.Val.String() {
		return a
	}
	out := Static{}
	if a.Kind == b.Kind {
		out.Kind = a.Kind
	}
	if a.Typed && b.Typed && sameType(a.T, b.T) {
		out.Typed, out.T = true, a.T
	}
	return out
}

func (st Static) fingerprint() string {
	switch {
	case st.Known:
		return fmt.Sprintf("=%d:%s", st.Val.Kind, st.Val.String())
	case st.Typed:
		return fmt.Sprintf("t%d:%s", st.T.Kind, st.T.Unit.String())
	}
	return fmt.Sprintf("k%d", st.Kind)
}

// scope is the variables defined at a point of a body, as known at load.
type scope struct {
	vars map[string]Static
}

func (sc *scope) clone() *scope {
	n := &scope{vars: make(map[string]Static, len(sc.vars))}
	for k, v := range sc.vars {
		n.vars[k] = v
	}
	return n
}

type checker struct {
	s       *Suite
	memo    map[string]Static
	checked map[*Block]bool
	stack   []*Block
	via     []Pos // the calls that led to the keyword being checked
}

// lookup is a variable as known at load: local, then suite, then built in.
func (c *checker) lookup(sc *scope, r *ref) (Static, bool) {
	if r.num != nil {
		return staticOf(*r.num), true
	}
	var st Static
	if v, ok := sc.vars[r.name]; ok {
		st = v
	} else if v, ok := c.s.vars[r.name]; ok {
		st = staticOf(v)
	} else if v, ok := builtinVars[r.name]; ok {
		st = staticOf(v)
	} else {
		return Static{}, false
	}
	return st, true
}

func (c *checker) refStatic(p Pos, r *ref, sc *scope) (Static, error) {
	st, ok := c.lookup(sc, r)
	if !ok {
		return st, errAt(p, "${%s} is not defined here", r.name)
	}
	if len(r.fields) == 0 {
		return st, nil
	}
	if !st.Known {
		return Static{}, nil
	}
	v, err := field(st.Val, r)
	if err != nil {
		return st, errAt(p, "%v", err)
	}
	return staticOf(v), nil
}

// cellStatic is what is known of a cell's value.
func (c *checker) cellStatic(p Pos, cl *cell, sc *scope) (Static, error) {
	if r := cl.whole(); r != nil {
		st, err := c.refStatic(p, r, sc)
		if err != nil {
			return st, err
		}
		if cl.list && st.Known && st.Val.Kind != VList {
			return st, errAt(p, "@{%s} expands a list, and ${%s} is %s", r.name, r.name, st.Val.String())
		}
		return st, nil
	}
	var b strings.Builder
	known := true
	for _, pt := range cl.parts {
		if pt.ref == nil {
			b.WriteString(pt.text)
			continue
		}
		st, err := c.refStatic(p, pt.ref, sc)
		if err != nil {
			return st, err
		}
		if !st.Known {
			known = false
			continue
		}
		b.WriteString(st.Val.String())
	}
	if !known {
		return Static{Kind: VString}, nil
	}
	return staticOf(Str(b.String())), nil
}

// block checks a test or keyword with what is known of its arguments, and
// says what it returns. strict is false for a keyword nobody calls, whose
// arguments are not known.
func (c *checker) block(b *Block, args map[string]Static, strict bool) (Static, error) {
	c.checked[b] = true
	sc := &scope{vars: map[string]Static{}}
	for k, v := range args {
		sc.vars[k] = v
	}
	// Defaults may use the arguments before them.
	if b.Keyword {
		for _, a := range b.Args {
			if _, given := args[a.Name]; given || a.Default == nil {
				continue
			}
			cl, err := c.s.cell(b.Pos, *a.Default)
			if err != nil {
				return Static{}, err
			}
			st, err := c.cellStatic(b.Pos, cl, sc)
			if err != nil {
				return Static{}, err
			}
			sc.vars[a.Name] = st
		}
	}
	if b.Timeout != "" {
		if _, err := c.duration(b.TimeoutP, "[Timeout]", b.Timeout, sc); err != nil {
			return Static{}, err
		}
	}
	if b.Setup != nil {
		if _, err := c.call(b.Setup, sc.clone(), strict); err != nil {
			return Static{}, err
		}
	}
	// The teardown runs whatever the body did, so it sees only what was
	// defined before the body.
	if b.Teardown != nil {
		if _, err := c.call(b.Teardown, sc.clone(), strict); err != nil {
			return Static{}, err
		}
	}
	fl := &flow{keyword: b.Keyword}
	if _, err := c.stmts(b.Body, sc, fl, strict); err != nil {
		return Static{}, err
	}
	return fl.ret, nil
}

// flow is what a body's statements need to know about where they are.
type flow struct {
	keyword bool
	loops   int
	ret     Static
	rets    int
}

func (c *checker) stmts(body []Stmt, sc *scope, fl *flow, strict bool) (*scope, error) {
	for _, st := range body {
		switch s := st.(type) {
		case *Call:
			ret, err := c.call(s, sc, strict)
			if err != nil {
				return sc, err
			}
			if err := c.assign(s, ret, sc); err != nil {
				return sc, err
			}

		case *For:
			inner := sc.clone()
			var item Static
			if s.Range {
				for _, raw := range s.Items {
					if _, err := c.number(s.Pos, "FOR ... IN RANGE", raw, sc); err != nil {
						return sc, err
					}
				}
				item = StaticNumber(expr.None)
			} else {
				if len(s.Items) == 0 {
					return sc, errAt(s.Pos, "FOR ... IN has nothing to loop over")
				}
				first := true
				for _, raw := range s.Items {
					cl, err := c.s.cell(s.Pos, raw)
					if err != nil {
						return sc, err
					}
					st, err := c.cellStatic(s.Pos, cl, sc)
					if err != nil {
						return sc, err
					}
					elems := []Static{st}
					if cl.list {
						elems = nil
						if st.Known {
							for _, e := range st.Val.List {
								elems = append(elems, staticOf(e))
							}
						} else {
							elems = []Static{{}}
						}
					}
					for _, e := range elems {
						// The loop variable is any item: the value is not
						// known, what kind of value it is may be.
						e.Known = false
						if first {
							item, first = e, false
						} else {
							item = join(item, e)
						}
					}
				}
				if len(s.Vars) > 1 && len(s.Items)%len(s.Vars) != 0 {
					return sc, errAt(s.Pos, "FOR with %d variables needs a multiple of %d items, and has %d",
						len(s.Vars), len(s.Vars), len(s.Items))
				}
			}
			for _, v := range s.Vars {
				inner.vars[v] = item
			}
			fl.loops++
			_, err := c.stmts(s.Body, inner, fl, strict)
			fl.loops--
			if err != nil {
				return sc, err
			}
			// A loop may run no times, so what it defines is not defined
			// after it.

		case *If:
			var outs []*scope
			hasElse := false
			for _, br := range s.Branches {
				if br.Cond == "" {
					hasElse = true
				} else if err := c.condition(br.Pos, br.Cond, sc, strict); err != nil {
					return sc, err
				}
				out, err := c.stmts(br.Body, sc.clone(), fl, strict)
				if err != nil {
					return sc, err
				}
				outs = append(outs, out)
			}
			// A variable is defined after the IF when every branch defines
			// it, and there is an ELSE, so that one always runs.
			if hasElse {
				for n, v := range outs[0].vars {
					if _, before := sc.vars[n]; before {
						continue
					}
					all := true
					for _, o := range outs[1:] {
						w, ok := o.vars[n]
						if !ok {
							all = false
							break
						}
						v = join(v, w)
					}
					if all {
						sc.vars[n] = v
					}
				}
			}

		case *While:
			if err := c.condition(s.Pos, s.Cond, sc, strict); err != nil {
				return sc, err
			}
			if _, err := c.whileLimit(s, sc); err != nil {
				return sc, err
			}
			fl.loops++
			_, err := c.stmts(s.Body, sc.clone(), fl, strict)
			fl.loops--
			if err != nil {
				return sc, err
			}

		case *Break, *Continue:
			if fl.loops == 0 {
				return sc, errAt(st.pos(), "%s outside FOR or WHILE", map[bool]string{true: "BREAK", false: "CONTINUE"}[isBreak(st)])
			}

		case *Return:
			if !fl.keyword {
				return sc, errAt(s.Pos, "RETURN is for keywords; a test ends at its last step")
			}
			var vals []Static
			for _, raw := range s.Values {
				cl, err := c.s.cell(s.Pos, raw)
				if err != nil {
					return sc, err
				}
				st, err := c.cellStatic(s.Pos, cl, sc)
				if err != nil {
					return sc, err
				}
				vals = append(vals, st)
			}
			var r Static
			switch len(vals) {
			case 0:
				r = staticOf(Value{})
			case 1:
				r = vals[0]
			default:
				lv := Value{Kind: VList}
				known := true
				for _, v := range vals {
					known = known && v.Known
					lv.List = append(lv.List, v.Val)
				}
				r = Static{Kind: VList}
				if known {
					r = staticOf(lv)
				}
			}
			if fl.rets == 0 {
				fl.ret = r
			} else {
				fl.ret = join(fl.ret, r)
			}
			fl.rets++
		}
	}
	return sc, nil
}

func isBreak(s Stmt) bool { _, ok := s.(*Break); return ok }

func (c *checker) assign(s *Call, ret Static, sc *scope) error {
	switch len(s.Assign) {
	case 0:
		return nil
	case 1:
		sc.vars[s.Assign[0]] = ret
		return nil
	}
	if ret.Kind != VList && ret.Kind != VNone {
		return errAt(s.Pos, "%s returns one value, and %d variables are assigned", s.Name, len(s.Assign))
	}
	for i, n := range s.Assign {
		st := Static{}
		if ret.Known && i < len(ret.Val.List) {
			st = staticOf(ret.Val.List[i])
		}
		sc.vars[n] = st
	}
	return nil
}

var namedArg = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_ ]*)=`)

// binding is where each parameter's value comes from: a cell of the call,
// or the parameter's default.
type binding struct {
	raw   string
	isDef bool
	given bool
}

// bind matches a call's cells to parameters: positional first, then
// name=value. A cell written name=value is always a named argument, and one
// for a name the keyword lacks is refused: write name\=value for the text.
func bind(p Pos, kw string, names []string, defaults []*string, cells []string, expanded bool) ([]binding, error) {
	out := make([]binding, len(names))
	idx := map[string]int{}
	for i, n := range names {
		idx[norm(n)] = i
	}
	pos := 0
	named := false
	for _, raw := range cells {
		if m := namedArg.FindStringSubmatch(raw); m != nil {
			i, ok := idx[norm(m[1])]
			if !ok {
				return nil, errAt(p, "%s has no argument %q; its arguments are %s. For text with =, write \\=",
					kw, m[1], argList(names))
			}
			if out[i].given {
				return nil, errAt(p, "%s: argument %s is given twice", kw, names[i])
			}
			out[i] = binding{raw: raw[len(m[0]):], given: true}
			named = true
			continue
		}
		if named {
			return nil, errAt(p, "%s: %q comes after a named argument, and is not named", kw, raw)
		}
		if pos >= len(names) {
			if expanded {
				return nil, nil
			}
			return nil, errAt(p, "%s takes at most %d arguments (%s), and has more", kw, len(names), argList(names))
		}
		out[pos] = binding{raw: raw, given: true}
		pos++
	}
	for i := range out {
		if out[i].given {
			continue
		}
		if defaults[i] == nil {
			if expanded {
				return nil, nil
			}
			return nil, errAt(p, "%s needs its argument %s", kw, names[i])
		}
		out[i] = binding{raw: *defaults[i], isDef: true}
	}
	return out, nil
}

func argList(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

func paramNames(ps []Param) ([]string, []*string) {
	n := make([]string, len(ps))
	d := make([]*string, len(ps))
	for i, p := range ps {
		n[i], d[i] = p.Name, p.Default
	}
	return n, d
}

// call checks one keyword call and says what it returns.
func (c *checker) call(s *Call, sc *scope, strict bool) (Static, error) {
	if msg, refused := refusal(s.Name); refused {
		return Static{}, errAt(s.Pos, "%s", msg)
	}
	if strings.Contains(s.Name, ".") {
		return Static{}, errAt(s.Pos, "%s: library prefixes are not in the subset; name the keyword alone", s.Name)
	}
	expanded := false
	for _, raw := range s.Args {
		cl, err := c.s.cell(s.Pos, raw)
		if err != nil && !c.isConditionCell(s, raw) {
			return Static{}, err
		}
		if cl != nil && cl.list {
			expanded = true
		}
	}

	if k, ok := c.s.kws[norm(s.Name)]; ok {
		return c.userCall(s, k, sc, strict, expanded)
	}
	k, ok := c.s.Lib.Keyword(s.Name)
	if !ok {
		return Static{}, errAt(s.Pos, "no keyword named %q", s.Name)
	}
	names, defs := paramNames(k.Params)
	b, err := bind(s.Pos, k.Name, names, defs, s.Args, expanded)
	if err != nil || b == nil {
		return Static{}, err
	}
	args := map[string]Static{}
	for i, prm := range k.Params {
		st, err := c.param(s.Pos, k, prm, b[i], sc, strict)
		if err != nil {
			return Static{}, err
		}
		args[prm.Name] = st
	}
	if k.CheckArgs != nil {
		if err := k.CheckArgs(args); err != nil {
			return Static{}, errAt(s.Pos, "%s: %v", k.Name, err)
		}
	}
	if k.ReturnOf != nil {
		return k.ReturnOf(args), nil
	}
	if k.Returns == VNumber {
		return StaticNumber(k.ReturnUnit), nil
	}
	return Static{Kind: k.Returns}, nil
}

// isConditionCell says whether raw is passed to a condition parameter, which
// is an expression rather than a cell to substitute into.
func (c *checker) isConditionCell(s *Call, raw string) bool {
	k, ok := c.s.Lib.Keyword(s.Name)
	if !ok {
		return false
	}
	names, defs := paramNames(k.Params)
	b, err := bind(s.Pos, k.Name, names, defs, s.Args, true)
	if err != nil || b == nil {
		return false
	}
	for i, p := range k.Params {
		if p.Kind == PCondition && b[i].raw == raw {
			return true
		}
	}
	return false
}

// param checks one argument against its parameter.
func (c *checker) param(p Pos, k *Keyword, prm Param, b binding, sc *scope, strict bool) (Static, error) {
	if prm.Kind == PCondition {
		return Static{Kind: VString}, c.condition(p, b.raw, sc, strict)
	}
	cl, err := c.s.cell(p, b.raw)
	if err != nil {
		return Static{}, err
	}
	st, err := c.cellStatic(p, cl, sc)
	if err != nil {
		return st, err
	}
	if !st.Known {
		// What kind it is may still be wrong.
		if prm.Kind == PQuantity && st.Typed && (st.T.Kind != expr.Number || !st.T.Unit.SameDimension(prm.Unit)) {
			return st, errAt(p, "%s: %s takes %s, and this is %s", k.Name, prm.Name, want(prm), st.T)
		}
		return st, nil
	}
	v, err := convert(prm, st.Val)
	if err != nil {
		return st, errAt(p, "%s: %s: %v", k.Name, prm.Name, err)
	}
	return staticOf(v), nil
}

func want(prm Param) string {
	if prm.Unit.Dimensionless() {
		return "a number"
	}
	if prm.Unit.SameDimension(expr.ParseUnit("s")) {
		return "a duration, such as 500ms"
	}
	return "a number in " + prm.Unit.Name
}

// convert turns a value into what a parameter takes. It is used at load on
// what is known then, and when the call is made on everything.
func convert(prm Param, v Value) (Value, error) {
	// ${None} is an argument not given: an optional parameter whose
	// default is ${None} receives it as it is, and the keyword says what
	// leaving it out means.
	if v.Kind == VNone && prm.Kind != PText {
		return v, nil
	}
	switch prm.Kind {
	case PAny, PCondition:
		return v, nil
	case PText:
		s := v.String()
		if v.Kind == VNone {
			s = ""
		}
		if prm.Check != nil {
			if err := prm.Check(s); err != nil {
				return v, err
			}
		}
		return Str(s), nil
	case PBool:
		switch {
		case v.Kind == VBool:
			return v, nil
		case v.Kind == VString && (v.Str == "True" || v.Str == "False"):
			return BoolV(v.Str == "True"), nil
		}
		return v, fmt.Errorf("%s is not True or False", v.String())
	case PQuantity:
		n := v
		if v.Kind == VString {
			f, u, err := expr.ParseQuantity(v.Str)
			if err != nil {
				return v, fmt.Errorf("%q is not %s", v.Str, want(prm))
			}
			n = Number(f, u)
		}
		if n.Kind != VNumber {
			return v, fmt.Errorf("%s is not %s", v.String(), want(prm))
		}
		if n.Unit.Scale == 0 {
			n.Unit = expr.None
		}
		if !n.Unit.SameDimension(prm.Unit) {
			if n.Unit.Dimensionless() && n.Unit.Name == "" {
				return v, fmt.Errorf("%s needs a unit: it takes %s", v.String(), want(prm))
			}
			return v, fmt.Errorf("%s is in %s, and this takes %s", v.String(), n.Unit, want(prm))
		}
		if math.IsNaN(n.Num) || math.IsInf(n.Num, 0) {
			return v, fmt.Errorf("%s is not a number", v.String())
		}
		return n, nil
	}
	return v, nil
}

// userCall checks a call to a user keyword, by checking the keyword's body
// with what is known of this call's arguments. A keyword called with
// different kinds of argument is checked once for each.
func (c *checker) userCall(s *Call, k *Block, sc *scope, strict, expanded bool) (Static, error) {
	for i, b := range c.stack {
		if b == k {
			var path []string
			for _, x := range c.stack[i:] {
				path = append(path, x.Name)
			}
			path = append(path, k.Name)
			return Static{}, errAt(s.Pos, "keyword %q calls itself, through %s; procedures do not recurse",
				k.Name, strings.Join(path, " → "))
		}
	}
	names := make([]string, len(k.Args))
	defs := make([]*string, len(k.Args))
	for i, a := range k.Args {
		names[i], defs[i] = strings.TrimSuffix(strings.TrimPrefix(a.Written, "${"), "}"), a.Default
	}
	b, err := bind(s.Pos, k.Name, names, defs, s.Args, expanded)
	if err != nil {
		return Static{}, err
	}
	args := map[string]Static{}
	sig := []string{fmt.Sprintf("%p", k)}
	for i, a := range k.Args {
		if b == nil {
			args[a.Name] = Static{}
			continue
		}
		if b[i].isDef {
			sig = append(sig, "default")
			continue // the keyword evaluates its own defaults
		}
		cl, err := c.s.cell(s.Pos, b[i].raw)
		if err != nil {
			return Static{}, err
		}
		st, err := c.cellStatic(s.Pos, cl, sc)
		if err != nil {
			return Static{}, err
		}
		args[a.Name] = st
		sig = append(sig, st.fingerprint())
	}
	key := strings.Join(sig, "\x00")
	if strict {
		if r, ok := c.memo[key]; ok {
			return r, nil
		}
	}
	c.stack = append(c.stack, k)
	c.via = append(c.via, s.Pos)
	r, err := c.block(k, args, strict)
	c.stack = c.stack[:len(c.stack)-1]
	c.via = c.via[:len(c.via)-1]
	if err != nil {
		var e *Error
		if errors.As(err, &e) && !strings.Contains(e.Msg, "(called from") && e.Pos.File+fmt.Sprint(e.Pos.Line) != s.Pos.File+fmt.Sprint(s.Pos.Line) {
			e.Msg += fmt.Sprintf(" (called from %s)", s.Pos)
		}
		return Static{}, err
	}
	if strict {
		c.memo[key] = r
	}
	return r, nil
}

// condResolver types the names a condition reads: $variables as known at
// load, and channels through the environment.
type condResolver struct {
	c       *checker
	sc      *scope
	unknown bool // a variable whose type is not known at load was read
}

func (r *condResolver) Resolve(name string) (expr.Type, error) {
	if strings.HasPrefix(name, "$") {
		rf, _ := parseRef(Pos{}, name, name[1:])
		st, ok := r.c.lookup(r.sc, rf)
		if !ok {
			return expr.Type{}, fmt.Errorf("${%s} is not defined here", rf.name)
		}
		if len(rf.fields) > 0 && st.Known {
			v, err := field(st.Val, rf)
			if err != nil {
				return expr.Type{}, err
			}
			st = staticOf(v)
		}
		if !st.Typed {
			r.unknown = true
			return expr.Type{}, fmt.Errorf("what ${%s} holds is not known when the procedure loads, so this condition cannot be checked; "+
				"give it a value written in the procedure, or one from a keyword that says what it returns", rf.name)
		}
		return st.T, nil
	}
	if r.c.s.env.Channels == nil {
		return expr.Type{}, fmt.Errorf("%s is not a known name; a variable is written $name or ${name}", name)
	}
	return r.c.s.env.Channels.Resolve(name)
}

// condition compiles a condition at load. Where it reads a variable whose type
// waits for the keyword's caller, it is checked at each call instead.
func (c *checker) condition(p Pos, src string, sc *scope, strict bool) error {
	r := &condResolver{c: c, sc: sc}
	_, err := expr.Compile(src, expr.Condition, r)
	if err == nil {
		return nil
	}
	if r.unknown && !strict {
		return nil
	}
	return errAt(p, "condition %q: %v", src, err)
}

func (c *checker) staticCell(p Pos, raw string, sc *scope) (Static, error) {
	cl, err := c.s.cell(p, raw)
	if err != nil {
		return Static{}, err
	}
	return c.cellStatic(p, cl, sc)
}

// duration checks a duration cell, and gives its value when it is known.
func (c *checker) duration(p Pos, what, raw string, sc *scope) (time.Duration, error) {
	st, err := c.staticCell(p, raw, sc)
	if err != nil || !st.Known {
		return 0, err
	}
	v, err := convert(Duration(what), st.Val)
	if err != nil {
		return 0, errAt(p, "%s: %v", what, err)
	}
	if v.Num <= 0 {
		return 0, errAt(p, "%s: %s is not longer than zero", what, raw)
	}
	return time.Duration(v.Num * 1e9), nil
}

func (c *checker) number(p Pos, what, raw string, sc *scope) (float64, error) {
	st, err := c.staticCell(p, raw, sc)
	if err != nil || !st.Known {
		return 0, err
	}
	v, err := convert(Quantity(what, ""), st.Val)
	if err != nil {
		return 0, errAt(p, "%s: %v", what, err)
	}
	return v.Num, nil
}

// whileLimit checks limit=: a count, or a duration.
func (c *checker) whileLimit(s *While, sc *scope) (Value, error) {
	st, err := c.staticCell(s.Pos, s.Limit, sc)
	if err != nil {
		return Value{}, err
	}
	if !st.Known {
		return Value{}, nil
	}
	return parseLimit(s.Pos, st.Val)
}

// parseLimit reads a WHILE limit: a whole number of iterations, or a
// duration.
func parseLimit(p Pos, v Value) (Value, error) {
	if n, err := strconv.Atoi(strings.TrimSpace(v.String())); err == nil {
		if n <= 0 {
			return v, errAt(p, "WHILE limit=%d: the limit is more than zero", n)
		}
		return Number(float64(n), expr.None), nil
	}
	d, err := convert(Duration("limit"), v)
	if err != nil || d.Num <= 0 {
		return v, errAt(p, "WHILE limit=%s: the limit is a whole number of iterations or a duration, such as 30s", v.String())
	}
	return d, nil
}
