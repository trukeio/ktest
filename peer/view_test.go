//go:build linux

package peer

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/contract"
	"github.com/trukeio/ktest/record"
)

// Records as a DUT rack streams them, laid out as doc/peer-protocol.md §7.4
// says, the state's codes given as text the way ktestd's schemas give them.

var states = logb.ValueToText{Keys: []float64{0, 1, 2, 3, 4}, Texts: stateCodes, Default: "?"}

func monSchema(name string, conv bool) *logb.Schema {
	f := logb.Field{Name: "state", BitOffset: 64, BitWidth: 8, Type: logb.TypeUint, Hold: true}
	if conv {
		f.Conv = states
	}
	return record.Schema("mon."+name, 8*80, []logb.Field{f,
		{Name: "from", BitOffset: 72, BitWidth: 8, Type: logb.TypeUint},
		{Name: "cause", BitOffset: 128, BitWidth: 8 * 64, Type: logb.TypeString},
	}, nil)
}

var (
	wmSchema   = record.Schema("mon.watermark", 8*16, []logb.Field{{Name: "upto", BitOffset: 64, BitWidth: 64, Type: logb.TypeSint, Hold: true}}, nil)
	modeSchema = record.Schema("mode", 8*272, []logb.Field{
		{Name: "seq", BitOffset: 64, BitWidth: 32, Type: logb.TypeUint},
		{Name: "event", BitOffset: 96, BitWidth: 8, Type: logb.TypeUint},
		{Name: "requested", BitOffset: 128, BitWidth: 256, Type: logb.TypeString, Hold: true},
		{Name: "applied", BitOffset: 384, BitWidth: 256, Type: logb.TypeString, Hold: true},
		{Name: "by", BitOffset: 640, BitWidth: 256, Type: logb.TypeString},
		{Name: "message", BitOffset: 896, BitWidth: 1280, Type: logb.TypeString},
	}, nil)
	actionSchema = record.Schema("dut.action.recover", 8*248, []logb.Field{
		{Name: "seq", BitOffset: 64, BitWidth: 32, Type: logb.TypeUint},
		{Name: "run", BitOffset: 96, BitWidth: 32, Type: logb.TypeUint},
		{Name: "event", BitOffset: 128, BitWidth: 8, Type: logb.TypeUint},
		{Name: "outcome", BitOffset: 136, BitWidth: 8, Type: logb.TypeUint},
		{Name: "message", BitOffset: 704, BitWidth: 1280, Type: logb.TypeString},
	}, nil)
)

func ms(n int) int64 { return int64(n) * int64(time.Millisecond) }

func monItem(s *logb.Schema, t int64, state byte) *Item {
	rec := make([]byte, s.RecordBytes())
	rec[8] = state
	return &Item{Schema: s, Rec: rec, PeerT: t, Recv: time.Now()}
}

func wmItem(t int64) *Item {
	rec := make([]byte, 16)
	binary.LittleEndian.PutUint64(rec[8:], uint64(t))
	return &Item{Schema: wmSchema, Rec: rec, PeerT: t, Recv: time.Now()}
}

func modeItem(t int64, seq uint32, ev byte, applied, msg string) *Item {
	rec := make([]byte, modeSchema.RecordBytes())
	binary.LittleEndian.PutUint32(rec[8:], seq)
	rec[12] = ev
	copy(rec[48:80], applied)
	copy(rec[112:], msg)
	return &Item{Schema: modeSchema, Rec: rec, PeerT: t, Recv: time.Now()}
}

func actionItem(t int64, seq uint32, ev, outcome byte, msg string) *Item {
	rec := make([]byte, actionSchema.RecordBytes())
	binary.LittleEndian.PutUint32(rec[8:], seq)
	rec[16], rec[17] = ev, outcome
	copy(rec[88:], msg)
	return &Item{Schema: actionSchema, Rec: rec, PeerT: t, Recv: time.Now()}
}

// The view's account of a monitor, and the window judgement of §9.5: the
// state held at the window's start counts, unknown counts as dev, and a
// restatement adds nothing.
func TestViewDeviated(t *testing.T) {
	v := NewView()
	v.SetLink(true)
	alive, speed := monSchema("alive", true), monSchema("speed", false) // speed's codes as bare numbers
	for _, it := range []*Item{
		monItem(alive, ms(0), 0), monItem(speed, ms(0), 0),
		monItem(alive, ms(100), 1), monItem(alive, ms(100), 1), // dev, then restated
		monItem(alive, ms(150), 0),
		monItem(speed, ms(300), 2), // unknown
		wmItem(ms(400)),
	} {
		v.Observe(it)
	}
	for _, tc := range []struct {
		p0, p1 int
		want   string
	}{
		{10, 90, "map[]"},
		{10, 100, "map[alive:dev]"},
		{120, 140, "map[alive:dev]"}, // dev held from before the window
		{150, 290, "map[]"},
		{200, 300, "map[speed:unknown]"},
	} {
		if got := fmt.Sprint(v.Deviated([]string{"alive", "speed"}, ms(tc.p0), ms(tc.p1))); got != tc.want {
			t.Errorf("[%d, %d] ms: %s, want %s", tc.p0, tc.p1, got, tc.want)
		}
	}
	if n := len(v.hist["alive"]); n != 3 {
		t.Errorf("alive has %d stamps; a restatement is not one", n)
	}
	if got := v.StateAt("never", ms(1)); got != StateUnknown {
		t.Errorf("a monitor never streamed is %s", got)
	}
	if got := fmt.Sprint(v.States([]string{"alive", "speed"})); got != "map[alive:ok speed:unknown]" {
		t.Errorf("states at the watermark: %s", got)
	}
	v.SetLink(false)
	if wm, _ := v.Watermark(); wm != 0 {
		t.Errorf("a lost link keeps watermark %d", wm)
	}
}

