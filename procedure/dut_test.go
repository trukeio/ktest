package procedure_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trukeio/ktest/procedure"
	"github.com/trukeio/ktest/procedure/lib"
)

// peer is a stand-in for a DUT rack behind -peer: each wait answers from a
// script, so that every outcome path of every operation can be taken.
type peer struct {
	mu sync.Mutex
	// dev is what each call to Deviations answers, in turn; the last answer
	// repeats.
	dev []map[string]string
	// ok is what each call to AllOK answers, in turn; the last repeats.
	ok        []bool
	lost      bool  // every wait fails link_lost
	modeErr   error // SetMode's answer
	invokeErr error // Invoke's answer
	calls     []string
	nDev, nOK int
}

func (p *peer) Monitor(n string) error {
	if !slices.Contains([]string{"alive", "speed", "abort"}, n) {
		return fmt.Errorf("the contract has no monitor %q", n)
	}
	return nil
}
func (p *peer) Mode(n string) error {
	if n != "active" {
		return fmt.Errorf("the contract has no mode %q", n)
	}
	return nil
}
func (p *peer) Action(n string) error {
	if n != "reset" {
		return fmt.Errorf("the contract has no action %q", n)
	}
	return nil
}

func (p *peer) note(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, s)
}

func (p *peer) SetMode(_ context.Context, m string, _ time.Duration) error {
	p.note("mode " + m)
	return p.modeErr
}

func (p *peer) Invoke(_ context.Context, a string, _ time.Duration) error {
	p.note("invoke " + a)
	return p.invokeErr
}

func (p *peer) Deviations(_ context.Context, ms []string, t0, t1 time.Time) (map[string]string, error) {
	p.note("deviations " + strings.Join(ms, ","))
	if p.lost {
		return nil, procedure.Failf(procedure.LinkLost, "the DUT rack stopped answering")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.dev) == 0 {
		return nil, nil
	}
	d := p.dev[min(p.nDev, len(p.dev)-1)]
	p.nDev++
	return d, nil
}

func (p *peer) AllOK(_ context.Context, ms []string, _ time.Time) (time.Time, bool, error) {
	p.note("allok")
	if p.lost {
		return time.Time{}, false, procedure.Failf(procedure.LinkLost, "the DUT rack stopped answering")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ok := true
	if len(p.ok) > 0 {
		ok = p.ok[min(p.nOK, len(p.ok)-1)]
	}
	p.nOK++
	return time.Now(), ok, nil
}

func (p *peer) States(ms []string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		out[m] = "ok"
	}
	return out
}

func (p *peer) Connected() bool { return !p.lost }

func loadDUT(t *testing.T, r *rack, p *peer, src string) (*procedure.Suite, error) {
	t.Helper()
	l := lib.New(r)
	lib.AddDUT(l, r, p)
	return procedure.Load(write(t, "p.robot", src), procedure.Env{Lib: l, Channels: r})
}

// The master's DUT keywords are checked at load against the contract: every
// mode, action and monitor they name must be the contract's.
func TestDUTLoadChecks(t *testing.T) {
	for _, c := range []struct{ name, line, want string }{
		{"a mode the contract lacks", "DUT Set Mode    sleep", `no mode "sleep"`},
		{"an action the contract lacks", "DUT Invoke    reboot", `no action "reboot"`},
		{"a monitor the contract lacks", "${r}=    DUT Evaluate    alive,speeed    window=1s", `no monitor "speeed"`},
		{"a ready monitor the contract lacks", "DUT Prepare    active    ready=ready", `no monitor "ready"`},
		{"a function monitor the contract lacks", "${c}=    Classify Recovery    alive    since=${t}    function=heart", `no monitor "heart"`},
		{"a method the search does not have", "${x}=    Threshold Search    p    alive    target=psu1.ch1    nominal=13V    start=9V    step=1V    floor=3V    method=ascending", "descending"},
		{"a floor above the start", "${x}=    Threshold Search    p    alive    target=psu1.ch1    nominal=13V    start=9V    step=1V    floor=10V", "above start"},
		{"a step that does not descend", "${x}=    Threshold Search    p    alive    target=psu1.ch1    nominal=13V    start=9V    step=-1V    floor=3V", "positive"},
		{"a stress target this rack lacks", "${x}=    Threshold Search    p    alive    target=psu9.ch1    nominal=13V    start=9V    step=1V    floor=3V", "not a supply channel"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := "*** Test Cases ***\nT\n    ${t}=    Mark Time\n    " + c.line + "\n"
			_, err := loadDUT(t, newRack(), &peer{}, src)
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "p.robot:4") {
				t.Fatalf("got %v, want a refusal at p.robot:4 naming %q", err, c.want)
			}
		})
	}
}

