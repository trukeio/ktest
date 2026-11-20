package lib

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/trukeio/ktest/expr"
	"github.com/trukeio/ktest/procedure"
)

// Peer is what the master's DUT keywords need of the daemon: the DUT rack
// behind -peer, through the five operations of handoff §4.8. Each wait ends
// with the outcome the handoff gives it: an error made with procedure.Failf
// and Timeout, Refused or LinkLost, or Fail for an action that failed.
type Peer interface {
	// Monitor, Mode and Action check, at load, that the contract declares a
	// name, with a sentence when it does not.
	Monitor(name string) error
	Mode(name string) error
	Action(name string) error

	// SetMode changes the DUT's mode, and returns once the DUT rack has
	// applied it and its settle time has passed by the DUT rack's watermark.
	SetMode(ctx context.Context, mode string, timeout time.Duration) error
	// Invoke runs an action, and returns once its end has arrived.
	Invoke(ctx context.Context, action string, timeout time.Duration) error
	// Deviations waits until the DUT rack's watermark has passed t1, and
	// says which monitors were dev or unknown at any time in [t0, t1], with
	// what. t0 and t1 are this rack's clock; the window is widened by the
	// clock offset's uncertainty, so that a deviation at its edge is counted.
	Deviations(ctx context.Context, monitors []string, t0, t1 time.Time) (map[string]string, error)
	// AllOK waits until every monitor is ok at the DUT rack's watermark, and
	// says when, by this rack's clock; ok is false if it did not happen by
	// deadline.
	AllOK(ctx context.Context, monitors []string, deadline time.Time) (at time.Time, ok bool, err error)
	// States are the monitors' states as last mirrored.
	States(monitors []string) map[string]string
	// Connected says whether the DUT rack's monitors are streaming.
	Connected() bool
}

// Classes are ISO 16750-1's functional status classes (handoff C10), which
// Classify Recovery returns.
var Classes = []string{"A", "B", "C", "D", "E"}

