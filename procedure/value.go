package procedure

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/trukeio/ktest/expr"
)

// VKind is what sort of value a variable holds.
type VKind uint8

const (
	VNone VKind = iota
	VString
	VNumber
	VBool
	VList
	VDict
)

// Value is a variable's value. Cells are strings, as in Robot; a keyword
// argument is converted to what its parameter takes when the call is made, and
// a keyword's result is what the keyword says it is.
type Value struct {
	Kind VKind
	Str  string
	Num  float64   // in SI
	Unit expr.Unit // a number's unit, for showing it as it was written
	Bool bool
	List []Value
	Keys []string // a dict's keys, in order
	Dict map[string]Value
}

func Str(s string) Value                  { return Value{Kind: VString, Str: s} }
func Number(v float64, u expr.Unit) Value { return Value{Kind: VNumber, Num: v, Unit: u} }
func BoolV(b bool) Value                  { return Value{Kind: VBool, Bool: b} }

// String is the value as a cell would show it: a number in the unit it was
// written in.
func (v Value) String() string {
	switch v.Kind {
	case VString:
		return v.Str
	case VNumber:
		s := v.Unit.Scale
		if s == 0 {
			s = 1
		}
		return strconv.FormatFloat(v.Num/s, 'g', -1, 64) + v.Unit.Name
	case VBool:
		if v.Bool {
			return "True"
		}
		return "False"
	case VList:
		parts := make([]string, len(v.List))
		for i, x := range v.List {
			parts[i] = x.String()
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case VDict:
		parts := make([]string, len(v.Keys))
		for i, k := range v.Keys {
			parts[i] = k + "=" + v.Dict[k].String()
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return "None"
}

// literal reads a cell's text the way a condition reads a substituted value:
// a number with an optional unit, True or False, or else a string. Robot
// substitutes a variable's text into a condition before evaluating it; this
// is the same reading, without the substitution.
func literal(s string) Value {
	switch s {
	case "True":
		return BoolV(true)
	case "False":
		return BoolV(false)
	}
	if v, u, err := expr.ParseQuantity(s); err == nil {
		return Number(v, u)
	}
	return Str(s)
}

// exprType is the type a value has in a condition.
func exprType(v Value) (expr.Type, bool) {
	switch v.Kind {
	case VString:
		l := literal(v.Str)
		if l.Kind == VString {
			return expr.Type{Kind: expr.String}, true
		}
		return exprType(l)
	case VNumber:
		u := v.Unit
		if u.Scale == 0 {
			u = expr.None
		}
		return expr.Type{Kind: expr.Number, Unit: u}, true
	case VBool:
		return expr.Type{Kind: expr.Bool}, true
	}
	return expr.Type{}, false
}

// exprValue is a value as a condition's input, in the unit exprType gave it.
func exprValue(v Value) expr.Value {
	switch v.Kind {
	case VString:
		l := literal(v.Str)
		if l.Kind == VString {
			return expr.StrValue(v.Str)
		}
		return exprValue(l)
	case VNumber:
		s := v.Unit.Scale
		if s == 0 {
			s = 1
		}
		return expr.Num(v.Num / s)
	case VBool:
		return expr.BoolValue(v.Bool)
	}
	return expr.Unknown
}

// A cell, parsed once at load: literal text and variable references, which
// are resolved when the step runs.
type cell struct {
	raw   string
	parts []part
	// list is @{name} alone in the cell: it expands into several arguments.
	list bool
}

type part struct {
	text string // literal, escapes resolved
	ref  *ref   // or a variable
}

// ref is ${name} or ${name.field.field}, or a number written ${3}.
type ref struct {
	name   string // normalised
	fields []string
	num    *Value
}

// whole says whether the cell is exactly one variable, which keeps the
// value's type; anything else is text.
func (c *cell) whole() *ref {
	if len(c.parts) == 1 && c.parts[0].ref != nil {
		return c.parts[0].ref
	}
	return nil
}

// parseCell resolves escapes and finds the variables in a cell.
func parseCell(p Pos, raw string) (*cell, error) {
	c := &cell{raw: raw}
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			c.parts = append(c.parts, part{text: lit.String()})
			lit.Reset()
		}
	}
	rs := []rune(raw)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r == '\\' && i+1 < len(rs) {
			i++
			switch rs[i] {
			case 'n':
				lit.WriteRune('\n')
			case 't':
				lit.WriteRune('\t')
			default:
				lit.WriteRune(rs[i])
			}
			continue
		}
		if (r == '$' || r == '@' || r == '&') && i+1 < len(rs) && rs[i+1] == '{' {
			if i+2 < len(rs) && rs[i+2] == '{' {
				return nil, errAt(p, "%s: ${{...}}, inline Python, is not in the subset", raw)
			}
			end := strings.IndexRune(string(rs[i+2:]), '}')
			if end < 0 {
				return nil, errAt(p, "%s: %c{ is not closed with }", raw, r)
			}
			body := string(rs[i+2 : i+2+end])
			if strings.ContainsAny(body, "{$@&") {
				return nil, errAt(p, "%s: variables inside variable names are not in the subset", raw)
			}
			next := i + 2 + end + 1
			if next < len(rs) && rs[next] == '[' {
				return nil, errAt(p, "%s: item access ${x}[...] is not in the subset; use ${x.field}", raw)
			}
			if r == '&' {
				return nil, errAt(p, "%s: &{dict} expansion is not in the subset; pass ${dict}, or name the arguments", raw)
			}
			rf, err := parseRef(p, raw, body)
			if err != nil {
				return nil, err
			}
			if r == '@' {
				if i != 0 || next != len(rs) {
					return nil, errAt(p, "%s: @{list} expands into arguments, so it stands alone in its cell", raw)
				}
				c.list = true
			}
			flush()
			c.parts = append(c.parts, part{ref: rf})
			i = next - 1
			continue
		}
		lit.WriteRune(r)
	}
	flush()
	if len(c.parts) == 0 {
		c.parts = []part{{text: ""}}
	}
	return c, nil
}

