package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rveen/ktest/dut"
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

// The second fixture is ktestd recording vcan0 while dutsim ran on it with
// -behaviour restore, powered from psusim channel 1, and dutsim-dip.robot
// dipped that supply to 6 V for 300 ms (testdata/dutsim-dip.*). dutsim's
// transcript is its own account of the run, written by nothing that writes
// Logb, and alive's transitions are held to it rather than to themselves:
// alive goes dev within 30 ms (its age limit) and a frame period of the DUT
// stopping, and ok within three frame periods of it running again: its first
// frame waits for dutsim's next tick, and delta() has no value until the
// second frame.
func TestDutsimFixture(t *testing.T) {
	r, err := check("testdata/dutsim.json", "testdata/dutsim-dip.logb", "mode.applied")
	if err != nil {
		t.Fatal(err)
	}
	epoch := recordingEpoch(t, "testdata/dutsim-dip.logb")
	stopped, running := dutStates(t, "testdata/dutsim-dip.txt", epoch)
	var dev, ok []int64
	for _, x := range r.tr {
		if x.Monitor != "alive" {
			continue
		}
		switch string(x.To) {
		case "dev":
			dev = append(dev, x.T)
		case "ok":
			ok = append(ok, x.T)
		}
	}
	const period = int64(10 * time.Millisecond)
	within := func(what string, got, want []int64, lo, hi int64) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("alive went %s %d times, and the DUT's transcript says %d", what, len(got), len(want))
		}
		for i := range got {
			if d := got[i] - want[i]; d < lo || d > hi {
				t.Errorf("alive %s %d: %v after the transcript's event, want %v to %v",
					what, i, time.Duration(d), time.Duration(lo), time.Duration(hi))
			}
		}
	}
	within("dev", dev, stopped, int64(30*time.Millisecond)-period, int64(30*time.Millisecond)+period)
	within("ok", ok, running, 0, 3*period)
}

// recordingEpoch is the wall time ktestd's recording counts its axis from. The
// recorder begins its first segment at its epoch and stamps the sync frame
// with it, so it is the first sync frame's wall time: the 16-byte sync
// pattern, then the segment's sequence number and its wall time, both u64
// little-endian.
func recordingEpoch(t *testing.T, path string) int64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(b, []byte("LOGBSYNC"))
	if i < 0 || len(b) < i+32 {
		t.Fatalf("%s has no sync frame", path)
	}
	return int64(binary.LittleEndian.Uint64(b[i+24:]))
}

// dutStates reads the times dutsim stopped and started running, in ns on the
// recording's axis.
func dutStates(t *testing.T, path string, epoch int64) (stopped, running []int64) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		stamp, what, _ := strings.Cut(line, " ")
		at, err := time.Parse(dut.TimeFormat, stamp)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		switch what {
		case "state stopped":
			stopped = append(stopped, at.UnixNano()-epoch)
		case "state running":
			running = append(running, at.UnixNano()-epoch)
		}
	}
	return stopped, running
}

// The third fixture is ktestd running dutsim.json live, as a DUT rack, over
// the same dip as the second (testdata/rack-live.logb). The rack recorded
// its monitors' transitions and watermark; a replay of the records it read
// gives the same transitions, to the nanosecond, which is what the purity of
// §4.3 promises, held here across the whole online path: tap, reorder window,
// evaluator and loop.
func TestLiveIsReplay(t *testing.T) {
	r, err := check("testdata/dutsim.json", "testdata/rack-live.logb", "mode.applied")
	if err != nil {
		t.Fatal(err)
	}
	if !r.haveLive || r.liveWM == 0 || r.firstGap != math.MaxInt64 {
		t.Fatalf("live %v, watermark %d, first gap %d: want a gapless recording with a watermark", r.haveLive, r.liveWM, r.firstGap)
	}
	if fails := compareLive(r); len(fails) > 0 {
		t.Fatalf("the rack and the replay disagree:\n%s", strings.Join(fails, "\n"))
	}
	n := 0
	for _, x := range r.live {
		if x.T <= r.liveWM {
			n++
		}
	}
	if n < 10 {
		t.Fatalf("only %d live transitions compared", n)
	}
	// One nanosecond out is a disagreement.
	r.live[len(r.live)/2].T++
	if fails := compareLive(r); len(fails) == 0 {
		t.Fatal("a transition moved by a nanosecond went unnoticed")
	}
	// A recording made by no rack has nothing to compare.
	plain, err := check("testdata/dutsim.json", "testdata/dutsim-dip.logb", "mode.applied")
	if err != nil {
		t.Fatal(err)
	}
	if fails := compareLive(plain); len(fails) != 1 || !strings.Contains(fails[0], "no mon.* streams") {
		t.Fatalf("got %v", fails)
	}
}
