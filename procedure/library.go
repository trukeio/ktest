package procedure

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/trukeio/ktest/expr"
)

// Outcome is how a keyword call ended. ok and nok are values: a keyword that
// judged something answers with one, and the procedure goes on. Every other
// outcome fails the keyword, stops the test and runs its [Teardown].
type Outcome string

const (
	OK       Outcome = "ok"
	NOK      Outcome = "nok"
	Timeout  Outcome = "timeout"   // a wait or a [Timeout] ran out
	Refused  Outcome = "refused"   // the daemon refused a request
	LinkLost Outcome = "link_lost" // the peer stopped answering
	Fail     Outcome = "fail"      // Fail, a failed check, or a value that could not be used
	Stopped  Outcome = "stopped"   // DELETE /procedures/run, or the daemon stopping
)

// Outcomes is every outcome, in the order the recording numbers them.
var Outcomes = []Outcome{OK, NOK, Timeout, Refused, LinkLost, Fail, Stopped}

// Failure is a keyword failing, with its outcome.
type Failure struct {
	Outcome Outcome
	Msg     string
}

func (f *Failure) Error() string { return string(f.Outcome) + ": " + f.Msg }

// Failf makes a Failure.
func Failf(o Outcome, format string, a ...any) error {
	return &Failure{Outcome: o, Msg: fmt.Sprintf(format, a...)}
}

// message is an error as a step's or a run's message: a sentence, for the
// context's errors too.
func message(err error) string {
	var f *Failure
	switch {
	case errors.As(err, &f):
		return f.Msg
	case errors.Is(err, context.Canceled):
		return "the run was stopped"
	case errors.Is(err, context.DeadlineExceeded):
		return "its [Timeout] ran out"
	}
	return err.Error()
}

// outcomeOf is the outcome an error stands for.
func outcomeOf(err error, ctx context.Context) Outcome {
	var f *Failure
	switch {
	case err == nil:
		return OK
	case errors.As(err, &f):
		return f.Outcome
	case errors.Is(err, context.DeadlineExceeded):
		return Timeout
	case errors.Is(err, context.Canceled):
		return Stopped
	}
	return Fail
}

// PKind is what a parameter takes.
type PKind uint8

const (
	PAny       PKind = iota // any value, as it is
	PText                   // text; Check, if set, validates it
	PQuantity               // a number in Unit's dimension: "12V", "500mV"
	PBool                   // True or False
	PCondition              // an expression of the condition profile, compiled at load
)

// Param is one parameter of a library keyword.
type Param struct {
	Name    string
	Kind    PKind
	Unit    expr.Unit // PQuantity: the dimension it takes; expr.None for a plain number
	Default *string   // nil when the argument is required
	// Check validates a PText argument; an error is a sentence.
	Check func(string) error
}

// Text, Quantity, Duration, Cond, AnyP make parameters.
func Text(name string) Param { return Param{Name: name, Kind: PText} }
func Quantity(name, unit string) Param {
	return Param{Name: name, Kind: PQuantity, Unit: expr.ParseUnit(unit)}
}
func Duration(name string) Param { return Quantity(name, "s") }
func Cond(name string) Param     { return Param{Name: name, Kind: PCondition} }
func AnyP(name string) Param     { return Param{Name: name, Kind: PAny} }

// Opt gives a parameter a default, which makes it optional.
func Opt(p Param, def string) Param { p.Default = &def; return p }

// Checked gives a text parameter a validator.
func Checked(p Param, check func(string) error) Param { p.Check = check; return p }

// Keyword is a library keyword: Go code with a declared signature, so that a
// call to it is checked when the procedure loads.
type Keyword struct {
	Name   string
	Doc    string
	Params []Param

	// Returns is the kind of what the keyword returns, VNone for nothing.
	// ReturnUnit is a number's unit.
	Returns    VKind
	ReturnUnit expr.Unit
	// ReturnOf, if set, says what a call returns from what its arguments
	// are known to be at load, for a keyword whose result takes its type from
	// them (Set Variable, Read Channel).
	ReturnOf func(args map[string]Static) Static
	// CheckArgs, if set, checks at load what is known of the arguments
	// together, for a keyword whose arguments must agree with each other,
	// such as being in one unit. An error is a sentence.
	CheckArgs func(args map[string]Static) error

	Run func(c *Ctx, args Args) (Value, error)
}

// Args are a call's arguments, converted to what the parameters take.
type Args map[string]Value

// Ctx is what a keyword runs with.
type Ctx struct {
	context.Context
	Host Host
	// conds are the call's compiled conditions, by parameter name.
	conds map[string]*Condition
	run   *runner
}

