package lib

import (
	"fmt"
	"time"

	"github.com/trukeio/ktest/expr"
	"github.com/trukeio/ktest/procedure"
)

func mustUnit(s string) expr.Unit { return expr.ParseUnit(s) }

// addThresholdSearch adds Threshold Search: the stress level at which the DUT
// stops coping, found by stepping a supply dip down until a monitor deviates.
func addThresholdSearch(l *procedure.Library, r Rack, p Peer, checkMonitors func(string) func(map[string]procedure.Static) error) {
	volts := func(name string) procedure.Param { return procedure.Quantity(name, "V") }
	target := procedure.Checked(procedure.Text("target"), r.Stressor)
	l.Add(&procedure.Keyword{
		Name: "Threshold Search",
		Doc: "Finds the lowest supply dip the DUT copes with. From start= down by step= to floor=, the target " +
			"dips to each level for duration= and returns to nominal=; a level passes when no monitor deviates from " +
			"the dip's start until they are all ok again, which they have recover= to be, after action= if they " +
			"are not. Each level is recorded as a step result, and the threshold, the lowest level passed before the " +
			"first that was not, is returned and recorded. The target is at nominal and on, as Stress Apply leaves " +
			"it. ${th}=    Threshold Search    dip    alive,speed    target=psu1.ch1    nominal=13.5V    start=9V    " +
			"step=0.5V    floor=3V    duration=300ms    action=reset.",
		Params: []procedure.Param{procedure.Text("point"), procedure.AnyP("monitors"), target,
			volts("nominal"), volts("start"), volts("step"), volts("floor"),
			procedure.Opt(procedure.Duration("duration"), "300ms"),
			procedure.Opt(procedure.Duration("recover"), "2s"),
			procedure.Opt(procedure.Checked(procedure.Text("action"), func(s string) error {
				if s == "" || s == "None" {
					return nil
				}
				return p.Action(s)
			}), "${None}"),
			procedure.Opt(procedure.Checked(procedure.Text("method"), func(s string) error {
				if s != "descending" {
					return fmt.Errorf("%q is not a method this keyword has; it has descending", s)
				}
				return nil
			}), "descending")},
		CheckArgs: func(a map[string]procedure.Static) error {
			if err := checkMonitors("monitors")(a); err != nil {
				return err
			}
			num := func(n string) (float64, bool) {
				st := a[n]
				if !st.Known {
					return 0, false
				}
				q := quantity(st.Val)
				return q.Num, q.Kind == procedure.VNumber
			}
			if s, ok := num("step"); ok && s <= 0 {
				return fmt.Errorf("step is %gV: a descending search steps down by a positive amount", s)
			}
			st, ok1 := num("start")
			fl, ok2 := num("floor")
			if ok1 && ok2 && fl > st {
				return fmt.Errorf("floor %gV is above start %gV", fl, st)
			}
			return nil
		},
		ReturnOf: func(map[string]procedure.Static) procedure.Static { return procedure.StaticNumber(mustUnit("V")) },
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			ms, err := monitorsOf(a["monitors"])
			if err != nil {
				return procedure.Value{}, procedure.Failf(procedure.Fail, "monitors: %v", err)
			}
			v := mustUnit("V")
			point, tgt := a["point"].Str, a["target"].Str
			nominal, step, floor := a["nominal"].Num, a["step"].Num, a["floor"].Num
			action := a["action"].Str
			if a["action"].Kind == procedure.VNone {
				action = ""
			}
			recov, dur := seconds(a["recover"]), seconds(a["duration"])
			var passed *float64
			// The levels are counted, not accumulated, so that 0.1 V steps do
			// not drift off the levels written.
			for i := 0; ; i++ {
				level := a["start"].Num - float64(i)*step
				if level < floor-step/1e6 {
					break
				}
				t0 := time.Now()
				if err := r.Set(c, tgt+".v.set", level); err != nil {
					return procedure.Value{}, err
				}
				if err := c.Sleep(dur); err != nil {
					return procedure.Value{}, err
				}
				if err := r.Set(c, tgt+".v.set", nominal); err != nil {
					return procedure.Value{}, err
				}
				at, ok, err := p.AllOK(c, ms, time.Now().Add(recov))
				if err != nil {
					return procedure.Value{}, err
				}
				if !ok && action != "" {
					if err := p.Invoke(c, action, recov); !actionRan(err) {
						return procedure.Value{}, err
					}
					at, ok, err = p.AllOK(c, ms, time.Now().Add(recov))
					if err != nil {
						return procedure.Value{}, err
					}
				}
				if !ok {
					r.Result(StepResult{Point: point, Level: procedure.Number(level, v), Result: "lost"})
					how := "and there is no action="
					if action != "" {
						how = "even after " + action
					}
					return procedure.Value{}, procedure.Failf(procedure.Fail,
						"the DUT did not recover from a dip to %gV within %v, %s", level, recov, how)
				}
				dev, err := p.Deviations(c, ms, t0, at)
				if err != nil {
					return procedure.Value{}, err
				}
				if len(dev) > 0 {
					r.Result(StepResult{Point: point, Level: procedure.Number(level, v), Result: "nok"})
					break
				}
				r.Result(StepResult{Point: point, Level: procedure.Number(level, v), Result: "ok"})
				l := level
				passed = &l
			}
			if passed == nil {
				return procedure.Value{}, procedure.Failf(procedure.Fail,
					"the DUT deviated already at start, %gV: the threshold is above it", a["start"].Num)
			}
			th := procedure.Number(*passed, v)
			r.Result(StepResult{Point: point, Level: th, Result: "threshold", Threshold: &th})
			return th, nil
		},
	})
}
