package lib

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rveen/ktest/procedure"
)

// DUTVersion is the DUT rack's library's version, recorded with every action.
const DUTVersion = "ktest-dut-lib 0.1.0"

// DUT is what the DUT rack's keywords need of the daemon. Every act goes
// through the control plane as the action's own request, recorded with via 4
// and the name that asked for the action.
type DUT interface {
	// Message checks, with a sentence when it is not, that the database has
	// a message and, unless signals is nil, that it encodes from them: a call
	// is checked at load as far as its arguments are known then.
	Message(name string, signals map[string]procedure.Value) error
	// Send puts one frame of a message on the bus. A signal's value is as the
	// procedure wrote it; the daemon reads it in the signal's own unit.
	Send(ctx context.Context, message string, signals map[string]procedure.Value) error
	// Cyclic starts or changes a cyclic task sending a message, held by the
	// DUT rack: it outlives the lease of whoever asked for the action.
	Cyclic(ctx context.Context, message string, signals map[string]procedure.Value, period time.Duration) error
	// StopCyclic stops a message's cyclic task.
	StopCyclic(ctx context.Context, message string) error

	// Settable checks that a channel is one this daemon can set, psu1.ch1.v.set.
	Settable(channel string) error
	// Set writes one setting, and returns once the instrument has answered.
	Set(ctx context.Context, channel string, value float64) error

	// Monitor checks that the contract declares a monitor.
	Monitor(name string) error
	// MonitorState is a monitor's state as last recorded, and whether there
	// is one.
	MonitorState(name string) (string, bool)
}

