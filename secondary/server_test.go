package secondary

import (
	"context"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/contract"
	"github.com/trukeio/ktest/peer"
	"github.com/trukeio/ktest/record"
)

// A DUT rack built on this package, driven by a master built on package
// peer, over TLS: the two halves of the protocol, neither of them ktestd.

const testContract = `{
  "contract": "test-dut",
  "schema": 1,
  "inputs":   {"Status": {"channel": "DutStatus", "max_age": "1s"},
               "Vx": {"channel": "DutSupply.v", "max_age": "1s"}},
  "monitors": {
    "alive":   {"ok": "age(Status) < 50ms"},
    "abort":   {"ok": "Vx < 16V"},
    "powered": {"ok": "Vx > 9V", "modes": ["active"]}
  },
  "modes":    {"active": {"action": "enter", "settle": "30ms"}},
  "resource": "actions.txt",
  "actions":  {"enter": "enter", "recover": "recover", "broken": "broken"},
  "timeouts": {"recover": "1s"}
}`

var (
	statusSchema = record.Schema("DutStatus", 8*16, []logb.Field{
		{Name: "counter", BitOffset: 64, BitWidth: 16, Type: logb.TypeUint},
	}, nil)
	supplySchema = record.Schema("DutSupply", 8*16, []logb.Field{
		{Name: "v", BitOffset: 64, BitWidth: 64, Type: logb.TypeFloat, Unit: "V"},
	}, nil)
)

// host is the program a DUT rack is: it reads its DUT, and writes what it
// reads, until told to stop; the supply it reads is whatever the test sets.
type host struct {
	srv      *Server
	alive    atomic.Bool
	volts    atomic.Uint64
	disarmed atomic.Int32
	stop     chan struct{}
	done     sync.WaitGroup
}

func (h *host) run(t *testing.T) {
	status, supply := h.srv.Stream("DutStatus"), h.srv.Stream("DutSupply")
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	var n uint16
	for {
		select {
		case <-h.stop:
			return
		case now := <-tick.C:
			if h.alive.Load() {
				n++
				if err := h.srv.Write(status, now, func(rec []byte) { binary.LittleEndian.PutUint16(rec[8:], n) }); err != nil {
					t.Error(err)
					return
				}
			}
			v := h.volts.Load()
			if err := h.srv.Write(supply, now, func(rec []byte) { binary.LittleEndian.PutUint64(rec[8:], v) }); err != nil {
				t.Error(err)
				return
			}
		}
	}
}

func (h *host) setVolts(v float64) { h.volts.Store(math.Float64bits(v)) }

