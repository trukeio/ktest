package secondary

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/channel"
	"github.com/trukeio/ktest/peer"
	"github.com/trukeio/ktest/record"
)

// A host as Core needs one: a writer that keeps what it is given, and a
// goroutine — the test's — that Do queues work for.
type fakeHost struct {
	schemas map[any]*logb.Schema
	lines   []string
	jobs    chan func()
}

func (h *fakeHost) NewStream(mk func() *logb.Schema) (any, error) {
	s := mk()
	h.schemas[s] = s
	return s, nil
}

func (h *fakeHost) Write(st any, t time.Time, fill func(rec []byte)) error {
	s := st.(*logb.Schema)
	rec := make([]byte, s.RecordBytes())
	fill(rec)
	it := peer.Item{Schema: s, Rec: rec}
	switch {
	case s.Name == "mode":
		h.lines = append(h.lines, fmt.Sprintf("mode %v %q %q %s", peer.Num(&it, "event"), peer.Str(&it, "requested"),
			peer.Str(&it, "applied"), peer.Str(&it, "message")))
	case strings.HasPrefix(s.Name, "dut.action."):
		h.lines = append(h.lines, fmt.Sprintf("%s %v %v %s", s.Name, peer.Num(&it, "event"), peer.Num(&it, "outcome"),
			peer.Str(&it, "message")))
	}
	return nil
}

// drain runs what Do queued until the wanted number of lines is written.
func (h *fakeHost) drain(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for len(h.lines) < n {
		select {
		case f := <-h.jobs:
			f()
		case <-deadline:
			t.Fatalf("%d lines, want %d: %q", len(h.lines), n, h.lines)
		}
	}
}

func TestCoreModesAndActions(t *testing.T) {
	dir := t.TempDir()
	cp := filepath.Join(dir, "dut-interface.json")
	must(t, os.WriteFile(cp, []byte(`{"contract": "c", "schema": 1,
  "inputs": {"Vx": {"channel": "Supply.v", "max_age": "1s"}},
  "monitors": {"abort": {"ok": "Vx < 16V"}},
  "modes": {"active": {"action": "enter", "settle": "10ms"}},
  "resource": "actions.txt", "actions": {"enter": "enter", "hang": "hang"}}`), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "actions.txt"), []byte("enter\nhang\n"), 0o644))
	table := channel.NewTable()
	table.Stream(record.Schema("Supply", 8*16, []logb.Field{{Name: "v", BitOffset: 64, BitWidth: 64, Type: logb.TypeFloat, Unit: "V"}}, nil))

	h := &fakeHost{schemas: map[any]*logb.Schema{}, jobs: make(chan func(), 16)}
	failEntry := true
	var infos []ActionInfo
	core, err := NewCore(CoreConfig{Contract: cp, Channels: table, Window: 0, WatermarkEvery: 10 * time.Millisecond, Queue: 16,
		Actions: ActionFunc(func(ctx context.Context, a string) error {
			info, _ := ActionInfoFrom(ctx)
			infos = append(infos, info) // read by the test after the end comes back through Do
			if a == "hang" {
				<-ctx.Done()
				return ctx.Err()
			}
			if failEntry {
				return errors.New("the DUT did not answer")
			}
			return nil
		}),
		Do: func(f func()) { h.jobs <- f }})
	must(t, err)
	t0 := time.Unix(1_795_000_000, 0)
	must(t, core.Declare(h, t0))
	go core.Run()
	by := By{Name: "alice", Via: record.ViaTLS}

	// An entry action that fails: the mode is not applied, and says why.
	core.SetMode(7, by, "active", t0)
	if a, busy := core.Busy(); !busy || a != "enter" {
		t.Errorf("busy %q %v while the entry action runs", a, busy)
	}
	h.drain(t, 5)
	if want := []string{
		`mode 0 "" "" `,
		`mode 1 "active" "" `,
		`dut.action.enter 1 0 `,
		`dut.action.enter 2 6 the DUT did not answer`,
		`mode 3 "active" "" entry action enter: fail: the DUT did not answer`,
	}; strings.Join(h.lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("records:\n%s\nwant:\n%s", strings.Join(h.lines, "\n"), strings.Join(want, "\n"))
	}
	if _, busy := core.Busy(); busy || core.Applied() != "" {
		t.Errorf("after a failed entry: busy %v, applied %q", busy, core.Applied())
	}

	// One that succeeds: applied.
	failEntry = false
	core.SetMode(8, by, "active", t0.Add(time.Second))
	h.drain(t, 9)
	if core.Applied() != "active" || !strings.HasPrefix(h.lines[8], `mode 2 "active" "active"`) {
		t.Errorf("applied %q, last record %s", core.Applied(), h.lines[8])
	}

	// An action stopped: its outcome is stopped, with why.
	core.Invoke(9, by, "hang", t0.Add(2*time.Second))
	core.Stop("the DUT was disarmed")
	h.drain(t, 11)
	if got := h.lines[10]; got != "dut.action.hang 2 7 the DUT was disarmed" {
		t.Errorf("a stopped action ended %s", got)
	}
	if len(infos) != 3 || infos[2] != (ActionInfo{Run: 3, Seq: 9, By: by, Action: "hang"}) ||
		infos[0].Mode != "active" || infos[0].Run != 1 {
		t.Errorf("what the actions were told: %+v", infos)
	}
	if body := core.Info(map[string]string{"host.name": "x"}); body["protocol"] != peer.Protocol || body["contract"] != "c" {
		t.Errorf("info %v", body)
	}
	core.Close()
	for range core.Outputs() {
	}
}
