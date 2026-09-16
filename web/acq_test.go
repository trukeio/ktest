//go:build linux

package web

// The scope widget draws an acquisition against the trigger, and where the
// trigger is comes out of arithmetic that lives in the worker and nowhere else:
// the daemon places an acquisition at a userspace stamp taken at readout and
// writes the envelope's axis relative to that, and the worker subtracts the
// run's own "first" to put the trigger back at zero. A mistake there draws the
// trace half a screen out, with nothing to say so.
//
// So the worker's path is run over the scope fixture and the result checked
// against what the dialect's rule says it must be: fourteen divisions across,
// the trigger half way, and every bucket either a value with samples in it or
// absent with none.

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

type acqLine struct {
	S           string  `json:"s"`
	Run         int     `json:"run"`
	Buckets     int     `json:"buckets"`
	Empty       int     `json:"empty"`
	First       float64 `json:"first"`
	Last        float64 `json:"last"`
	Span        float64 `json:"span"`
	TriggerFrac float64 `json:"triggerFrac"`
	Min         float64 `json:"min"`
	Max         float64 `json:"max"`
	TDiv        string  `json:"tdiv"`
	VDiv        string  `json:"vdiv"`
}

func TestWorkerPlacesTheTrigger(t *testing.T) {
	node := needNode(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "scope.logb")
	if err := os.WriteFile(path, writeScopeFixture(t), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "--experimental-strip-types", "--no-warnings", "test/acq.ts", path)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}

	var got []acqLine
	for _, line := range splitLines(string(out)) {
		var a acqLine
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		got = append(got, a)
	}
	if len(got) != 2 {
		t.Fatalf("%d acquisitions, want the fixture's 2", len(got))
	}

	// The fixture's two acquisitions: 1 ms/div then 2 ms/div, each 20 buckets
	// over its own span, the first with two buckets that held no sample.
	for i, want := range []struct {
		run     int
		first   float64
		empty   int
		buckets int
	}{
		{1, -35e-6, 2, 35},
		{2, -70e-6, 0, 35},
	} {
		a := got[i]
		if a.Run != want.run || a.Buckets != want.buckets {
			t.Errorf("acquisition %d: run %d, %d buckets; want run %d and %d",
				i+1, a.Run, a.Buckets, want.run, want.buckets)
		}
		if math.Abs(a.First-want.first) > 1e-12 {
			t.Errorf("acquisition %d: sample 0 at %g s from the trigger, want %g", i+1, a.First, want.first)
		}
		// Half the screen is before the trigger. Bucket centres are symmetric
		// about it, so this is a half; anything near the edges is a trace
		// drawn in the wrong place, which is the failure the test exists for.
		if math.Abs(a.TriggerFrac-0.5) > 0.02 {
			t.Errorf("acquisition %d: the trigger is %.1f%% across, want about half",
				i+1, 100*a.TriggerFrac)
		}
		if a.Empty != want.empty {
			t.Errorf("acquisition %d: %d buckets held no sample, want %d", i+1, a.Empty, want.empty)
		}
		// The fixture's envelope is a band about a sine, so the extremes are
		// within a volt or so of ±1 either way — enough to catch a widget
		// handed min and max the wrong way round, or a zero where a bucket was
		// absent.
		if a.Min > -1 || a.Max < 1 || a.Max <= a.Min {
			t.Errorf("acquisition %d: the band runs %g to %g", i+1, a.Min, a.Max)
		}
	}
	t.Logf("2 acquisitions drawn against the trigger: %g s and %g s of screen, the trigger half way across each",
		got[0].Span, got[1].Span)
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