// Condition is the compiled condition a PCondition parameter received.
func (c *Ctx) Condition(param string) *Condition { return c.conds[param] }

// Sleep waits d, or until the run is stopped.
func (c *Ctx) Sleep(d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-c.Done():
		return c.Err()
	}
}

// Host is what a procedure runs against: the daemon, or a test's stand-in.
type Host interface {
	// Resolve types a channel name for a condition; an error is a sentence.
	Resolve(name string) (expr.Type, error)
	// Value is a channel's latest value, in the unit Resolve gave it, and when
	// it was taken, in nanoseconds; ok is false while there is none.
	Value(name string) (v expr.Value, t int64, ok bool)
	// Step is told when each keyword call starts and ends.
	Step(StepEvent)
}

// StepEvent is one keyword call starting or ending.
type StepEvent struct {
	ID      uint32 // from 1, in the order calls start
	Parent  uint32 // the call this one is inside, 0 for a test's own step
	End     bool
	Pos     Pos
	Keyword string
	Args    []string // as resolved
	Outcome Outcome  // End only
	Msg     string   // End only: why it failed, or what it returned
	Start   time.Time
	Stop    time.Time // End only
}

// Library is a set of keywords, by Robot's normalised name.
type Library struct {
	Version string
	kws     map[string]*Keyword
}

// NewLibrary makes a library with the built-in keywords of the subset.
func NewLibrary(version string) *Library {
	l := &Library{Version: version, kws: map[string]*Keyword{}}
	for _, k := range builtins() {
		l.Add(k)
	}
	return l
}

// Add adds a keyword. Two keywords with one name is a programming error.
func (l *Library) Add(k *Keyword) {
	n := norm(k.Name)
	if _, dup := l.kws[n]; dup {
		panic("procedure: keyword " + k.Name + " added twice")
	}
	l.kws[n] = k
}

// Keyword finds a keyword by name.
func (l *Library) Keyword(name string) (*Keyword, bool) {
	k, ok := l.kws[norm(name)]
	return k, ok
}

// Names are the keywords' names, sorted.
func (l *Library) Names() []string {
	var out []string
	for _, k := range l.kws {
		out = append(out, k.Name)
	}
	sort.Strings(out)
	return out
}

// refusedKeywords are Robot BuiltIn keywords the subset leaves out, with why.
// They are named so that a procedure written for Robot hears why, rather
// than that the keyword does not exist.
var refusedKeywords = map[string]string{
	"evaluate":                  "Evaluate is not in the subset: a condition is written in IF, WHILE or Should Be True",
	"settestvariable":           "Set Test Variable is not in the subset: variables are local; pass values as arguments",
	"setsuitevariable":          "Set Suite Variable is not in the subset: variables are local; pass values as arguments",
	"setglobalvariable":         "Set Global Variable is not in the subset: variables are local; pass values as arguments",
	"setlocalvariable":          "Set Local Variable is not in the subset; write ${x}=    Set Variable    value",
	"settaskvariable":           "Set Task Variable is not in the subset: variables are local; pass values as arguments",
	"importlibrary":             "Import Library is not in the subset: the keywords are Ktest's own",
	"importresource":            "Import Resource is not in the subset; use Resource in the Settings",
	"importvariables":           "Import Variables is not in the subset; use Variables in the Settings",
	"callmethod":                "Call Method is not in the subset",
	"runkeyword":                "Run Keyword is not in the subset: every call is written out, so that it is checked at load",
	"runkeywordif":              "Run Keyword If is not in the subset; use IF",
	"runkeywords":               "Run Keywords is not in the subset: write each call on its own line",
	"runkeywordandignoreerror":  "Run Keyword And Ignore Error is not in the subset: a failure fails the test",
	"runkeywordandreturnstatus": "Run Keyword And Return Status is not in the subset: a failure fails the test",
	"waituntilkeywordsucceeds":  "Wait Until Keyword Succeeds is not in the subset; use Wait Until with a condition",
	"repeatkeyword":             "Repeat Keyword is not in the subset; use FOR",
	"sleep":                     "Sleep is Settle here: Settle    500ms",
}

// refusal names a keyword the subset refuses, if name is one.
func refusal(name string) (string, bool) {
	n := norm(name)
	if msg, ok := refusedKeywords[n]; ok {
		return msg, true
	}
	if strings.HasPrefix(n, "runkeyword") {
		return name + " is not in the subset: every call is written out, so that it is checked at load", true
	}
	return "", false
}