func parseRef(p Pos, raw, body string) (*ref, error) {
	if body == "" {
		return nil, errAt(p, "%s: an empty variable name", raw)
	}
	// ${3}, ${-1.5}: Robot's number variables. A number with a unit is
	// written as text, 12V, and converted where a quantity is taken.
	if f, err := strconv.ParseFloat(strings.TrimSpace(body), 64); err == nil && !math.IsNaN(f) {
		v := Number(f, expr.None)
		return &ref{num: &v}, nil
	}
	parts := strings.Split(body, ".")
	for _, s := range parts {
		if strings.TrimSpace(s) == "" {
			return nil, errAt(p, "%s: a variable's field has no name", raw)
		}
	}
	r := &ref{name: norm(parts[0])}
	for _, s := range parts[1:] {
		r.fields = append(r.fields, strings.TrimSpace(s))
	}
	return r, nil
}

func (r *ref) String() string {
	if r.num != nil {
		return "${" + r.num.String() + "}"
	}
	return "${" + strings.Join(append([]string{r.name}, r.fields...), ".") + "}"
}

// builtinVars are Robot's built-in variables the subset keeps.
var builtinVars = map[string]Value{
	"empty": Str(""),
	"space": Str(" "),
	"true":  BoolV(true),
	"false": BoolV(false),
	"none":  {},
}

// field reads ${v.a.b}.
func field(v Value, r *ref) (Value, error) {
	for _, f := range r.fields {
		if v.Kind != VDict {
			return Value{}, fmt.Errorf("%s: %s is not a dictionary, so it has no field %s", r, v.String(), f)
		}
		x, ok := v.Dict[f]
		if !ok {
			return Value{}, fmt.Errorf("%s: no field %s", r, f)
		}
		v = x
	}
	return v, nil
}