func TestMasterDrivesSecondary(t *testing.T) {
	dir := t.TempDir()
	cp := filepath.Join(dir, "dut-interface.json")
	must(t, os.WriteFile(cp, []byte(testContract), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "actions.txt"), []byte("enter\nrecover\nbroken\n"), 0o644))
	recPath := filepath.Join(dir, "dut.logb")
	recFile, err := os.Create(recPath)
	must(t, err)

	h := &host{stop: make(chan struct{})}
	h.alive.Store(true)
	h.setVolts(12)
	srv, err := New(Config{
		Contract:  cp,
		Streams:   []*logb.Schema{statusSchema, supplySchema},
		Auth:      Tokens(map[string]Caller{"m-token": {Name: "master", Operate: true}, "v-token": {Name: "viewer"}}),
		Recording: recFile,
		Meta:      map[string]string{"daemon.version": "secondary test"},
		Actions: ActionFunc(func(ctx context.Context, action string) error {
			switch action {
			case "broken":
				return errors.New("the DUT did not answer")
			case "recover":
				<-ctx.Done() // never ends of itself: the timeout ends it
				return ctx.Err()
			}
			return nil
		}),
		OnDisarm: func() { h.disarmed.Add(1) },
	})
	must(t, err)
	h.srv = srv
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run(ctx) }()
	h.done.Add(1)
	go func() { defer h.done.Done(); h.run(t) }()

	ts := httptest.NewTLSServer(srv.Handler())
	defer ts.Close()

	// The master: its own copy of the contract, a Link, a View fed with what
	// the Link hands over, the DUT operations and the guard.
	mdir := t.TempDir()
	mcp := filepath.Join(mdir, "dut-interface.json")
	must(t, os.WriteFile(mcp, []byte(testContract), 0o644))
	must(t, os.WriteFile(filepath.Join(mdir, "actions.txt"), []byte("enter\nrecover\nbroken\n"), 0o644))
	id, err := contract.Identify(mcp)
	must(t, err)
	c := client(t, ts, "m-token", mcp)
	link := &peer.Link{Client: c, Contract: id}
	view := peer.NewView()
	lctx, lcancel := context.WithCancel(context.Background())
	defer lcancel()
	go link.Run(lctx, func(e peer.Event) bool { view.Apply(e); return true })
	dut := &peer.DUT{View: view, Client: c, Contract: id, Lease: link.Lease}
	guard := peer.Guard{WatermarkAge: 100 * time.Millisecond, Abort: "abort"}

	// Connected, leased and armed, the monitors ok at the watermark.
	waitFor(t, "the lease", func() bool { return link.Lease() != "" })
	_, ok, err := dut.AllOK(context.Background(), []string{"alive", "abort"}, time.Now().Add(2*time.Second))
	if err != nil || !ok {
		t.Fatalf("all ok: %v %v", ok, err)
	}
	if v := guard.Check(view, time.Now()); v.MonitoringLost || v.DUTAbort {
		t.Errorf("a healthy DUT: %+v", v)
	}

	// The protocol's version and the contract, stated (§11).
	info, err := c.Info(context.Background())
	if err != nil || info.Protocol != peer.Protocol || info.Hash != id.Hash || info.ResourceHash != id.ResourceHash ||
		info.Provenance["daemon.version"] != "secondary test" {
		t.Errorf("info %+v %v", info, err)
	}

	// A mode, its entry action, its settle by the watermark; the monitor
	// that watches it is judged from then on.
	must(t, dut.SetMode(context.Background(), "active", 2*time.Second))
	if m, _ := view.Mode(); m != "active" {
		t.Errorf("mode %q", m)
	}
	waitFor(t, "powered ok", func() bool { return view.States([]string{"powered"})["powered"] == peer.StateOK })

	// Actions: one that fails, and one that runs into its timeout.
	if err := dut.Invoke(context.Background(), "broken", 2*time.Second); outcome(err) != peer.Fail ||
		!strings.Contains(err.Error(), "did not answer") {
		t.Errorf("broken: %v", err)
	}
	if err := dut.Invoke(context.Background(), "recover", 3*time.Second); outcome(err) != peer.Timeout {
		t.Errorf("recover: %v", err)
	}

	// The DUT goes quiet: alive deviates, by the deadline age() implies, and
	// a window over it says so once the watermark has passed it.
	t0 := time.Now()
	h.alive.Store(false)
	time.Sleep(150 * time.Millisecond)
	dev, err := dut.Deviations(context.Background(), []string{"alive", "abort"}, t0, time.Now())
	if err != nil || dev["alive"] != peer.StateDev || dev["abort"] != "" {
		t.Errorf("deviations %v %v", dev, err)
	}
	h.alive.Store(true)

	// Overvoltage: abort is dev, and the master's guard says protect.
	h.setVolts(17)
	waitFor(t, "dut_abort", func() bool { return guard.Check(view, time.Now()).DUTAbort })
	h.setVolts(12)
	waitFor(t, "dut_abort cleared", func() bool { return !guard.Check(view, time.Now()).DUTAbort })

	// A viewer may not change the mode, and may disarm; the host is told,
	// and the mode cannot change until someone arms again.
	vc := client(t, ts, "v-token", mcp)
	if _, err := vc.SetMode(context.Background(), link.Lease(), "active"); refused(err) != http.StatusForbidden {
		t.Errorf("a viewer's mode change: %v", err)
	}
	must(t, vc.Disarm(context.Background()))
	if h.disarmed.Load() != 1 {
		t.Errorf("the host was told of %d disarms", h.disarmed.Load())
	}
	if _, err := c.SetMode(context.Background(), link.Lease(), ""); refused(err) != http.StatusConflict {
		t.Errorf("a mode change disarmed: %v", err)
	}

	// The link goes, and the DUT rack carries on; then it stops.
	lcancel()
	time.Sleep(50 * time.Millisecond)
	close(h.stop)
	h.done.Wait()
	cancel()
	must(t, <-runDone)
	must(t, recFile.Close())

	// The recording, held to the evaluator by monitor-check: every transition
	// recorded live is the replay's, to the nanosecond (§5.5).
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command to run monitor-check with")
	}
	out, err := exec.Command(goBin, "run", "../cmd/monitor-check", "-live", cp, recPath).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "are the replay's, to the nanosecond") ||
		strings.Contains(string(out), "gap") {
		t.Errorf("monitor-check: %v\n%s", err, out)
	}
}

