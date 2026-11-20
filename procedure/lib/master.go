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

// AddDUT adds the master's DUT keywords to l (handoff §4.8) for one DUT
// rack: the operations, and the keywords made of them.
func AddDUT(l *procedure.Library, r Rack, p Peer) { AddDUTs(l, r, map[string]Peer{"": p}) }

// AddDUTs adds the DUT keywords for the DUT racks a master drives, by name.
// Every keyword that reaches a DUT takes dut=, the rack's name: it may be
// left out when there is one rack, and must be given when there are more,
// so that a procedure written for one rack runs unchanged and one written for
// several says which DUT each step is about. The names a call gives — modes,
// actions, monitors — are checked at load against that rack's contract.
func AddDUTs(l *procedure.Library, r Rack, peers map[string]Peer) {
	d := &duts{peers: peers}
	dut := procedure.Opt(procedure.Checked(procedure.Text("dut"), d.known), "")
	mode := procedure.Text("mode")
	action := procedure.Text("action")
	monitors := procedure.AnyP("monitors")
	optAction := procedure.Opt(procedure.Text("action"), "${None}")
	checkMonitors := func(param string) func(a map[string]procedure.Static) error {
		return d.check(param + ":monitors")
	}

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
		Params:    []procedure.Param{mode, procedure.Opt(procedure.Duration("timeout"), "10s"), dut},
		CheckArgs: d.check("mode:mode"),
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			p, err := d.pick(a)
			if err != nil {
				return procedure.Value{}, err
			}
			return procedure.Value{}, p.SetMode(c, a["mode"].Str, seconds(a["timeout"]))
		},
	})

	l.Add(&procedure.Keyword{
		Name:   "DUT Invoke",
		Doc:    "Runs a DUT action and returns once it has ended; fails as it did: DUT Invoke    reset.",
		Params:    []procedure.Param{action, procedure.Opt(procedure.Duration("timeout"), "30s"), dut},
		CheckArgs: d.check("action:action"),
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			p, err := d.pick(a)
			if err != nil {
				return procedure.Value{}, err
			}
			return procedure.Value{}, p.Invoke(c, a["action"].Str, seconds(a["timeout"]))
		},
	})

	l.Add(&procedure.Keyword{
		Name: "DUT Wait For",
		Doc: "Waits until a condition over the DUT's mirrored channels holds, failing with timeout if it does not " +
			"and link_lost if the DUT rack stops answering: DUT Wait For    dut.mon.alive.state == \"ok\"    timeout=2s.",
		Params: []procedure.Param{procedure.Cond("condition"),
			procedure.Opt(procedure.Duration("timeout"), "10s"), procedure.Opt(procedure.Duration("poll"), "10ms"), dut},
		CheckArgs: d.check(),
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			p, err := d.pick(a)
			if err != nil {
				return procedure.Value{}, err
			}
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
		Params: []procedure.Param{mode, procedure.Text("ready"),
			procedure.Opt(procedure.Duration("timeout"), "10s"), dut},
		CheckArgs: d.check("mode:mode", "ready:monitor"),
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			p, err := d.pick(a)
			if err != nil {
				return procedure.Value{}, err
			}
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
		p, err := d.pick(a)
		if err != nil {
			return nil, t0, t1, err
		}
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
		Params:    []procedure.Param{monitors, window, since, dut},
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
		Params:    []procedure.Param{monitors, procedure.Opt(procedure.Duration("window"), "1s"), since, dut},
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
			procedure.Opt(procedure.Duration("timeout"), "5s"), dut},
		CheckArgs: d.check("monitors:monitors", "function:monitors", "action:action"),
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
			p, err := d.pick(a)
			if err != nil {
				return procedure.Value{}, err
			}
			class, err := classify(c, p, ms, fn, time.Unix(0, int64(a["since"].Num*1e9)), act, seconds(a["timeout"]))
			return procedure.Str(class), err
		},
	})

	l.Add(&procedure.Keyword{
		Name:      "DUT Snapshot",
		Doc:       "Returns the monitors' states as last mirrored, as text, which the step records: DUT Snapshot    alive,speed.",
		Params:    []procedure.Param{monitors, dut},
		CheckArgs: checkMonitors("monitors"),
		ReturnOf:  textResult,
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			ms, err := monitorsOf(a["monitors"])
			if err != nil {
				return procedure.Value{}, procedure.Failf(procedure.Fail, "monitors: %v", err)
			}
			p, err := d.pick(a)
			if err != nil {
				return procedure.Value{}, err
			}
			st := p.States(ms)
			var parts []string
			for _, m := range ms {
				parts = append(parts, m+"="+st[m])
			}
			return procedure.Str(strings.Join(parts, ", ")), nil
		},
	})

	addThresholdSearch(l, r, d, dut)
}

// duts are the DUT racks the keywords reach, by name; "" is the one rack of
// a master that drives one.
type duts struct {
	peers map[string]Peer
}

func (d *duts) names() string {
	var ns []string
	for n := range d.peers {
		ns = append(ns, n)
	}
	slices.Sort(ns)
	return strings.Join(ns, ", ")
}

// known checks a dut= the call gives.
func (d *duts) known(name string) error {
	if name == "" {
		return nil
	}
	if _, ok := d.peers[name]; !ok {
		if _, one := d.peers[""]; one {
			return fmt.Errorf("dut=%s: this rack drives one DUT rack, which needs no name", name)
		}
		return fmt.Errorf("dut=%s: this rack drives %s", name, d.names())
	}
	return nil
}

// peer is the rack a dut= names: the only one when it is "", which is
// right only when there is one.
func (d *duts) peer(name string) (Peer, error) {
	if name == "" {
		if len(d.peers) == 1 {
			for _, p := range d.peers {
				return p, nil
			}
		}
		return nil, fmt.Errorf("dut= is required: this rack drives %s", d.names())
	}
	if p, ok := d.peers[name]; ok {
		return p, nil
	}
	return nil, d.known(name)
}

// pick is the rack a call is about, as it runs.
func (d *duts) pick(a procedure.Args) (Peer, error) {
	p, err := d.peer(a["dut"].Str)
	if err != nil {
		return nil, procedure.Failf(procedure.Fail, "%v", err)
	}
	return p, nil
}

// check is a CheckArgs that holds the names a call gives to the contract of
// the rack its dut= names, when that is known at load. Each spec is
// param:kind, the kind mode, action, monitor or monitors (a list).
func (d *duts) check(specs ...string) func(a map[string]procedure.Static) error {
	return func(a map[string]procedure.Static) error {
		st := a["dut"]
		if !st.Known {
			return nil // a variable: it is checked when the call runs
		}
		p, err := d.peer(st.Val.Str)
		if err != nil {
			return err
		}
		for _, spec := range specs {
			param, kind, _ := strings.Cut(spec, ":")
			v := a[param]
			if !v.Known || v.Val.Kind == procedure.VNone {
				continue
			}
			var names []string
			switch kind {
			case "monitors":
				ms, err := monitorsOf(v.Val)
				if err != nil {
					return fmt.Errorf("%s: %v", param, err)
				}
				names = ms
			default:
				if s := v.Val.Str; s != "" && s != "None" {
					names = []string{s}
				}
			}
			for _, n := range names {
				var err error
				switch kind {
				case "mode":
					err = p.Mode(n)
				case "action":
					err = p.Action(n)
				default:
					err = p.Monitor(n)
				}
				if err != nil {
					return fmt.Errorf("%s: %v", param, err)
				}
			}
		}
		return nil
	}
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
