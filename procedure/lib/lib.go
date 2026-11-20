// Package lib is the keyword library procedures run with: what a procedure
// can ask of a rack. Algorithms live here, in Go, as keywords; a new need
// becomes a new keyword or a new parameter with a default that keeps old
// procedures meaning what they meant, never new syntax (handoff decision 8).
//
// The library holds no policy. What a keyword asks of an instrument goes
// through the daemon's control plane like any client's request — leased,
// armed and limited — and is recorded there.
package lib

import (
	"context"
	"fmt"
	"time"

	"github.com/trukeio/ktest/expr"
	"github.com/trukeio/ktest/procedure"
)

// Version is the library's version, recorded with every run. It changes
// whenever a keyword's meaning does, so that a recording says which meaning
// its steps had.
const Version = "ktest-lib 0.1.0"

// Rack is what the keywords need of the daemon.
type Rack interface {
	// Resolve types a channel a procedure reads, as conditions do.
	Resolve(name string) (expr.Type, error)

	// Stressor says, with a sentence when it is not, whether target is
	// something Stress Apply drives: a supply channel, psu1.ch1.
	Stressor(target string) error

	// Set writes one setting, psu1.ch1.v.set, through the control plane, and
	// returns once the instrument has answered. A refusal is a
	// *procedure.Failure with the outcome refused.
	Set(ctx context.Context, channel string, value float64) error

	// Result records one step result of a procedure.
	Result(r StepResult)
}

// StepResult is what Record Step records.
type StepResult struct {
	Point     string
	Level     procedure.Value
	Result    string
	Threshold *procedure.Value
}