// The guard: monitoring is lost with the link, before the first watermark
// and when the watermark ages; abort holds while the monitor is dev.
func TestGuard(t *testing.T) {
	g := Guard{WatermarkAge: 100 * time.Millisecond, Abort: "abort"}
	v := NewView()
	now := time.Unix(1_763_000_000, 0)
	if r := g.Check(v, now); !r.MonitoringLost || !strings.Contains(r.LostWhy, "not connected") {
		t.Errorf("down: %+v", r)
	}
	v.SetLink(true)
	if r := g.Check(v, now); !r.MonitoringLost || !strings.Contains(r.LostWhy, "no watermark") {
		t.Errorf("no watermark: %+v", r)
	}
	// The DUT rack's clock runs 1 s ahead of this one.
	v.SetClock(Estimate{Offset: time.Second, Uncertainty: time.Millisecond})
	v.Observe(wmItem(now.Add(time.Second - 50*time.Millisecond).UnixNano()))
	if r := g.Check(v, now); r.MonitoringLost || r.DUTAbort {
		t.Errorf("a watermark 50 ms old: %+v", r)
	}
	if r := g.Check(v, now.Add(60*time.Millisecond)); !r.MonitoringLost || !strings.Contains(r.LostWhy, "110ms old") {
		t.Errorf("a watermark 110 ms old: %+v", r)
	}
	abort := monSchema("abort", true)
	v.Observe(monItem(abort, now.UnixNano(), 1))
	if r := g.Check(v, now); !r.DUTAbort {
		t.Errorf("abort dev: %+v", r)
	}
	v.Observe(monItem(abort, now.Add(time.Millisecond).UnixNano(), 0))
	if r := g.Check(v, now); r.DUTAbort {
		t.Errorf("abort ok again: %+v", r)
	}
}

// A fake DUT rack: it accepts every mode and action with the next number,
// and the test streams what follows into the view by hand.
type fakeRack struct {
	mu   sync.Mutex
	seq  uint32
	seen []string
}

func (f *fakeRack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Ktest-Lease") != "L" {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":"no lease"}`)
		return
	}
	f.seq++
	f.seen = append(f.seen, r.Method+" "+r.URL.Path)
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"seq":%d}`, f.seq)
}

func testDUT(t *testing.T, lease string) (*DUT, *fakeRack) {
	f := &fakeRack{}
	c := rack(t, f)
	v := NewView()
	v.SetLink(true)
	id := &contract.Identity{Name: "c", Modes: []string{"active"}, Actions: []string{"recover"},
		Monitors: []string{"alive"}, Settle: map[string]time.Duration{"active": 20 * time.Millisecond}}
	return &DUT{View: v, Client: c, Contract: id, Lease: func() string { return lease }}, f
}

func outcomeOf(err error) Outcome {
	var f *Failure
	if errors.As(err, &f) {
		return f.Outcome
	}
	if err == nil {
		return OK
	}
	return "error: " + Outcome(err.Error())
}

func TestDUTSetMode(t *testing.T) {
	d, _ := testDUT(t, "L")
	go func() {
		time.Sleep(10 * time.Millisecond)
		d.View.Observe(modeItem(ms(1000), 1, ModeRequested, "", ""))
		d.View.Observe(modeItem(ms(1010), 1, ModeApplied, "active", ""))
		d.View.Observe(wmItem(ms(1020))) // not yet settled: 1010 + 20
		time.Sleep(10 * time.Millisecond)
		d.View.Observe(wmItem(ms(1030)))
	}()
	if err := d.SetMode(context.Background(), "active", time.Second); err != nil {
		t.Fatal(err)
	}
	if m, at := d.View.Mode(); m != "active" || at != ms(1010) {
		t.Errorf("mode %q at %d", m, at)
	}

	go func() {
		time.Sleep(10 * time.Millisecond)
		d.View.Observe(modeItem(ms(2000), 2, ModeFailed, "", "the DUT did not answer"))
	}()
	if err := d.SetMode(context.Background(), "active", time.Second); outcomeOf(err) != Fail ||
		!strings.Contains(err.Error(), "did not answer") {
		t.Errorf("an entry action failing: %v", err)
	}

	if err := d.SetMode(context.Background(), "active", 30*time.Millisecond); outcomeOf(err) != Timeout {
		t.Errorf("nothing applied: %v", err)
	}
}