// AddDUT adds the master's DUT keywords to l (handoff §4.8): the operations,
// and the keywords made of them.
func AddDUT(l *procedure.Library, r Rack, p Peer) {
	mode := procedure.Checked(procedure.Text("mode"), func(s string) error {
		if s == "" {
			return nil
		}
		return p.Mode(s)
	})
	action := procedure.Checked(procedure.Text("action"), p.Action)
	monitors := procedure.AnyP("monitors")
	checkMonitors := func(param string) func(a map[string]procedure.Static) error {
		return func(a map[string]procedure.Static) error {
			st := a[param]
			if !st.Known {
				return nil
			}
			ms, err := monitorsOf(st.Val)
			if err != nil {
				return fmt.Errorf("%s: %v", param, err)
			}
			for _, m := range ms {
				if err := p.Monitor(m); err != nil {
					return fmt.Errorf("%s: %v", param, err)
				}
			}
			return nil
		}
	}
	optAction := procedure.Opt(procedure.Checked(procedure.Text("action"), func(s string) error {
		if s == "" || s == "None" {
			return nil
		}
		return p.Action(s)
	}), "${None}")

	l.Add(&procedure.Keyword{
		Name:   "Mark Time",
		Doc:    "Returns this rack's clock, for since= of DUT Evaluate and Classify Recovery: ${t}=    Mark Time.",
		Params: nil,
		ReturnOf: func(map[string]procedure.Static) procedure.Static {
			return procedure.StaticNumber(procedureSeconds)
		},
		Run: func(*procedure.Ctx, procedure.Args) (procedure.Value, error) {
			return procedure.Number(float64(time.Now().UnixNano())/1e9, procedureSeconds), nil
		},
	})

	l.Add(&procedure.Keyword{
		Name: "DUT Set Mode",
		Doc: "Changes the DUT's mode, \"\" for none, and returns once the DUT rack has applied it and its settle time " +
			"has passed: DUT Set Mode    active.",
		Params: []procedure.Param{mode, procedure.Opt(procedure.Duration("timeout"), "10s")},
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			return procedure.Value{}, p.SetMode(c, a["mode"].Str, seconds(a["timeout"]))
		},
	})

	l.Add(&procedure.Keyword{
		Name:   "DUT Invoke",
		Doc:    "Runs a DUT action and returns once it has ended; fails as it did: DUT Invoke    reset.",
		Params: []procedure.Param{action, procedure.Opt(procedure.Duration("timeout"), "30s")},
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			return procedure.Value{}, p.Invoke(c, a["action"].Str, seconds(a["timeout"]))
		},
	})

	l.Add(&procedure.Keyword{
		Name: "DUT Wait For",
		Doc: "Waits until a condition over the DUT's mirrored channels holds, failing with timeout if it does not " +
			"and link_lost if the DUT rack stops answering: DUT Wait For    dut.mon.alive.state == \"ok\"    timeout=2s.",
		Params: []procedure.Param{procedure.Cond("condition"),
			procedure.Opt(procedure.Duration("timeout"), "10s"), procedure.Opt(procedure.Duration("poll"), "10ms")},
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			cond := c.Condition("condition")
			deadline := time.Now().Add(seconds(a["timeout"]))
			for {
				if !p.Connected() {
					return procedure.Value{}, procedure.Failf(procedure.LinkLost, "the DUT rack is not connected")
				}
				if ok, err := cond.Eval(); err == nil && ok {
					return procedure.Value{}, nil
				}
				if time.Now().After(deadline) {
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
		Name: "DUT Prepare",
		Doc: "Puts the DUT in a mode and waits for a monitor to be ok: DUT Prepare    active    ready=alive    " +
			"timeout=10s. The timeout bounds each of the two.",
		Params: []procedure.Param{mode, procedure.Checked(procedure.Text("ready"), p.Monitor),
			procedure.Opt(procedure.Duration("timeout"), "10s")},
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			to := seconds(a["timeout"])
			if err := p.SetMode(c, a["mode"].Str, to); err != nil {
				return procedure.Value{}, err
			}
			_, ok, err := p.AllOK(c, []string{a["ready"].Str}, time.Now().Add(to))
			switch {
			case err != nil:
				return procedure.Value{}, err
			case !ok:
				return procedure.Value{}, procedure.Failf(procedure.Timeout, "%s was not ok within %v",
					a["ready"].Str, to)
			}
			return procedure.Value{}, nil
		},
	})

	evaluate := func(c *procedure.Ctx, a procedure.Args) (map[string]string, time.Time, time.Time, error) {
		ms, err := monitorsOf(a["monitors"])
		if err != nil {
			return nil, time.Time{}, time.Time{}, procedure.Failf(procedure.Fail, "monitors: %v", err)
		}
		t0 := time.Now()
		if s := a["since"]; s.Kind == procedure.VNumber {
			t0 = time.Unix(0, int64(s.Num*1e9))
		}
		t1 := time.Now().Add(seconds(a["window"]))
		if err := c.Sleep(time.Until(t1)); err != nil {
			return nil, t0, t1, err
		}
		dev, err := p.Deviations(c, ms, t0, t1)
		return dev, t0, t1, err
	}
	window := procedure.Opt(procedure.Duration("window"), "0s")
	since := procedure.Opt(procedure.AnyP("since"), "${None}")

	l.Add(&procedure.Keyword{
		Name: "DUT Evaluate",
		Doc: "Returns nok if any of the monitors was dev or unknown at any time in a window, and ok if none was. " +
			"The window is from since= (a Mark Time), or from the call, to window= after the call: " +
			"${r}=    DUT Evaluate    alive,speed    window=2s. It waits for the DUT rack's watermark to pass the " +
			"window's end, and fails link_lost if it does not.",
		Params:    []procedure.Param{monitors, window, since},
		CheckArgs: checkMonitors("monitors"),
		ReturnOf:  textResult,
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			dev, _, _, err := evaluate(c, a)
			if err != nil {
				return procedure.Value{}, err
			}
			if len(dev) > 0 {
				return procedure.Str(string(procedure.NOK)), nil
			}
			return procedure.Str(string(procedure.OK)), nil
		},
	})

	l.Add(&procedure.Keyword{
		Name: "Verify Baseline",
		Doc: "Fails unless every monitor is ok throughout a window from the call: Verify Baseline    alive,speed    " +
			"window=1s. Run it before a stress, so that a DUT already deviating is not blamed on the stress.",
		Params:    []procedure.Param{monitors, procedure.Opt(procedure.Duration("window"), "1s"), since},
		CheckArgs: checkMonitors("monitors"),
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			dev, _, _, err := evaluate(c, a)
			if err != nil {
				return procedure.Value{}, err
			}
			if len(dev) > 0 {
				return procedure.Value{}, procedure.Failf(procedure.Fail, "not a baseline: %s", describe(dev))
			}
			return procedure.Value{}, nil
		},
	})

	l.Add(&procedure.Keyword{
		Name: "Classify Recovery",
		Doc: "Returns the DUT's ISO 16750-1 functional status class for a stress applied since since= (a Mark Time) " +
			"and removed before the call. A: no monitor deviated. B: only monitors outside function= deviated, and " +
			"all came back by themselves within timeout. C: a function= monitor deviated, and all came back by " +
			"themselves. D: they came back only after action=. E: they did not come back. " +
			"${class}=    Classify Recovery    alive,speed    since=${t}    function=alive    action=reset    timeout=2s.",
		Params: []procedure.Param{monitors, procedure.AnyP("since"),
			procedure.Opt(procedure.AnyP("function"), "${None}"), optAction,
			procedure.Opt(procedure.Duration("timeout"), "5s")},
		CheckArgs: func(a map[string]procedure.Static) error {
			if err := checkMonitors("monitors")(a); err != nil {
				return err
			}
			if f := a["function"]; f.Known && f.Val.Kind != procedure.VNone {
				return checkMonitors("function")(a)
			}
			return nil
		},
		ReturnOf: textResult,
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			ms, err := monitorsOf(a["monitors"])
			if err != nil {
				return procedure.Value{}, procedure.Failf(procedure.Fail, "monitors: %v", err)
			}
			var fn []string
			if f := a["function"]; f.Kind != procedure.VNone {
				if fn, err = monitorsOf(f); err != nil {
					return procedure.Value{}, procedure.Failf(procedure.Fail, "function: %v", err)
				}
			}
			if a["since"].Kind != procedure.VNumber {
				return procedure.Value{}, procedure.Failf(procedure.Fail, "since is %s; it is a time from Mark Time",
					a["since"].String())
			}
			act := a["action"].Str
			if a["action"].Kind == procedure.VNone {
				act = ""
			}
			class, err := classify(c, p, ms, fn, time.Unix(0, int64(a["since"].Num*1e9)), act, seconds(a["timeout"]))
			return procedure.Str(class), err
		},
	})

	l.Add(&procedure.Keyword{
		Name:      "DUT Snapshot",
		Doc:       "Returns the monitors' states as last mirrored, as text, which the step records: DUT Snapshot    alive,speed.",
		Params:    []procedure.Param{monitors},
		CheckArgs: checkMonitors("monitors"),
		ReturnOf:  textResult,
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			ms, err := monitorsOf(a["monitors"])
			if err != nil {
				return procedure.Value{}, procedure.Failf(procedure.Fail, "monitors: %v", err)
			}
			st := p.States(ms)
			var parts []string
			for _, m := range ms {
				parts = append(parts, m+"="+st[m])
			}
			return procedure.Str(strings.Join(parts, ", ")), nil
		},
	})

	addThresholdSearch(l, r, p, checkMonitors)
}