// New makes the library: the subset's built-ins and the rack's keywords.
func New(r Rack) *procedure.Library {
	l := procedure.NewLibrary(Version)
	channel := procedure.Checked(procedure.Text("channel"), func(s string) error {
		_, err := r.Resolve(s)
		return err
	})
	target := procedure.Checked(procedure.Text("target"), r.Stressor)

	l.Add(&procedure.Keyword{
		Name:   "Settle",
		Doc:    "Waits a fixed time: Settle    500ms.",
		Params: []procedure.Param{procedure.Duration("duration")},
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			return procedure.Value{}, c.Sleep(seconds(a["duration"]))
		},
	})

	l.Add(&procedure.Keyword{
		Name: "Wait Until",
		Doc: "Waits until a condition holds, checking it every poll, and fails with timeout if it has not " +
			"held within timeout: Wait Until    psu1.ch1.meas.v > 11.5V    timeout=2s.",
		Params: []procedure.Param{procedure.Cond("condition"),
			procedure.Opt(procedure.Duration("timeout"), "10s"), procedure.Opt(procedure.Duration("poll"), "10ms")},
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			cond := c.Condition("condition")
			deadline := time.Now().Add(seconds(a["timeout"]))
			for {
				ok, err := cond.Eval()
				// A channel with no value yet is waited for, like a false
				// condition: it may be about to arrive.
				if err == nil && ok {
					return procedure.Value{}, nil
				}
				if time.Now().After(deadline) {
					if err != nil {
						return procedure.Value{}, procedure.Failf(procedure.Timeout, "%v, after %v", err, seconds(a["timeout"]))
					}
					return procedure.Value{}, procedure.Failf(procedure.Timeout, "'%s' did not hold within %v",
						cond.Source(), seconds(a["timeout"]))
				}
				if err := c.Sleep(seconds(a["poll"])); err != nil {
					return procedure.Value{}, err
				}
			}
		},
	})

	l.Add(&procedure.Keyword{
		Name:   "Read Channel",
		Doc:    "Returns a channel's latest value: ${v}=    Read Channel    psu1.ch1.meas.v.",
		Params: []procedure.Param{channel},
		ReturnOf: func(a map[string]procedure.Static) procedure.Static {
			ch := a["channel"]
			if !ch.Known {
				return procedure.Static{}
			}
			t, err := r.Resolve(ch.Val.Str)
			if err != nil {
				return procedure.Static{}
			}
			switch t.Kind {
			case expr.Number:
				return procedure.StaticNumber(t.Unit)
			case expr.Bool:
				return procedure.Static{Kind: procedure.VBool, Typed: true, T: t}
			}
			return procedure.Static{Kind: procedure.VString, Typed: true, T: t}
		},
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			name := a["channel"].Str
			t, err := r.Resolve(name)
			if err != nil {
				return procedure.Value{}, procedure.Failf(procedure.Fail, "%v", err)
			}
			v, _, ok := c.Host.Value(name)
			if !ok || !v.Known {
				return procedure.Value{}, procedure.Failf(procedure.Fail, "%s has no value yet", name)
			}
			switch t.Kind {
			case expr.Number:
				return procedure.Number(v.Num*t.Unit.Scale, t.Unit), nil
			case expr.Bool:
				return procedure.BoolV(v.Bool), nil
			}
			return procedure.Str(v.Str), nil
		},
	})

	l.Add(&procedure.Keyword{
		Name: "Should Be Within",
		Doc: "Fails unless actual is within tolerance of expected, all three in one unit: " +
			"Should Be Within    ${v}    12V    0.2V.",
		Params: []procedure.Param{procedure.AnyP("actual"), procedure.AnyP("expected"), procedure.AnyP("tolerance")},
		CheckArgs: func(a map[string]procedure.Static) error {
			var first expr.Unit
			var name string
			for _, p := range []string{"actual", "expected", "tolerance"} {
				st := a[p]
				var u expr.Unit
				switch {
				case st.Known:
					q := quantity(st.Val)
					if q.Kind != procedure.VNumber {
						return fmt.Errorf("%s: %s is not a number", p, st.Val.String())
					}
					u = q.Unit
				case st.Typed && st.T.Kind == expr.Number:
					u = st.T.Unit
				case st.Typed:
					return fmt.Errorf("%s is %s, not a number", p, st.T)
				default:
					continue // known when the call is made
				}
				if name == "" {
					first, name = u, p
				} else if !u.SameDimension(first) {
					return fmt.Errorf("%s is in %s and %s in %s; all three are in one unit", name, first, p, u)
				}
			}
			return nil
		},
		Run: func(_ *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			act, exp, tol := quantity(a["actual"]), quantity(a["expected"]), quantity(a["tolerance"])
			for _, q := range []procedure.Value{act, exp, tol} {
				if q.Kind != procedure.VNumber {
					return procedure.Value{}, procedure.Failf(procedure.Fail, "%s is not a number", q.String())
				}
			}
			if !act.Unit.SameDimension(exp.Unit) || !act.Unit.SameDimension(tol.Unit) {
				return procedure.Value{}, procedure.Failf(procedure.Fail, "%s, %s and %s are not in one unit",
					act.String(), exp.String(), tol.String())
			}
			if d := act.Num - exp.Num; d > tol.Num || -d > tol.Num {
				return procedure.Value{}, procedure.Failf(procedure.Fail, "%s is not within %s of %s",
					act.String(), tol.String(), exp.String())
			}
			return procedure.Value{}, nil
		},
	})

	l.Add(&procedure.Keyword{
		Name: "Record Step",
		Doc: "Records a step result in procedure.result: the point of the test, the stress level, " +
			"the result, and a threshold where one was found.",
		Params: []procedure.Param{procedure.Text("point"), procedure.AnyP("level"), procedure.Text("result"),
			procedure.Opt(procedure.AnyP("threshold"), "${None}")},
		Run: func(_ *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			sr := StepResult{Point: a["point"].Str, Level: quantity(a["level"]), Result: a["result"].Str}
			if th := a["threshold"]; th.Kind != procedure.VNone {
				q := quantity(th)
				sr.Threshold = &q
			}
			r.Result(sr)
			return procedure.Value{}, nil
		},
	})

	l.Add(&procedure.Keyword{
		Name: "Stress Apply",
		Doc: "Applies a stress level with a lab instrument: sets the supply channel's voltage, and its current " +
			"limit when current= is given, then switches its output on. Stress Apply    psu1.ch1    13.5V    current=1A. " +
			"Without current= the limit is left as the supply holds it. The instrument is leased and armed by the run.",
		Params: []procedure.Param{target, procedure.Quantity("level", "V"),
			procedure.Opt(procedure.Quantity("current", "A"), "${None}")},
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			t := a["target"].Str
			if i := a["current"]; i.Kind == procedure.VNumber {
				if err := r.Set(c, t+".i.set", i.Num); err != nil {
					return procedure.Value{}, err
				}
			}
			if err := r.Set(c, t+".v.set", a["level"].Num); err != nil {
				return procedure.Value{}, err
			}
			return procedure.Value{}, r.Set(c, t+".out.set", 1)
		},
	})

	l.Add(&procedure.Keyword{
		Name:   "Stress Remove",
		Doc:    "Removes the stress: switches the supply channel's output off. Stress Remove    psu1.ch1.",
		Params: []procedure.Param{target},
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			return procedure.Value{}, r.Set(c, a["target"].Str+".out.set", 0)
		},
	})
	return l
}

func seconds(v procedure.Value) time.Duration { return time.Duration(v.Num * 1e9) }

// quantity reads a level written as text, 12V, as the number it is; anything
// else stays as it is.
func quantity(v procedure.Value) procedure.Value {
	if v.Kind != procedure.VString {
		return v
	}
	f, u, err := expr.ParseQuantity(v.Str)
	if err != nil {
		return v
	}
	return procedure.Number(f, u)
}

// Describe lists the keywords, for a person reading what a library offers.
func Describe(l *procedure.Library) string {
	out := ""
	for _, n := range l.Names() {
		k, _ := l.Keyword(n)
		out += fmt.Sprintf("%s\n    %s\n", k.Name, k.Doc)
	}
	return out
}
