//go:build linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trukeio/logb/dbc"

	"github.com/trukeio/ktest/dut"
	"github.com/trukeio/ktest/expr"
	"github.com/trukeio/ktest/monitor"
	"github.com/trukeio/ktest/procedure"
)

const testContract = `{
  "contract": "t", "schema": 1,
  "inputs":   {"Alive": "DutStatus", "Vx": "psu1.ch1.meas.v"},
  "monitors": {"alive": {"ok": "age(Alive) < 30ms"}, "vx": {"ok": "Vx > 9V"}},
  "modes":    {"active": {"action": "enter", "settle": "100ms"}},
  "resource": "actions.robot",
  "actions":  {"enter": "Enter", "reset": "Reset DUT"}
}`

const goodActions = `*** Variables ***
&{RESET}      Request=1
&{PRESENT}    Active=1

*** Keywords ***
Enter
    Start Cyclic    TesterPresent    100ms    ${PRESENT}
    Set Channel    psu1.ch1.v.set    12V

Reset DUT
    Send Message    DutReset    ${RESET}
    Wait For Monitor    alive    timeout=1s
`

// dutRack loads the test contract with actions as its resource, the way the
// daemon does at start: against the streams it will record and the dutsim
// database.
func dutRack(t *testing.T, actions string) (*dutInst, error) {
	t.Helper()
	dir := t.TempDir()
	cp := filepath.Join(dir, "dut-interface.json")
	if err := os.WriteFile(cp, []byte(testContract), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "actions.robot"), []byte(actions), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := dbc.Parse(bytes.NewReader(dut.DBC))
	if err != nil {
		t.Fatal(err)
	}
	sup := supplyFlags{{name: "psu1", addr: "x", channels: 2}}
	ct, err := contractChannels("vcan0", db, sup, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	c, err := monitor.Load(cp, ct, procedure.CheckActions)
	if err != nil {
		t.Fatal(err)
	}
	d := &daemon{bus: "vcan0", db: db, supplies: map[string]int{"psu1": 2}, scopes: map[string]int{}}
	return newDutInst(d, c, ct)
}

// The resource is checked whole at start, with the DUT library: a mistake
// refuses the start rather than the day an action is first asked for.
func TestDutResource(t *testing.T) {
	di, err := dutRack(t, goodActions)
	if err != nil {
		t.Fatalf("a good resource was refused: %v", err)
	}
	if di.suite == nil || len(di.c.Actions) != 2 {
		t.Fatalf("suite %v, actions %v", di.suite, di.c.Actions)
	}
	for _, c := range []struct {
		name, line, want string
	}{
		{"an action that takes arguments", "Reset DUT\n    [Arguments]    ${n}\n    Log    ${n}\n", "an action is called with no arguments"},
		{"a message the database lacks", "Reset DUT\n    Send Message    Nonesuch\n", "no message \"Nonesuch\""},
		{"a signal the message lacks", "Reset DUT\n    Send Message    DutReset    ${BAD}\n", "has no signal \"Nonesuch\""},
		{"a signal value out of range", "Reset DUT\n    Send Message    DutReset    ${BIG}\n", "outside the range"},
		{"a monitor the contract lacks", "Reset DUT\n    Wait For Monitor    speed\n", "no monitor \"speed\""},
		{"a state that is no state", "Reset DUT\n    Wait For Monitor    alive    state=fine\n", "not a monitor state"},
		{"a channel this rack cannot set", "Reset DUT\n    Set Channel    psu9.ch1.v.set    1V\n", "no channel"},
		{"a keyword outside the DUT library", "Reset DUT\n    Stress Apply    psu1.ch1    12V\n", "Stress Apply"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := "*** Variables ***\n&{BAD}    Nonesuch=1\n&{BIG}    Request=300\n\n*** Keywords ***\nEnter\n    Log    in\n\n" + c.line
			_, err := dutRack(t, res)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want a refusal naming %q", err, c.want)
			}
		})
	}
}

// A signal's value is read in the signal's own unit: converted when written
// in another unit of its dimension, taken as written when bare or in the
// signal's own unit, refused when its dimension is another.
func TestDutSignals(t *testing.T) {
	m := &dbc.Message{Name: "M", Signals: []*dbc.Signal{{Name: "Volt", Unit: "V"}, {Name: "Speed", Unit: "rpm"},
		{Name: "Req"}, {Name: "Mode"}}}
	num := func(s string) procedure.Value {
		f, u, err := expr.ParseQuantity(s)
		if err != nil {
			t.Fatal(err)
		}
		return procedure.Number(f, u)
	}
	got, err := signals(m, map[string]procedure.Value{
		"Volt": num("500mV"), "Speed": procedure.Str("3000rpm"), "Req": procedure.Str("1"),
		"Mode": procedure.Str("Active"),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"Volt": 0.5, "Speed": 3000.0, "Req": 1.0, "Mode": "Active"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %v (%T), want %v", k, got[k], got[k], v)
		}
	}
	if _, err := signals(m, map[string]procedure.Value{"Volt": num("2A")}); err == nil ||
		!strings.Contains(err.Error(), `is in "V"`) {
		t.Errorf("amps for a volt signal: %v", err)
	}
	if _, err := signals(m, map[string]procedure.Value{"Nope": procedure.Str("1")}); err == nil {
		t.Error("a signal the message lacks was taken")
	}
}
