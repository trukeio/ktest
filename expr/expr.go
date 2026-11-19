// Package expr is the one expression language of Ktest: contract monitors,
// limit expressions and procedure conditions are all written in it (handoff
// decision 6), so that a condition means the same wherever it is written.
//
//	abs(Speed - 3000) <= 30
//	age(Alive) < 30ms and delta(Alive.counter, 16) == 1
//	$level >= 12V or dut.mon.ready == "ok"
//
// It is small on purpose. There is no assignment, no function a user can
// define, no side effect and nothing that loops, so an expression always ends
// and the same history always gives the same answer.
//
// Three things are decided when an expression is compiled, never when it runs:
// every name it reads, the dimension of every number, and what its profile
// allows. A monitor that compares a voltage with a time is refused when the
// contract loads, not found deviating at three in the morning.
//
// Numbers are carried in SI. A number written with a unit is converted on the
// way in; a number written bare takes the unit of what it is compared with or
// added to, so that abs(Speed - 3000) <= 30 reads in the rpm the database
// gives Speed.
//
// Time is the samples' time. Nothing here reads a clock: a program is told
// when each sample was taken, and what time it is when it is asked. Replaying
// a recording therefore gives the transitions the live run gave, to the
// nanosecond.
package expr

import (
	"fmt"
	"math"
)

// Profile is what a use of the language may have.
type Profile uint8

const (
	// Monitor is a contract monitor: time functions and arithmetic.
	Monitor Profile = iota + 1
	// Limit is a limit expression; the same as Monitor, kept apart so that
	// either may be narrowed later without the other.
	Limit
	// Condition is a procedure's IF or WHILE: comparisons and logic only. A
	// value that needs computing is computed by a keyword, where it is
	// recorded as a step.
	Condition
)

func (p Profile) String() string {
	switch p {
	case Monitor:
		return "monitor"
	case Limit:
		return "limit"
	case Condition:
		return "condition"
	}
	return fmt.Sprintf("profile(%d)", uint8(p))
}

// Kind is what sort of value something is.
type Kind uint8

const (
	Number Kind = iota + 1
	Bool
	String
	Enum
)

func (k Kind) String() string {
	switch k {
	case Number:
		return "a number"
	case Bool:
		return "true or false"
	case String:
		return "a string"
	case Enum:
		return "an enumeration"
	}
	return "nothing"
}

// EnumType is an ordered enumeration declared in a contract. < and > compare
// by the order of Values.
type EnumType struct {
	Name   string
	Values []string
}

func (e *EnumType) index(s string) (int, bool) {
	for i, v := range e.Values {
		if v == s {
			return i, true
		}
	}
	return 0, false
}

// Type is the type of a name or of an expression.
type Type struct {
	Kind Kind
	Unit Unit      // Number only
	Enum *EnumType // Enum only
}

// NumberIn is the type of a number recorded in the unit a schema states.
func NumberIn(unit string) Type { return Type{Kind: Number, Unit: ParseUnit(unit)} }

func (t Type) String() string {
	switch t.Kind {
	case Number:
		if t.Unit.Name == "" {
			return "a number"
		}
		return "a number in " + t.Unit.Name
	case Enum:
		return "a value of " + t.Enum.Name
	}
	return t.Kind.String()
}

// Resolver says what a name is. Variables are asked for with their dollar,
// "$level", so that a procedure variable and a channel may share a name.
// The error, if any, is a sentence; the compiler puts the column in front.
type Resolver interface {
	Resolve(name string) (Type, error)
}

// Names is a Resolver over a fixed table.
type Names map[string]Type

func (n Names) Resolve(name string) (Type, error) {
	t, ok := n[name]
	if !ok {
		return Type{}, fmt.Errorf("%s is not a known name", name)
	}
	return t, nil
}

// Value is a value, or the admission that there is none. A channel nobody has
// heard from, a window that lost samples or a division by zero is not known,
// and logic over it follows Kleene: false and unknown is false, true or
// unknown is true, anything else with unknown is unknown.
type Value struct {
	Known bool
	Num   float64 // a Number, in the input's own unit on Observe and in SI from Eval
	Bool  bool
	Str   string // a String, or an Enum's value on Observe
}

// Unknown is the value nobody knows.
var Unknown = Value{}

func Num(v float64) Value     { return Value{Known: !math.IsNaN(v), Num: v} }
func BoolValue(b bool) Value  { return Value{Known: true, Bool: b} }
func StrValue(s string) Value { return Value{Known: true, Str: s} }

// MaxWindowSamples bounds what a windowed function (rate, mean) keeps per call
// site. A window that held more samples than this answers unknown until the
// samples it lost have left it: bounded state, and no answer made up from
// part of a window.
var MaxWindowSamples = 4096