// Each class of Classify Recovery, from a script of what the DUT rack's
// monitors did, and the action asked for only when the DUT did not come back.
func TestClassifyRecovery(t *testing.T) {
	src := `*** Test Cases ***
T
    ${t}=    Mark Time
    ${c}=    Classify Recovery    alive,speed    since=${t}    function=alive    action=reset    timeout=1s
    Log    ${c}
    Should Be True    $c == "${WANT}"
`
	for _, c := range []struct {
		class   string
		p       *peer
		invoked bool
	}{
		{"A", &peer{}, false},
		{"B", &peer{dev: []map[string]string{{"speed": "dev"}}}, false},
		{"C", &peer{dev: []map[string]string{{"alive": "dev", "speed": "unknown"}}}, false},
		{"C", &peer{dev: []map[string]string{{"speed": "dev"}, {"speed": "dev", "alive": "dev"}}}, false}, // alive stopped on the way back
		{"D", &peer{dev: []map[string]string{{"alive": "dev"}}, ok: []bool{false, true}}, true},
		{"E", &peer{dev: []map[string]string{{"alive": "dev"}}, ok: []bool{false, false}}, true},
		// The action runs and fails, its own wait timing out: still E.
		{"E", &peer{dev: []map[string]string{{"alive": "dev"}}, ok: []bool{false, false},
			invokeErr: procedure.Failf(procedure.Timeout, "alive was dev after 1s")}, true},
	} {
		t.Run(c.class, func(t *testing.T) {
			r := newRack()
			s, err := loadDUT(t, r, c.p, strings.Replace(src, "${WANT}", c.class, 1))
			if err != nil {
				t.Fatal(err)
			}
			if res := run(t, s, r, "T"); res.Outcome != procedure.OK {
				t.Fatalf("%s: %s (calls %v)", res.Outcome, res.Msg, c.p.calls)
			}
			if got := slices.Contains(c.p.calls, "invoke reset"); got != c.invoked {
				t.Errorf("the action was asked for: %v, want %v (calls %v)", got, c.invoked, c.p.calls)
			}
		})
	}
	// Without an action, a DUT that does not come back is E, and nothing is
	// asked of it.
	r, p := newRack(), &peer{dev: []map[string]string{{"alive": "dev"}}, ok: []bool{false}}
	s, err := loadDUT(t, r, p, strings.Replace(strings.Replace(src, "    action=reset", "", 1), "${WANT}", "E", 1))
	if err != nil {
		t.Fatal(err)
	}
	if res := run(t, s, r, "T"); res.Outcome != procedure.OK || slices.Contains(p.calls, "invoke reset") {
		t.Fatalf("%s %s %v", res.Outcome, res.Msg, p.calls)
	}
}