// NewDUT makes the DUT rack's library (handoff §4.5): the subset's built-ins,
// Settle, and the keywords that send, keep a cyclic task, set a channel and
// wait for a monitor. Nothing else: an action operates the DUT, and judging
// it is the monitors' job.
func NewDUT(d DUT) *procedure.Library {
	l := procedure.NewLibrary(DUTVersion)
	monitor := procedure.Checked(procedure.Text("monitor"), d.Monitor)
	message := procedure.Text("message")
	signals := procedure.Opt(procedure.AnyP("signals"), "${None}")
	// checkMessage checks a call at load: the message, and its signals when
	// they are known then, as a dictionary of the Variables is.
	checkMessage := func(a map[string]procedure.Static) error {
		m := a["message"]
		if !m.Known {
			return nil
		}
		sig, known, err := signalsOf(a["signals"])
		if err != nil {
			return err
		}
		if !known {
			return d.Message(m.Val.Str, nil)
		}
		if sig == nil {
			sig = map[string]procedure.Value{}
		}
		return d.Message(m.Val.Str, sig)
	}
	args := func(a procedure.Args) (string, map[string]procedure.Value, error) {
		sig, _, err := signalsOf(procedure.Static{Known: true, Val: a["signals"]})
		return a["message"].Str, sig, err
	}

	l.Add(&procedure.Keyword{
		Name:   "Settle",
		Doc:    "Waits a fixed time: Settle    500ms.",
		Params: []procedure.Param{procedure.Duration("duration")},
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			return procedure.Value{}, c.Sleep(seconds(a["duration"]))
		},
	})

	l.Add(&procedure.Keyword{
		Name: "Send Message",
		Doc: "Puts one frame of a database message on the bus, its signals from a dictionary in physical units; " +
			"a signal not given takes the database's start value. Send Message    DutReset    ${RESET}, " +
			"with &{RESET}    Request=1 in the Variables.",
		Params:    []procedure.Param{message, signals},
		CheckArgs: checkMessage,
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			m, sig, err := args(a)
			if err != nil {
				return procedure.Value{}, procedure.Failf(procedure.Fail, "%v", err)
			}
			return procedure.Value{}, d.Send(c, m, sig)
		},
	})

	l.Add(&procedure.Keyword{
		Name: "Start Cyclic",
		Doc: "Starts, or changes, a cyclic task sending a database message every period. The task is the DUT rack's: " +
			"it runs on when the lease of whoever asked for the action lapses, and stops on a disarm of the DUT or a stop. " +
			"Start Cyclic    EngineCmd    10ms    ${CMD}.",
		Params:    []procedure.Param{message, procedure.Duration("period"), signals},
		CheckArgs: checkMessage,
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			m, sig, err := args(a)
			if err != nil {
				return procedure.Value{}, procedure.Failf(procedure.Fail, "%v", err)
			}
			return procedure.Value{}, d.Cyclic(c, m, sig, seconds(a["period"]))
		},
	})

	l.Add(&procedure.Keyword{
		Name:   "Stop Cyclic",
		Doc:    "Stops a message's cyclic task: Stop Cyclic    EngineCmd.",
		Params: []procedure.Param{message},
		CheckArgs: func(a map[string]procedure.Static) error {
			if m := a["message"]; m.Known {
				return d.Message(m.Val.Str, nil)
			}
			return nil
		},
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			return procedure.Value{}, d.StopCyclic(c, a["message"].Str)
		},
	})

	l.Add(&procedure.Keyword{
		Name: "Set Channel",
		Doc: "Sets an instrument channel of this rack, through its control plane and limits: " +
			"Set Channel    psu1.ch1.v.set    13.5V.",
		Params: []procedure.Param{procedure.Checked(procedure.Text("channel"), d.Settable), procedure.AnyP("value")},
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			v := quantity(a["value"])
			var x float64
			switch v.Kind {
			case procedure.VNumber:
				x = v.Num
			case procedure.VBool:
				if v.Bool {
					x = 1
				}
			default:
				switch strings.ToLower(v.String()) {
				case "on":
					x = 1
				case "off":
				default:
					return procedure.Value{}, procedure.Failf(procedure.Fail, "%s is not a value to set", v.String())
				}
			}
			return procedure.Value{}, d.Set(c, a["channel"].Str, x)
		},
	})

	l.Add(&procedure.Keyword{
		Name: "Wait For Monitor",
		Doc: "Waits until a monitor of the contract is in a state, ok unless state= says otherwise, and fails with " +
			"timeout if it is not within timeout: Wait For Monitor    alive    timeout=2s.",
		Params: []procedure.Param{monitor,
			procedure.Opt(procedure.Checked(procedure.Text("state"), checkState), "ok"),
			procedure.Opt(procedure.Duration("timeout"), "10s"), procedure.Opt(procedure.Duration("poll"), "5ms")},
		Run: func(c *procedure.Ctx, a procedure.Args) (procedure.Value, error) {
			name, want := a["monitor"].Str, a["state"].Str
			deadline := time.Now().Add(seconds(a["timeout"]))
			for {
				got, ok := d.MonitorState(name)
				if ok && got == want {
					return procedure.Value{}, nil
				}
				if time.Now().After(deadline) {
					if !ok {
						got = "not yet recorded"
					}
					return procedure.Value{}, procedure.Failf(procedure.Timeout, "monitor %s was %s, not %s, after %v",
						name, got, want, seconds(a["timeout"]))
				}
				if err := c.Sleep(seconds(a["poll"])); err != nil {
					return procedure.Value{}, err
				}
			}
		},
	})
	return l
}

var monitorStates = []string{"ok", "dev", "unknown", "inactive", "settling"}

func checkState(s string) error {
	if !slices.Contains(monitorStates, s) {
		return fmt.Errorf("%q is not a monitor state: one of %s", s, strings.Join(monitorStates, ", "))
	}
	return nil
}

// signalsOf reads a signals argument: a dictionary of signal values, or none
// (nil). known is false while the argument is not known.
func signalsOf(st procedure.Static) (sig map[string]procedure.Value, known bool, err error) {
	if !st.Known {
		return nil, false, nil
	}
	v := st.Val
	switch v.Kind {
	case procedure.VNone:
		return nil, true, nil
	case procedure.VDict:
	default:
		return nil, true, fmt.Errorf("signals is %s; it is a dictionary, &{NAME} in the Variables, "+
			"passed as ${NAME}", v.String())
	}
	sig = map[string]procedure.Value{}
	for _, k := range v.Keys {
		sig[k] = v.Dict[k]
	}
	return sig, true, nil
}
