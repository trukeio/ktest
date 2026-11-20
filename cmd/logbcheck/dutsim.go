package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/dut"
)

// The DUT check (-dutsim) holds a lab rack's verdicts on a DUT to the DUT's
// own account of itself: dutsim's transcript, which says when its supply went
// low, what it did then, whether it was reset and when it ran again — written
// by nothing that writes Logb, and by nothing that judged the DUT.
//
// Each step result a procedure recorded (procedure.result) is held to what the
// transcript says happened since the result before it:
//
//   - a class (handoff C10): A, no dip below the DUT's threshold; B, a glitch;
//     C, a stop it came back from by itself; D, a stop it came back from after
//     a reset; E, a stop it did not come back from;
//   - ok and nok, as Threshold Search records levels: no dip, and a dip;
//   - lost, a dip it did not come back from, reset or not.
//
// A lab rack testing several DUTs records each one's results at points of
// its own, and each is held to its own transcript with -dutsim
// point=transcript.
//
// The lab rack's clock and dutsim's are the same machine's, as in
// examples/two-racks; a recording made against a DUT on another machine
// needs the clocks' offset first.

// verdict is one step result.
type verdict struct {
	t      time.Time
	point  string
	level  string
	result string
}

// dutEvent is one line of dutsim's transcript.
type dutEvent struct {
	t    time.Time
	what string
}

func readVerdicts(path string) ([]verdict, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, err := logb.NewReader(f)
	if err != nil {
		return nil, err
	}
	epoch := int64(-1)
	var out []verdict
	for {
		b, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if epoch < 0 {
			for _, m := range r.Meta {
				if m.Key == "recording.epoch" {
					epoch, _ = strconv.ParseInt(m.Value, 10, 64)
				}
			}
			if epoch < 0 {
				return nil, errors.New("the recording does not state recording.epoch, so its times cannot be put beside a transcript's")
			}
		}
		if b.Schema.Name != "procedure.result" {
			continue
		}
		idx, err := fieldIndex(b.Schema, "point", "level_text", "result")
		if err != nil {
			return nil, err
		}
		for i := 0; i < int(b.Count); i++ {
			ax, err := b.Axis(i)
			if err != nil {
				return nil, err
			}
			d, err := ax.Duration(b.Schema.AxisExp)
			if err != nil {
				return nil, err
			}
			text := func(f string) string { v, _ := b.Value(i, idx[f]); s, _ := v.(string); return s }
			out = append(out, verdict{t: time.Unix(0, epoch+int64(d)), point: text("point"),
				level: text("level_text"), result: text("result")})
		}
	}
	slices.SortStableFunc(out, func(a, b verdict) int { return a.t.Compare(b.t) })
	return out, nil
}

func readDutsim(path string) ([]dutEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []dutEvent
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		stamp, what, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		t, err := time.Parse(dut.TimeFormat, stamp)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %v", path, n, err)
		}
		out = append(out, dutEvent{t, what})
	}
	return out, sc.Err()
}

// expected is what the transcript says between two step results.
func expected(evs []dutEvent) (class string, dipped bool) {
	has := func(what string) bool {
		return slices.ContainsFunc(evs, func(e dutEvent) bool { return e.what == what })
	}
	dipped = slices.ContainsFunc(evs, func(e dutEvent) bool { return strings.HasPrefix(e.what, "supply low") })
	switch {
	case !dipped:
		return "A", false
	case has("state glitched"):
		return "B", true
	case has("state stopped"):
		return "C", true
	case has("state hung"):
		// Running again after a reset is D; still hung, or dead, is E.
		i := slices.IndexFunc(evs, func(e dutEvent) bool { return e.what == "reset" })
		if i >= 0 && slices.ContainsFunc(evs[i:], func(e dutEvent) bool { return e.what == "state running" }) {
			return "D", true
		}
		return "E", true
	}
	return "E", true // dead, or a dip during a boot the transcript did not finish
}

// compareDutsim holds the recording's verdicts to the transcript.
func compareDutsim(recPath, trPath, point string, verbose bool) (int, error) {
	all, err := readVerdicts(recPath)
	if err != nil {
		return 0, err
	}
	// With several DUTs, each one's results are recorded at points of its
	// own, and are held to its own transcript, each since its own before.
	var vs []verdict
	for _, v := range all {
		if point == "" || v.point == point {
			vs = append(vs, v)
		}
	}
	evs, err := readDutsim(trPath)
	if err != nil {
		return 0, err
	}
	if len(vs) == 0 {
		return 0, errors.New("the recording has no step results to hold to the transcript")
	}
	return compareVerdicts(vs, evs, verbose)
}

// compareVerdicts holds each verdict to the transcript's events since the
// verdict before it.
func compareVerdicts(vs []verdict, evs []dutEvent, verbose bool) (int, error) {
	var bad []string
	report := func(format string, a ...any) {
		if len(bad) < 10 || verbose {
			bad = append(bad, fmt.Sprintf(format, a...))
		}
	}
	n := 0
	prev := time.Time{}
	for _, v := range vs {
		var window []dutEvent
		for _, e := range evs {
			if e.t.After(prev) && !e.t.After(v.t) {
				window = append(window, e)
			}
		}
		prev = v.t
		class, dipped := expected(window)
		want := ""
		switch {
		case slices.Contains([]string{"A", "B", "C", "D", "E"}, v.result):
			want = class
		case v.result == "ok", v.result == "nok":
			want = "ok"
			if dipped {
				want = "nok"
			}
		case v.result == "lost":
			if class != "E" {
				want = class + " (it came back)"
			} else {
				want = "lost"
			}
		default:
			continue // a threshold, or a result this check has no account of
		}
		n++
		if v.result != want {
			report("%s at %s, %s: the procedure recorded %s, and dutsim's transcript says %s",
				v.point, v.level, v.t.Format("15:04:05.000"), v.result, want)
		}
	}
	if len(bad) > 0 {
		return n, errors.New(strings.Join(bad, "\n"))
	}
	return n, nil
}