// Every operation's outcomes: ok and nok as values, refused, timeout and
// link_lost failing the step and the test, with the teardown run.
func TestDUTOutcomes(t *testing.T) {
	src := `*** Test Cases ***
T
    [Teardown]    Log    teardown
    ` + "%s\n"
	for _, c := range []struct {
		name, line string
		p          *peer
		outcome    procedure.Outcome // the test's
		step       string            // the operation's step, as "Keyword outcome"
	}{
		{"evaluate ok", "${r}=    DUT Evaluate    alive    window=10ms", &peer{}, procedure.OK, "DUT Evaluate ok"},
		{"evaluate nok is a value", "${r}=    DUT Evaluate    alive    window=10ms", &peer{dev: []map[string]string{{"alive": "dev"}}},
			procedure.OK, "DUT Evaluate nok"},
		{"evaluate with the link lost", "${r}=    DUT Evaluate    alive    window=10ms", &peer{lost: true},
			procedure.LinkLost, "DUT Evaluate link_lost"},
		{"baseline that is not one", "Verify Baseline    alive,speed    window=10ms", &peer{dev: []map[string]string{{"speed": "unknown"}}},
			procedure.Fail, "Verify Baseline fail"},
		{"mode refused", "DUT Set Mode    active", &peer{modeErr: procedure.Failf(procedure.Refused, "no lease")},
			procedure.Refused, "DUT Set Mode refused"},
		{"mode timed out", "DUT Set Mode    active", &peer{modeErr: procedure.Failf(procedure.Timeout, "not applied")},
			procedure.Timeout, "DUT Set Mode timeout"},
		{"prepare not ready", "DUT Prepare    active    ready=alive    timeout=10ms", &peer{ok: []bool{false}},
			procedure.Timeout, "DUT Prepare timeout"},
		{"invoke failed", "DUT Invoke    reset", &peer{invokeErr: procedure.Failf(procedure.Fail, "reset: no reply")},
			procedure.Fail, "DUT Invoke fail"},
		{"wait with the link lost", "DUT Wait For    psu1.ch1.out.applied    timeout=10ms", &peer{lost: true},
			procedure.LinkLost, "DUT Wait For link_lost"},
		{"wait timed out", "DUT Wait For    psu1.ch1.meas.v > 20V    timeout=20ms", &peer{},
			procedure.Timeout, "DUT Wait For timeout"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRack()
			s, err := loadDUT(t, r, c.p, fmt.Sprintf(src, c.line))
			if err != nil {
				t.Fatal(err)
			}
			res := run(t, s, r, "T")
			if res.Outcome != c.outcome {
				t.Errorf("test %s (%s), want %s", res.Outcome, res.Msg, c.outcome)
			}
			ends := r.ends()
			if !slices.Contains(ends, c.step) {
				t.Errorf("steps %v, want %q among them", ends, c.step)
			}
			if !slices.Contains(ends, "Log ok") {
				t.Errorf("the teardown did not run: %v", ends)
			}
		})
	}
}

// Threshold Search steps the dip down until a monitor deviates, records each
// level and the threshold, and returns the threshold: the last level passed.
func TestThresholdSearch(t *testing.T) {
	src := `*** Test Cases ***
T
    ${th}=    Threshold Search    dip    alive    target=psu1.ch1    nominal=13.5V    start=9V    step=0.5V    floor=6V    duration=1ms    recover=10ms
    Should Be True    ${th} == 8V
`
	r := newRack()
	p := &peer{dev: []map[string]string{nil, nil, nil, {"alive": "dev"}}}
	s, err := loadDUT(t, r, p, src)
	if err != nil {
		t.Fatal(err)
	}
	if res := run(t, s, r, "T"); res.Outcome != procedure.OK {
		t.Fatalf("%s: %s", res.Outcome, res.Msg)
	}
	var got []string
	for _, x := range r.results {
		got = append(got, x.Level.String()+" "+x.Result)
	}
	want := []string{"9V ok", "8.5V ok", "8V ok", "7.5V nok", "8V threshold"}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("results %v, want %v", got, want)
	}
	wantSets := "psu1.ch1.v.set=9 psu1.ch1.v.set=13.5 psu1.ch1.v.set=8.5 psu1.ch1.v.set=13.5 psu1.ch1.v.set=8 " +
		"psu1.ch1.v.set=13.5 psu1.ch1.v.set=7.5 psu1.ch1.v.set=13.5"
	if strings.Join(r.sets, " ") != wantSets {
		t.Errorf("sets %v", r.sets)
	}

	// A DUT that deviates already at the start has no threshold in range.
	r, p = newRack(), &peer{dev: []map[string]string{{"alive": "dev"}}}
	s, _ = loadDUT(t, r, p, src)
	if res := run(t, s, r, "T"); res.Outcome != procedure.Fail || !strings.Contains(res.Msg, "already at start") {
		t.Errorf("%s: %s", res.Outcome, res.Msg)
	}
}
