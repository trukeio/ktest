package procedure_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rveen/ktest/expr"
	"github.com/rveen/ktest/procedure"
	"github.com/rveen/ktest/procedure/lib"
)

// rack is a stand-in for the daemon: a few channels, the writes asked of it,
// the results recorded, and the steps.
type rack struct {
	mu      sync.Mutex
	values  map[string]expr.Value
	types   map[string]expr.Type
	sets    []string
	refuse  map[string]bool
	results []lib.StepResult
	steps   []procedure.StepEvent
}

func newRack() *rack {
	return &rack{
		values: map[string]expr.Value{"psu1.ch1.meas.v": expr.Num(12), "psu1.ch1.out.applied": expr.BoolValue(true)},
		types: map[string]expr.Type{
			"psu1.ch1.meas.v":      expr.NumberIn("V"),
			"psu1.ch1.meas.i":      expr.NumberIn("A"),
			"psu1.ch1.out.applied": {Kind: expr.Bool},
		},
		refuse: map[string]bool{},
	}
}

func (r *rack) Resolve(name string) (expr.Type, error) {
	t, ok := r.types[name]
	if !ok {
		return t, fmt.Errorf("%s is not a channel of this rack", name)
	}
	return t, nil
}

func (r *rack) Value(name string) (expr.Value, int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.values[name]
	return v, time.Now().UnixNano(), ok
}

func (r *rack) Stressor(t string) error {
	if t != "psu1.ch1" && t != "psu1.ch2" {
		return fmt.Errorf("%s is not a supply channel of this rack", t)
	}
	return nil
}

func (r *rack) Set(_ context.Context, ch string, v float64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refuse[ch] {
		return procedure.Failf(procedure.Refused, "%s: interlock", ch)
	}
	r.sets = append(r.sets, fmt.Sprintf("%s=%g", ch, v))
	if ch == "psu1.ch1.v.set" {
		r.values["psu1.ch1.meas.v"] = expr.Num(v)
	}
	return nil
}

func (r *rack) Result(sr lib.StepResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, sr)
}

func (r *rack) Step(ev procedure.StepEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, ev)
}

// reset forgets what earlier runs did. The library a suite was loaded with
// holds this rack, so a test reuses it rather than making another.
func (r *rack) reset() {
	n := newRack()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values, r.sets, r.refuse, r.results, r.steps = n.values, nil, n.refuse, nil, nil
}

// ends are the steps that ended, as "Keyword outcome", for comparing.
func (r *rack) ends() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range r.steps {
		if s.End {
			out = append(out, s.Keyword+" "+string(s.Outcome))
		}
	}
	return out
}

// write puts files in a directory, the first being the procedure.
func write(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	var first string
	for i := 0; i+1 < len(files); i += 2 {
		p := filepath.Join(dir, files[i])
		if err := os.WriteFile(p, []byte(files[i+1]), 0o644); err != nil {
			t.Fatal(err)
		}
		if first == "" {
			first = p
		}
	}
	return first
}

func load(t *testing.T, r *rack, files ...string) (*procedure.Suite, error) {
	t.Helper()
	return procedure.Load(write(t, files...), procedure.Env{Lib: lib.New(r), Channels: r})
}