func client(t *testing.T, ts *httptest.Server, token, contractPath string) *peer.Client {
	t.Helper()
	dir := t.TempDir()
	cert := filepath.Join(dir, "rack.pem")
	must(t, os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}), 0o644))
	tok := filepath.Join(dir, "token")
	must(t, os.WriteFile(tok, []byte(token), 0o600))
	cfg, err := peer.ParseConfig("dut=" + ts.URL + ",token=" + tok + ",cacert=" + cert + ",contract=" + contractPath)
	must(t, err)
	c, err := peer.New(cfg)
	must(t, err)
	return c
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func outcome(err error) peer.Outcome {
	var f *peer.Failure
	if errors.As(err, &f) {
		return f.Outcome
	}
	if err == nil {
		return peer.OK
	}
	return peer.Outcome("error: " + err.Error())
}

func refused(err error) int {
	var re *peer.RefusedError
	if errors.As(err, &re) {
		return re.Status
	}
	return 0
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Each refusal of §6.1, with its status, and recorded under a number.
func TestRefusals(t *testing.T) {
	dir := t.TempDir()
	cp := filepath.Join(dir, "dut-interface.json")
	must(t, os.WriteFile(cp, []byte(testContract), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "actions.txt"), []byte("enter\nrecover\nbroken\n"), 0o644))
	srv, err := New(Config{Contract: cp, Streams: []*logb.Schema{statusSchema, supplySchema},
		Auth: Tokens(map[string]Caller{"a": {Name: "alice", Operate: true}, "b": {Name: "bob", Operate: true},
			"v": {Name: "viewer"}}),
		Recording: io.Discard, Actions: ActionFunc(func(context.Context, string) error { return nil })})
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	defer func() { cancel(); <-done }()
	ts := httptest.NewTLSServer(srv.Handler())
	defer ts.Close()
	a, b, v := client(t, ts, "a", cp), client(t, ts, "b", cp), client(t, ts, "v", cp)
	bg := context.Background()

	if _, err := client(t, ts, "nobody", cp).Info(bg); refused(err) != http.StatusUnauthorized {
		t.Errorf("an unknown token: %v", err)
	}
	if _, err := v.Lease(bg, "", time.Minute); refused(err) != http.StatusForbidden {
		t.Errorf("a viewer's lease: %v", err)
	}
	if _, err := a.Lease(bg, "mallory", time.Minute); refused(err) != http.StatusBadRequest {
		t.Errorf("a lease in another's name: %v", err)
	}
	tok, err := a.Lease(bg, "", time.Minute)
	must(t, err)
	if _, err := b.Lease(bg, "", time.Minute); refused(err) != http.StatusConflict ||
		!strings.Contains(err.Error(), "alice holds") {
		t.Errorf("a lease held: %v", err)
	}
	if err := b.Arm(bg, "wrong"); refused(err) != http.StatusForbidden {
		t.Errorf("arm without the lease: %v", err)
	}
	if _, err := a.Invoke(bg, tok, "recover"); refused(err) != http.StatusConflict {
		t.Errorf("an action unarmed: %v", err)
	}
	must(t, a.Arm(bg, tok))
	if _, err := a.SetMode(bg, tok, "sleep"); refused(err) != http.StatusNotFound {
		t.Errorf("an undeclared mode: %v", err)
	}
	if _, err := a.Invoke(bg, tok, "dance"); refused(err) != http.StatusNotFound {
		t.Errorf("an undeclared action: %v", err)
	}
	if _, err := a.Subscribe(bg, []string{"mon.[x"}); refused(err) != http.StatusBadRequest {
		t.Errorf("a bad glob: %v", err)
	}
	seq, err := a.Invoke(bg, tok, "recover")
	if err != nil || seq == 0 {
		t.Errorf("an action: %d %v", seq, err)
	}
	must(t, a.Renew(bg, tok))
	must(t, a.Release(bg, tok))
	if _, err := b.Lease(bg, "", time.Minute); err != nil {
		t.Errorf("a lease released and taken again: %v", err)
	}
}