var procedureSeconds = mustUnit("s")

// textResult is the static type of a keyword returning text, ok and nok or a
// class, which a condition can then compare.
func textResult(map[string]procedure.Static) procedure.Static {
	return procedure.Static{Kind: procedure.VString, Typed: true, T: expr.Type{Kind: expr.String}}
}

// classify is Classify Recovery (handoff §4.8, C10).
func classify(ctx context.Context, p Peer, ms, fn []string, since time.Time, action string,
	timeout time.Duration) (string, error) {
	now := time.Now()
	dev, err := p.Deviations(ctx, ms, since, now)
	if err != nil {
		return "", err
	}
	if len(dev) == 0 {
		return "A", nil
	}
	at, ok, err := p.AllOK(ctx, ms, now.Add(timeout))
	if err != nil {
		return "", err
	}
	if ok {
		// Everything that deviated on the way back counts too: a function
		// that stopped after the stress was removed stopped because of it.
		if dev, err = p.Deviations(ctx, ms, since, at); err != nil {
			return "", err
		}
		for m := range dev {
			if slices.Contains(fn, m) {
				return "C", nil
			}
		}
		return "B", nil
	}
	if action == "" {
		return "E", nil
	}
	if err := p.Invoke(ctx, action, timeout); !actionRan(err) {
		return "", err
	}
	if _, ok, err = p.AllOK(ctx, ms, time.Now().Add(timeout)); err != nil {
		return "", err
	}
	if ok {
		return "D", nil
	}
	return "E", nil
}

// actionRan says whether an action was carried out, whatever came of it: an
// action that ran and failed — its own wait for the DUT timing out, say — is
// what classifying a DUT that did not come back is about, and is judged by the
// monitors after it. A refusal, a lost link or a stopped run is not.
func actionRan(err error) bool {
	var f *procedure.Failure
	if err == nil {
		return true
	}
	if !errors.As(err, &f) {
		return false
	}
	return f.Outcome == procedure.Timeout || f.Outcome == procedure.Fail
}

// monitorsOf reads a list of monitors: a list variable, or text separated by
// commas or spaces.
func monitorsOf(v procedure.Value) ([]string, error) {
	var out []string
	switch v.Kind {
	case procedure.VList:
		for _, x := range v.List {
			out = append(out, x.String())
		}
	case procedure.VString:
		out = strings.FieldsFunc(v.Str, func(r rune) bool { return r == ',' || r == ' ' })
	default:
		return nil, fmt.Errorf("%s is not a list of monitors: alive,speed or @{MONITORS}", v.String())
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no monitor is named")
	}
	return out, nil
}

// describe writes deviations as "alive dev, speed unknown", in name order.
func describe(dev map[string]string) string {
	var parts []string
	for _, m := range slices.Sorted(func(yield func(string) bool) {
		for k := range dev {
			if !yield(k) {
				return
			}
		}
	}) {
		parts = append(parts, m+" "+dev[m])
	}
	return strings.Join(parts, ", ")
}
