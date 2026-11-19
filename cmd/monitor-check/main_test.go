package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The fixture is a recording of examples/supply-bench running its ramp
// procedure four times (2, 4, 6, 8 V; the last two runs stopped part way),
// written by ktestd against psusim. supply-bench.json watches it.

func replay(t *testing.T) *result {
	t.Helper()
	r, err := check("testdata/supply-bench.json", "testdata/supply-ramp.logb", "mode.applied")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFixture(t *testing.T) {
	r := replay(t)
	var got []string
	for _, x := range r.tr {
		got = append(got, fmt.Sprintf("%.3f %s %s>%s", r.rel(x.T), x.Monitor, x.From, x.To))
	}
	// below7 deviates each time the ramp reaches 8 V, 50 ms after the first
	// reading above 7 V; tracks never does, since the supply measures what it
	// was set to; fresh is ok from the first measurement on.
	want := []string{
		"0.102 fresh unknown>ok",
		"0.204 tracks unknown>ok",
		"0.252 below7 unknown>ok",
		"2.552 below7 ok>dev",
		"2.853 below7 dev>ok",
		"5.152 below7 ok>dev",
		"5.452 below7 dev>ok",
		"7.653 below7 ok>dev",
		"7.752 below7 dev>ok",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("transitions\n got %s\nwant %s", strings.Join(got, "\n     "), strings.Join(want, "\n     "))
	}
	// No mode channel in this recording, and the contract has no modes:
	// nothing to say about it.
	if len(r.notes) != 0 {
		t.Errorf("notes %v", r.notes)
	}
}

// TestReplayIsDeterministic: two replays of one recording agree to the
// nanosecond.
func TestReplayIsDeterministic(t *testing.T) {
	a, b := replay(t), replay(t)
	if fmt.Sprint(a.tr) != fmt.Sprint(b.tr) {
		t.Error("two replays differ")
	}
}

func TestExpect(t *testing.T) {
	r := replay(t)
	tol := 50 * time.Millisecond
	for _, c := range []struct {
		expect string
		fails  []string
	}{
		{"dev@2.55,5.15,7.65", nil},
		{"dev@2.55s, 5.15s, 7.65s", nil},
		{"dev@2.55,5.15", []string{"below7 deviates at +7.652605s (below7: debounce), which -expect does not list"}},
		{"dev@2.55,5.15,7.65,9", []string{"no deviation begins within 50ms of 9s"}},
		{"dev@2.4,5.15,7.65", []string{"no deviation begins within 50ms of 2.4s",
			"below7 deviates at +2.552452s (below7: debounce), which -expect does not list"}},
		{"ok", []string{"below7 deviates at +2.552452s", "below7 deviates at +5.152406s", "below7 deviates at +7.652605s"}},
	} {
		e, err := parseExpect(c.expect)
		if err != nil {
			t.Fatal(err)
		}
		fails := e.judge(r, tol)
		if len(fails) != len(c.fails) {
			t.Errorf("%s: %q", c.expect, fails)
			continue
		}
		for i := range fails {
			if !strings.Contains(fails[i], c.fails[i]) {
				t.Errorf("%s: %q, want %q", c.expect, fails[i], c.fails[i])
			}
		}
	}
	for _, bad := range []string{"dev", "dev@x", "fine"} {
		if _, err := parseExpect(bad); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}

func TestJSON(t *testing.T) {
	var b bytes.Buffer
	writeJSON(&b, replay(t))
	var out struct {
		Contract    string
		Transitions []struct {
			T       float64
			Monitor string
			To      string
		}
	}
	if err := json.Unmarshal(b.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Contract != "supply-bench-demo" || len(out.Transitions) != 9 || out.Transitions[3].To != "dev" {
		t.Errorf("%+v", out)
	}
}

func TestModeNote(t *testing.T) {
	// A contract with modes over a recording with no mode channel says so.
	r, err := check("testdata/with-modes.json", "testdata/supply-ramp.logb", "mode.applied")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.notes) != 1 || !strings.Contains(r.notes[0], "no text channel mode.applied") {
		t.Errorf("notes %v", r.notes)
	}
	for _, x := range r.tr {
		if x.Monitor == "active" && x.To != "inactive" {
			t.Errorf("a monitor of a mode never entered: %v", x)
		}
	}
}