func TestDUTInvoke(t *testing.T) {
	d, f := testDUT(t, "L")
	go func() {
		time.Sleep(10 * time.Millisecond)
		d.View.Observe(actionItem(ms(1), 1, 1, 0, ""))
		d.View.Observe(actionItem(ms(2), 1, 2, 1, ""))
	}()
	if err := d.Invoke(context.Background(), "recover", time.Second); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		d.View.Observe(actionItem(ms(3), 2, 2, 3, "Recover ran 5s"))
	}()
	if err := d.Invoke(context.Background(), "recover", time.Second); outcomeOf(err) != Timeout {
		t.Errorf("an action timing out on the DUT rack: %v", err)
	}
	if got := strings.Join(f.seen, ";"); got != "POST /dut/actions/recover;POST /dut/actions/recover" {
		t.Errorf("requests %s", got)
	}

	// Without the lease, nothing is sent; with a link lost, the wait ends.
	d.Lease = func() string { return "" }
	if err := d.Invoke(context.Background(), "recover", time.Second); outcomeOf(err) != Refused {
		t.Errorf("no lease: %v", err)
	}
	d.Lease = func() string { return "L" }
	go func() { time.Sleep(10 * time.Millisecond); d.View.SetLink(false) }()
	if err := d.Invoke(context.Background(), "recover", time.Second); outcomeOf(err) != LinkLost {
		t.Errorf("the link lost: %v", err)
	}
	d.Lease = func() string { return "wrong" }
	d.View.SetLink(true)
	if err := d.Invoke(context.Background(), "recover", time.Second); outcomeOf(err) != Refused {
		t.Errorf("refused by the rack: %v", err)
	}
}

// Deviations waits for the watermark to pass the window, and is told to
// whoever records judgements.
func TestDUTDeviations(t *testing.T) {
	d, _ := testDUT(t, "L")
	var js []Judgement
	d.OnJudgement = func(j Judgement) { js = append(js, j) }
	alive := monSchema("alive", true)
	// Now: Deviations waits for the watermark until the window's end, by
	// this clock, plus the grace.
	t0 := time.Now()
	d.View.Observe(monItem(alive, t0.UnixNano(), 0))
	go func() {
		time.Sleep(10 * time.Millisecond)
		d.View.Observe(monItem(alive, t0.Add(50*time.Millisecond).UnixNano(), 1))
		d.View.Observe(wmItem(t0.Add(200 * time.Millisecond).UnixNano()))
	}()
	dev, err := d.Deviations(context.Background(), []string{"alive"}, t0, t0.Add(100*time.Millisecond))
	if err != nil || fmt.Sprint(dev) != "map[alive:dev]" {
		t.Fatalf("%v %v", dev, err)
	}
	if len(js) != 1 || js[0].Kind != JudgeDeviations || js[0].Result != "alive=dev" {
		t.Errorf("judgements %+v", js)
	}
	d.WatermarkGrace = 20 * time.Millisecond
	_, err = d.Deviations(context.Background(), []string{"alive"}, t0, t0.Add(300*time.Millisecond))
	if outcomeOf(err) != LinkLost {
		t.Errorf("a watermark that never passes: %v", err)
	}
}

// A Link refuses a DUT rack with another contract, says so, and retries.
func TestLinkRefusesContract(t *testing.T) {
	dir := t.TempDir()
	cp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(cp, []byte(`{"contract":"c","schema":1,"inputs":{},"monitors":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := contract.Identify(cp)
	if err != nil {
		t.Fatal(err)
	}
	c := rack(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dut/contract":
			fmt.Fprint(w, `{"protocol":1,"contract":"c","sha256":"0000"}`)
		case "/clock":
			fmt.Fprintf(w, `{"t":%d}`, time.Now().UnixNano())
		default:
			t.Errorf("%s %s: nothing but the contract and the clock before the contract matches", r.Method, r.URL.Path)
		}
	}))
	l := &Link{Client: c, Contract: id, Retry: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	events := make(chan Event, 16)
	go l.Run(ctx, func(e Event) bool {
		select {
		case events <- e:
			return true
		case <-ctx.Done():
			return false
		}
	})
	refused, clock := 0, false
	for refused < 2 || !clock {
		select {
		case e := <-events:
			switch {
			case e.Link != nil:
				if e.Link.Kind != LinkRefused || !strings.Contains(e.Link.Msg, "sha256 0000") {
					t.Fatalf("link %+v", e.Link)
				}
				refused++
			case e.Clock != nil:
				clock = true
			}
		case <-ctx.Done():
			t.Fatalf("refused %d times, clock %v", refused, clock)
		}
	}
	if l.Lease() != "" {
		t.Error("a lease taken on a rack with another contract")
	}
}