func mustLoad(t *testing.T, r *rack, files ...string) *procedure.Suite {
	t.Helper()
	s, err := load(t, r, files...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func run(t *testing.T, s *procedure.Suite, r *rack, test string) procedure.Result {
	t.Helper()
	return s.Run(context.Background(), context.Background(), test, r)
}

// TestRefusals holds every construct outside the subset, and every load-time
// check, to its sentence and its line.
func TestRefusals(t *testing.T) {
	for _, c := range []struct {
		name, src, want string
	}{
		{"Library", "*** Settings ***\nLibrary    OperatingSystem\n", "p.robot:2: Library is not in the subset"},
		{"suite setup", "*** Settings ***\nSuite Setup    Log    x\n", "Suite Setup is not in the subset; give each test its own [Setup]"},
		{"test template", "*** Settings ***\nTest Template    Log\n", "templates are not in the subset"},
		{"Evaluate", "*** Test Cases ***\nT\n    ${x}=    Evaluate    1+1\n", "p.robot:3: Evaluate is not in the subset"},
		{"inline python", "*** Test Cases ***\nT\n    Log    ${{1+1}}\n", "p.robot:3: ${{1+1}}: ${{...}}, inline Python, is not in the subset"},
		{"TRY", "*** Test Cases ***\nT\n    TRY\n        Log    x\n    EXCEPT\n        Log    y\n    END\n", "p.robot:3: TRY/EXCEPT is not in the subset"},
		{"GROUP", "*** Test Cases ***\nT\n    GROUP    g\n    END\n", "GROUP is not in the subset"},
		{"VAR", "*** Test Cases ***\nT\n    VAR    ${x}    1\n", "VAR is not in the subset"},
		{"[Template]", "*** Test Cases ***\nT\n    [Template]    Log\n", "[Template]: templates are not in the subset"},
		{"Set Suite Variable", "*** Test Cases ***\nT\n    Set Suite Variable    ${x}    1\n", "Set Suite Variable is not in the subset"},
		{"Set Global Variable", "*** Test Cases ***\nT\n    Set Global Variable    ${x}    1\n", "Set Global Variable is not in the subset"},
		{"Set Test Variable", "*** Test Cases ***\nT\n    Set Test Variable    ${x}    1\n", "Set Test Variable is not in the subset"},
		{"Run Keyword", "*** Test Cases ***\nT\n    Run Keyword    Log    x\n", "Run Keyword is not in the subset"},
		{"Run Keyword If", "*** Test Cases ***\nT\n    Run Keyword If    True    Log    x\n", "Run Keyword If is not in the subset; use IF"},
		{"Sleep", "*** Test Cases ***\nT\n    Sleep    1s\n", "Sleep is Settle here"},
		{"WHILE without limit", "*** Test Cases ***\nT\n    WHILE    True\n        Log    x\n    END\n", "p.robot:3: WHILE needs limit="},
		{"WHILE limit=NONE", "*** Test Cases ***\nT\n    WHILE    True    limit=NONE\n    END\n", "WHILE limit=NONE is not in the subset"},
		{"WHILE on_limit", "*** Test Cases ***\nT\n    WHILE    True    limit=3    on_limit=pass\n    END\n", "WHILE on_limit is not in the subset"},
		{"inline IF", "*** Test Cases ***\nT\n    IF    True    Log    x\n", "inline IF is not in the subset"},
		{"IN ENUMERATE", "*** Test Cases ***\nT\n    FOR    ${i}    IN ENUMERATE    a    b\n    END\n", "FOR ... IN ENUMERATE is not in the subset"},
		{"no END", "*** Test Cases ***\nT\n    FOR    ${i}    IN    a\n        Log    ${i}\n", "p.robot:3: FOR has no END"},
		{"[Return]", "*** Keywords ***\nK\n    [Return]    1\n", "[Return] is old syntax; write RETURN"},
		{"embedded args", "*** Keywords ***\nSet ${x} High\n    Log    x\n", "keywords with embedded arguments are not in the subset"},
		{"item access", "*** Test Cases ***\nT\n    Log    ${x}[0]\n", "item access ${x}[...] is not in the subset"},
		{"pipes", "*** Test Cases ***\n| T |\n", "the pipe-separated format is not in the subset"},
		{"yaml", "*** Settings ***\nVariables    v.yaml\n*** Test Cases ***\nT\n    Log    x\n", "YAML variable files are not read yet"},
		{"Tasks", "*** Tasks ***\n", "a procedure's section is *** Test Cases ***"},
		{"library prefix", "*** Test Cases ***\nT\n    BuiltIn.Log    x\n", "library prefixes are not in the subset"},

		// Load-time checks.
		{"unknown keyword", "*** Test Cases ***\nT\n    Frobnicate    x\n", `p.robot:3: no keyword named "Frobnicate"`},
		{"unknown argument", "*** Test Cases ***\nT\n    Settle    duraton=1s\n", `Settle has no argument "duraton"; its arguments are duration`},
		{"missing argument", "*** Test Cases ***\nT\n    Settle\n", "Settle needs its argument duration"},
		{"too many arguments", "*** Test Cases ***\nT\n    Settle    1s    2s\n", "Settle takes at most 1 arguments"},
		{"positional after named", "*** Test Cases ***\nT\n    Wait Until    condition=True    1s\n", "comes after a named argument"},
		{"wrong unit", "*** Test Cases ***\nT\n    Settle    5V\n", "Settle: duration: 5V is in V, and this takes a duration, such as 500ms"},
		{"no unit", "*** Test Cases ***\nT\n    Settle    5\n", "Settle: duration: 5 needs a unit"},
		{"stress in amps", "*** Test Cases ***\nT\n    Stress Apply    psu1.ch1    2A\n", "level: 2A is in A, and this takes a number in V"},
		{"unknown stressor", "*** Test Cases ***\nT\n    Stress Apply    psu9.ch1    12V\n", "psu9.ch1 is not a supply channel of this rack"},
		{"unknown channel", "*** Test Cases ***\nT\n    ${v}=    Read Channel    psu1.ch9.meas.v\n", "psu1.ch9.meas.v is not a channel of this rack"},
		{"undefined variable", "*** Test Cases ***\nT\n    Log    ${nope}\n", "p.robot:3: ${nope} is not defined here"},
		{"variable from one IF branch", "*** Test Cases ***\nT\n    IF    True\n        ${x}=    Set Variable    1\n    END\n    Log    ${x}\n",
			"p.robot:6: ${x} is not defined here"},
		{"loop variable after loop", "*** Test Cases ***\nT\n    FOR    ${i}    IN    a\n    END\n    Log    ${i}\n", "${i} is not defined here"},
		{"condition units", "*** Test Cases ***\nT\n    IF    psu1.ch1.meas.v > 2A\n    END\n",
			`p.robot:3: condition "psu1.ch1.meas.v > 2A": column 17: >: V and A are not the same kind of quantity`},
		{"condition arithmetic", "*** Variables ***\n${x}    3V\n*** Test Cases ***\nT\n    IF    $x + 1V > 2V\n    END\n",
			"does no arithmetic"},
		{"condition unknown name", "*** Test Cases ***\nT\n    IF    psu7.v > 2V\n    END\n", "psu7.v is not a channel of this rack"},
		{"condition &&", "*** Test Cases ***\nT\n    Should Be True    True && True\n", "&&: write and"},
		{"untyped variable in condition",
			"*** Test Cases ***\nT\n    FOR    ${x}    IN    1V    a\n        IF    $x > 0V\n        END\n    END\n",
			"what ${x} holds is not known when the procedure loads"},
		{"BREAK outside loop", "*** Test Cases ***\nT\n    BREAK\n", "BREAK outside FOR or WHILE"},
		{"RETURN in a test", "*** Test Cases ***\nT\n    RETURN\n", "RETURN is for keywords"},
		{"recursion", "*** Test Cases ***\nT\n    A\n*** Keywords ***\nA\n    B\nB\n    A\n",
			`keyword "A" calls itself, through A → B → A; procedures do not recurse`},
		{"self recursion in an unused keyword", "*** Test Cases ***\nT\n    Log    x\n*** Keywords ***\nA\n    A\n",
			`keyword "A" calls itself`},
		{"duplicate keyword", "*** Test Cases ***\nT\n    Log    x\n*** Keywords ***\nA\n    Log    1\nA\n    Log    2\n",
			`keyword "A" is already defined at`},
		{"keyword named as a library one", "*** Test Cases ***\nT\n    Log    x\n*** Keywords ***\nSettle\n    Log    1\n",
			"has the name of a library keyword"},
		{"bad timeout", "*** Test Cases ***\nT\n    [Timeout]    3V\n    Log    x\n", "[Timeout]: 3V is in V"},
		{"no tests", "*** Keywords ***\nA\n    Log    x\n", "a procedure has at least one test case"},
		{"user keyword argument", "*** Test Cases ***\nT\n    Ramp    to=12V\n*** Keywords ***\nRamp\n    [Arguments]    ${level}\n    Log    ${level}\n",
			`Ramp has no argument "to"; its arguments are level`},
		{"error inside a called keyword", "*** Test Cases ***\nT\n    Ramp    2A\n*** Keywords ***\nRamp\n    [Arguments]    ${level}\n    Stress Apply    psu1.ch1    ${level}\n",
			"p.robot:7: Stress Apply: level: 2A is in A, and this takes a number in V (called from p.robot:3)"},
	} {
		_, err := load(t, newRack(), "p.robot", c.src, "v.yaml", "x: 1")
		if err == nil {
			t.Errorf("%s: loaded, want %q", c.name, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, err, c.want)
		}
	}
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRun(t *testing.T) {
	r := newRack()
	sweep, common := fixture(t, "sweep.robot"), fixture(t, "common.resource")
	// ${n + 1} is not a variable of the subset: there is no arithmetic in a
	// cell, and the procedure is refused for it rather than run wrongly.
	bad := strings.Replace(common, "${m}=    Increment    ${n}\n        RETURN    ${m}", "RETURN    ${n + 1}", 1)
	_, err := load(t, r, "sweep.robot", sweep, "common.resource", bad, "levels.json", fixture(t, "levels.json"))
	if err == nil || !strings.Contains(err.Error(), "${n+1} is not defined here") {
		t.Fatalf("got %v", err)
	}
	s := mustLoad(t, r, "sweep.robot", sweep, "common.resource", common, "levels.json", fixture(t, "levels.json"))
	if got := s.Tests(); strings.Join(got, ",") != "Sweep,Fails,Loops,Slow,Refused" {
		t.Errorf("tests %v", got)
	}
	if len(s.Files) != 3 || len(s.Hash) != 64 {
		t.Errorf("files %v, hash %q", s.Files, s.Hash)
	}

	res := run(t, s, r, "Sweep")
	if res.Outcome != procedure.OK {
		t.Fatalf("Sweep: %+v", res)
	}
	want := []string{"psu1.ch1.v.set=10", "psu1.ch1.out.set=1", "psu1.ch1.v.set=11", "psu1.ch1.out.set=1",
		"psu1.ch1.v.set=12", "psu1.ch1.out.set=1", "psu1.ch1.out.set=0"}
	if strings.Join(r.sets, " ") != strings.Join(want, " ") {
		t.Errorf("sets\n got %v\nwant %v", r.sets, want)
	}
	if len(r.results) != 3 || r.results[2].Level.String() != "12V" || r.results[2].Result != "ok" {
		t.Errorf("results %+v", r.results)
	}
	// Every step that started ended, with an id and a parent.
	starts, ends := 0, 0
	for _, ev := range r.steps {
		if ev.End {
			ends++
		} else {
			starts++
		}
	}
	if starts != ends || starts == 0 {
		t.Errorf("%d steps started, %d ended", starts, ends)
	}

	// A failed check fails the test, and the teardown runs.
	r.reset()
	res = run(t, s, r, "Fails")
	if res.Outcome != procedure.Fail || res.Msg != "no hundred volts here" || res.Pos.Line != 43 {
		t.Errorf("Fails: %+v", res)
	}
	if last := r.sets[len(r.sets)-1]; last != "psu1.ch1.out.set=0" {
		t.Errorf("Fails: teardown did not remove the stress: %v", r.sets)
	}

	// A WHILE that reaches its limit fails.
	r.reset()
	res = run(t, s, r, "Loops")
	if res.Outcome != procedure.Fail || !strings.Contains(res.Msg, "WHILE reached its limit of 3") {
		t.Errorf("Loops: %+v", res)
	}

	// A [Timeout] ends the test as timeout; the teardown still runs.
	r.reset()
	res = run(t, s, r, "Slow")
	if res.Outcome != procedure.Timeout {
		t.Errorf("Slow: %+v", res)
	}
	if e := r.ends(); e[len(e)-1] != "Log ok" || e[len(e)-2] != "Settle timeout" {
		t.Errorf("Slow: steps %v", e)
	}

	// A refusal fails the keyword with refused, and the test stops there.
	r.reset()
	r.refuse["psu1.ch1.v.set"] = true
	res = run(t, s, r, "Refused")
	if res.Outcome != procedure.Refused {
		t.Errorf("Refused: %+v", res)
	}
	if e := r.ends(); strings.Join(e, ",") != "Stress Apply refused,Log ok" {
		t.Errorf("Refused: steps %v", e)
	}
}

// TestRobotParses holds the fixtures to Robot Framework's own parser: the
// subset is Robot syntax, so every procedure this package accepts must parse
// there too. Robot is a test-time dependency only; without it this is skipped.
// KTEST_ROBOT_PYTHON names a Python that has it, python3 by default.
func TestRobotParses(t *testing.T) {
	py := os.Getenv("KTEST_ROBOT_PYTHON")
	if py == "" {
		py = "python3"
	}
	if err := exec.Command(py, "-c", "import robot").Run(); err != nil {
		t.Skipf("no Robot Framework for %s; set KTEST_ROBOT_PYTHON to a Python that has it", py)
	}
	files, err := filepath.Glob("testdata/*.r*")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	if _, err := procedure.Load(filepath.Join("testdata", "sweep.robot"), procedure.Env{Lib: lib.New(newRack()), Channels: newRack()}); err != nil {
		t.Fatalf("the fixtures do not load here: %v", err)
	}
	for _, f := range files {
		out, err := exec.Command(py, "-c", robotCheck, f).CombinedOutput()
		if err != nil {
			t.Errorf("%s: Robot's parser: %v\n%s", f, err, out)
		}
	}
}

// robotCheck parses a file with Robot's own parser and fails on any error
// it reports, in any statement or block.
const robotCheck = `
import sys
from robot.api import get_model, get_resource_model
from robot.api.parsing import ModelVisitor
path = sys.argv[1]
model = (get_resource_model if path.endswith('.resource') else get_model)(path)
errors = []
class V(ModelVisitor):
    def generic_visit(self, node):
        for e in getattr(node, 'errors', ()) or ():
            errors.append('%s:%s: %s' % (path, getattr(node, 'lineno', '?'), e))
        super().generic_visit(node)
V().visit(model)
if errors:
    print('\n'.join(errors))
    sys.exit(1)
`

// TestStop: stopping a run ends the step under way as stopped, and runs the
// teardown.
func TestStop(t *testing.T) {
	r := newRack()
	s := mustLoad(t, r, "p.robot", "*** Test Cases ***\nT\n    [Teardown]    Stress Remove    psu1.ch1\n    Settle    1h\n")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan procedure.Result)
	go func() { done <- s.Run(ctx, context.Background(), "T", r) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case res := <-done:
		if res.Outcome != procedure.Stopped {
			t.Errorf("%+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the run did not stop")
	}
	if e := r.ends(); strings.Join(e, ",") != "Settle stopped,Stress Remove ok" {
		t.Errorf("steps %v", e)
	}
	for _, ev := range r.steps {
		if ev.End && ev.Keyword == "Settle" && ev.Msg != "the run was stopped" {
			t.Errorf("Settle's message: %q", ev.Msg)
		}
	}
}

// TestConditionWithoutValue: a condition reading a channel nobody has heard
// from fails, rather than being taken as false.
func TestConditionWithoutValue(t *testing.T) {
	r := newRack()
	s := mustLoad(t, r, "p.robot", "*** Test Cases ***\nT\n    IF    psu1.ch1.meas.i > 1A\n        Log    x\n    END\n")
	res := run(t, s, r, "T")
	if res.Outcome != procedure.Fail || !strings.Contains(res.Msg, "psu1.ch1.meas.i has no value yet") {
		t.Errorf("%+v", res)
	}
}

// TestKeywordCheckedPerCall: a keyword is checked with what each call passes
// it, so the same body is refused for one caller and not another.
func TestKeywordCheckedPerCall(t *testing.T) {
	kw := "*** Keywords ***\nAbove\n    [Arguments]    ${x}\n    IF    $x > 1V\n        Log    above\n    END\n"
	if _, err := load(t, newRack(), "p.robot", "*** Test Cases ***\nT\n    Above    2V\n"+kw); err != nil {
		t.Errorf("volts: %v", err)
	}
	_, err := load(t, newRack(), "p.robot", "*** Test Cases ***\nT\n    Above    2V\n    Above    2A\n"+kw)
	if err == nil || !strings.Contains(err.Error(), "p.robot:8: condition \"$x > 1V\"") ||
		!strings.Contains(err.Error(), "(called from p.robot:4)") {
		t.Errorf("amps: %v", err)
	}
}

// TestHash: the hash covers every file read, so that a changed byte in a
// resource changes it.
func TestHash(t *testing.T) {
	r := newRack()
	a := mustLoad(t, r, "p.robot", "*** Settings ***\nResource    k.resource\n*** Test Cases ***\nT\n    K\n",
		"k.resource", "*** Keywords ***\nK\n    Log    a\n")
	b := mustLoad(t, r, "p.robot", "*** Settings ***\nResource    k.resource\n*** Test Cases ***\nT\n    K\n",
		"k.resource", "*** Keywords ***\nK\n    Log    b\n")
	if a.Hash == b.Hash {
		t.Error("a changed resource left the hash as it was")
	}
}

func TestShouldBeWithin(t *testing.T) {
	r := newRack()
	_, err := load(t, r, "p.robot", "*** Test Cases ***\nT\n    Should Be Within    12V    12.1V    100mA\n")
	if err == nil || !strings.Contains(err.Error(), "p.robot:3: Should Be Within: actual is in V and tolerance in mA") {
		t.Errorf("units: %v", err)
	}
	s := mustLoad(t, r, "p.robot", "*** Test Cases ***\nNear\n    ${v}=    Read Channel    psu1.ch1.meas.v\n"+
		"    Should Be Within    ${v}    12.1V    200mV\nFar\n    ${v}=    Read Channel    psu1.ch1.meas.v\n"+
		"    Should Be Within    ${v}    13V    0.5V\n")
	if res := run(t, s, r, "Near"); res.Outcome != procedure.OK {
		t.Errorf("Near: %+v", res)
	}
	if res := run(t, s, r, "Far"); res.Outcome != procedure.Fail || res.Msg != "12V is not within 0.5V of 13V" {
		t.Errorf("Far: %+v", res)
	}
}

func TestStressCurrent(t *testing.T) {
	r := newRack()
	s := mustLoad(t, r, "p.robot", "*** Test Cases ***\nT\n    Stress Apply    psu1.ch1    5V    current=1.5A\n    Stress Apply    psu1.ch1    6V\n")
	if res := run(t, s, r, "T"); res.Outcome != procedure.OK {
		t.Fatalf("%+v", res)
	}
	want := "psu1.ch1.i.set=1.5 psu1.ch1.v.set=5 psu1.ch1.out.set=1 psu1.ch1.v.set=6 psu1.ch1.out.set=1"
	if got := strings.Join(r.sets, " "); got != want {
		t.Errorf("sets\n got %s\nwant %s", got, want)
	}
	if _, err := load(t, r, "p.robot", "*** Test Cases ***\nT\n    Stress Apply    psu1.ch1    5V    current=2V\n"); err == nil ||
		!strings.Contains(err.Error(), "current: 2V is in V, and this takes a number in A") {
		t.Errorf("current in volts: %v", err)
	}
}
