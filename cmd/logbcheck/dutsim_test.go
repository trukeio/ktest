package main

import (
	"strings"
	"testing"
	"time"
)

// The DUT check on verdicts and a transcript made up for the purpose: one
// dip of each kind, then one fault planted at a time.
func TestDutsimCheck(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	ev := func(ms int, what string) dutEvent { return dutEvent{at(ms), what} }
	evs := []dutEvent{
		ev(0, "supply ok 13.5V"), ev(0, "state booting"), ev(100, "state running"),
		// 1 s: a dip that stays above the threshold says nothing; A.
		// 2 s: a glitch; B.
		ev(2000, "supply low 8V"), ev(2000, "state glitched"), ev(2300, "supply ok 13.5V"), ev(2300, "state running"),
		// 3 s: a stop it came back from by itself; C.
		ev(3000, "supply low 6V"), ev(3000, "state stopped"), ev(3300, "supply ok 13.5V"), ev(3300, "state booting"),
		ev(3400, "state running"),
		// 4 s: a hang, reset; D.
		ev(4000, "supply low 6V"), ev(4000, "state hung"), ev(4300, "supply ok 13.5V"), ev(4500, "reset"),
		ev(4500, "state booting"), ev(4600, "state running"),
		// 5 s: a hang the reset did not end; E.
		ev(5000, "supply low 3V"), ev(5000, "state dead"), ev(5300, "supply ok 13.5V"), ev(5500, "reset ignored: dead"),
	}
	good := func() []verdict {
		return []verdict{
			{at(1900), "dip", "12V", "A"}, {at(2900), "dip", "8V", "B"}, {at(3900), "dip", "6V", "C"},
			{at(4900), "dip", "6V", "D"}, {at(5900), "dip", "3V", "E"},
		}
	}
	if n, err := compareVerdicts(good(), evs, true); err != nil || n != 5 {
		t.Fatalf("verdicts that agree: %d, %v", n, err)
	}
	for _, c := range []struct {
		name   string
		mutate func([]verdict)
		want   string
	}{
		{"a glitch called a stop", func(v []verdict) { v[1].result = "C" }, "recorded C, and dutsim's transcript says B"},
		{"a reset recovery called automatic", func(v []verdict) { v[3].result = "C" }, "says D"},
		{"a lost DUT called recovered", func(v []verdict) { v[4].result = "D" }, "says E"},
		{"a dip called no deviation", func(v []verdict) { v[2].result = "A" }, "says C"},
		{"a level passed that was a dip", func(v []verdict) { v[1].result = "ok" }, "says nok"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := good()
			c.mutate(v)
			if _, err := compareVerdicts(v, evs, true); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
}
