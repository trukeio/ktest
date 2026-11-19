package expr

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// A Unit is what a number is measured in. Values are carried in SI base units
// whatever unit they were written or recorded in, so that 500mV and 0.5V are
// the same number once compiled; Scale is what one of this unit is in SI, and
// it matters only where a number is converted on its way in.
//
// Dimensions are checked, magnitudes are not: adding a voltage to a current is
// a load error, and adding 1mV to 1V is 1.001V.
type Unit struct {
	Name  string  // as written or as the schema states it: "mV", "rpm"; "" for none
	Scale float64 // one of this unit, in SI

	// dim is the dimension in canonical form, "V A^-1" for ohms. A unit this
	// package does not know, "rpm" or "B", is a dimension of its own, so that
	// two channels in rpm compare and one in rpm and one in V do not.
	dim string
}

// None is a dimensionless number.
var None = Unit{Scale: 1}

// Dimensionless reports whether u has no dimension.
func (u Unit) Dimensionless() bool { return u.dim == "" }

// SameDimension reports whether a quantity in u may be compared with or added
// to one in v.
func (u Unit) SameDimension(v Unit) bool { return u.dim == v.dim }

func (u Unit) String() string {
	if u.Name == "" {
		return "no unit"
	}
	return u.Name
}

// base is a unit this package knows: its dimension and whether it takes an SI
// prefix. The derived units are written in the bases they are made of, so
// that V*A compares with W and 1/s with Hz.
type base struct {
	dim    map[string]int
	scale  float64
	prefix bool
}

var bases = map[string]base{
	"s":   {dim: map[string]int{"s": 1}, scale: 1, prefix: true},
	"V":   {dim: map[string]int{"V": 1}, scale: 1, prefix: true},
	"A":   {dim: map[string]int{"A": 1}, scale: 1, prefix: true},
	"W":   {dim: map[string]int{"V": 1, "A": 1}, scale: 1, prefix: true},
	"Ohm": {dim: map[string]int{"V": 1, "A": -1}, scale: 1, prefix: true},
	"Ω":   {dim: map[string]int{"V": 1, "A": -1}, scale: 1, prefix: true},
	"Hz":  {dim: map[string]int{"s": -1}, scale: 1, prefix: true},
	// A level in decibels and a temperature in Celsius are not ratios of
	// anything above, and are not scaled: 0 °C is not zero of anything. Each
	// is its own dimension, so that they compare only with their own kind.
	"dB":   {dim: map[string]int{"dB": 1}, scale: 1},
	"°C":   {dim: map[string]int{"°C": 1}, scale: 1},
	"degC": {dim: map[string]int{"°C": 1}, scale: 1},
	"%":    {dim: map[string]int{}, scale: 0.01},
}

var prefixes = map[rune]float64{
	'p': 1e-12, 'n': 1e-9, 'u': 1e-6, 'µ': 1e-6, 'm': 1e-3, 'k': 1e3, 'M': 1e6, 'G': 1e9,
}

// unitList is how an error sentence names the units a literal may carry.
const unitList = "V, A, W, Ohm, Hz, s, dB, °C and %, with an SI prefix where one makes sense"

// lookupUnit parses a unit this package knows, prefixed or not.
func lookupUnit(s string) (Unit, bool) {
	if b, ok := bases[s]; ok {
		return Unit{Name: s, Scale: b.scale, dim: dimString(b.dim)}, true
	}
	r := []rune(s)
	if len(r) < 2 {
		return Unit{}, false
	}
	p, ok := prefixes[r[0]]
	if !ok {
		return Unit{}, false
	}
	b, ok := bases[string(r[1:])]
	if !ok || !b.prefix {
		return Unit{}, false
	}
	return Unit{Name: s, Scale: p * b.scale, dim: dimString(b.dim)}, true
}

// ParseUnit reads the unit a schema gives a channel. A unit this package does
// not know is not an error, since a database may measure in anything: it
// becomes a dimension of its own, and a bare number beside it takes it.
func ParseUnit(s string) Unit {
	s = strings.TrimSpace(s)
	if s == "" {
		return None
	}
	if u, ok := lookupUnit(s); ok {
		return u
	}
	return Unit{Name: s, Scale: 1, dim: dimString(map[string]int{s: 1})}
}

// ParseQuantity reads a number with a unit this package knows, such as "20ms"
// or "1.5V", into SI. A configuration file's durations and thresholds are read
// with it, so that they are written the way expressions write them.
func ParseQuantity(s string) (float64, Unit, error) {
	toks, err := lex(s)
	if err != nil {
		return 0, Unit{}, err
	}
	neg := false
	if len(toks) > 0 && toks[0].kind == tOp && toks[0].text == "-" {
		neg, toks = true, toks[1:]
	}
	if len(toks) != 2 || toks[0].kind != tNum {
		return 0, Unit{}, fmt.Errorf("%q is not a number with a unit, such as 20ms", s)
	}
	v, u := toks[0].num, None
	if toks[0].unit != "" {
		var ok bool
		if u, ok = lookupUnit(toks[0].unit); !ok {
			return 0, Unit{}, fmt.Errorf("%q: unknown unit %q; units are %s", s, toks[0].unit, unitList)
		}
	}
	if neg {
		v = -v
	}
	return v * u.Scale, u, nil
}

// dimString is a dimension's canonical form: bases sorted, exponents other
// than 1 written out, zero exponents dropped.
func dimString(d map[string]int) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(d)) {
		switch e := d[k]; e {
		case 0:
		case 1:
			parts = append(parts, k)
		default:
			parts = append(parts, k+"^"+strconv.Itoa(e))
		}
	}
	return strings.Join(parts, " ")
}

func parseDim(s string) map[string]int {
	d := map[string]int{}
	if s == "" {
		return d
	}
	for _, p := range strings.Split(s, " ") {
		k, e, ok := strings.Cut(p, "^")
		n := 1
		if ok {
			n, _ = strconv.Atoi(e)
		}
		d[k] += n
	}
	return d
}

// product is the unit of a*b, or of a/b with sign -1. Its scale is 1: the
// values multiplied are already SI, and a product has no unit of its own for
// a bare number to take a scale from.
func product(a, b Unit, sign int) Unit {
	d := parseDim(a.dim)
	for k, e := range parseDim(b.dim) {
		d[k] += sign * e
	}
	op := "·"
	if sign < 0 {
		op = "/"
	}
	an, bn := a.Name, b.Name
	if an == "" {
		an = "1"
	}
	name := an + op + bn
	if bn == "" {
		name = a.Name
	}
	ds := dimString(d)
	if ds == "" {
		name = ""
	}
	return Unit{Name: name, Scale: 1, dim: ds}
}

var seconds = Unit{Name: "s", Scale: 1, dim: "s"}
