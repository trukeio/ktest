package conformance

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/contract"
	"github.com/trukeio/ktest/peer"
	"github.com/trukeio/ktest/secondary"
)

// The kit is held to the implementations in this repository: each must pass
// it cleanly, and an implementation broken on purpose must fail it where it
// is broken.

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// rack is a secondary.Server on the reference contract, served over TLS
// through wrap, with three tokens written to files.
type rack struct {
	url, cert, contract  string
	operate, other, view string
	stop                 func()
}

func newRack(t *testing.T, wrap func(http.Handler) http.Handler) *rack {
	dir := t.TempDir()
	cp := filepath.Join(dir, "dut-interface.json")
	must(t, os.WriteFile(cp, []byte(referenceContract), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "actions.txt"), []byte(referenceActions), 0o644))
	srv, err := secondary.New(secondary.Config{Contract: cp, Streams: []*logb.Schema{beatSchema, refSupplySchema},
		Actions: secondary.ActionFunc(func(ctx context.Context, a string) error { return nil }),
		Auth: secondary.Tokens(map[string]secondary.Caller{"op": {Name: "alice", Operate: true},
			"op2": {Name: "bob", Operate: true}, "vw": {Name: "viewer"}}),
		Recording: io.Discard})
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	gen := make(chan struct{})
	go func() {
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for n := uint32(0); ; n++ {
			select {
			case <-gen:
				return
			case now := <-tick.C:
				srv.Write(srv.Stream("Beat"), now, func(rec []byte) { binary.LittleEndian.PutUint32(rec[8:], n) })
				srv.Write(srv.Stream("Supply"), now, func(rec []byte) { binary.LittleEndian.PutUint64(rec[8:], 0x4028000000000000) }) // 12 V
			}
		}
	}()
	h := srv.Handler()
	if wrap != nil {
		h = wrap(h)
	}
	ts := httptest.NewTLSServer(h)
	r := &rack{url: ts.URL, contract: cp, cert: filepath.Join(dir, "cert.pem")}
	must(t, os.WriteFile(r.cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}), 0o644))
	for name, tok := range map[*string]string{&r.operate: "op", &r.other: "op2", &r.view: "vw"} {
		*name = filepath.Join(dir, tok+".token")
		must(t, os.WriteFile(*name, []byte(tok), 0o600))
	}
	r.stop = func() { ts.Close(); close(gen); cancel(); <-done }
	return r
}

func (r *rack) config() SecondaryConfig {
	return SecondaryConfig{URL: r.url, CACert: r.cert, Contract: r.contract, Token: r.operate, Other: r.other,
		View: r.view, Action: "reset", Wait: 3 * time.Second}
}

func TestSecondaryConforms(t *testing.T) {
	r := newRack(t, nil)
	defer r.stop()
	rep, err := CheckSecondary(context.Background(), r.config())
	must(t, err)
	var b bytes.Buffer
	rep.Write(&b)
	if rep.Failed() != 0 {
		t.Fatalf("secondary.Server does not conform:\n%s", b.String())
	}
	for _, x := range rep.Results {
		if x.Status == Skip {
			t.Errorf("skipped with every token and an action given: %s %s: %s", x.Section, x.Title, x.Detail)
		}
	}
	t.Logf("\n%s", b.String())
}

// A rack broken three ways: it states no protocol, lets anyone take a held
// lease from its holder, and answers a mode change disarmed as a bad request.
func TestSecondaryBroken(t *testing.T) {
	r := newRack(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			switch {
			case req.URL.Path == "/dut/contract":
				rec := httptest.NewRecorder()
				next.ServeHTTP(rec, req)
				body := strings.Replace(rec.Body.String(), `"protocol":1,`, "", 1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(rec.Code)
				io.WriteString(w, body)
			case req.URL.Path == "/dut/mode" && req.Header.Get("Ktest-Lease") != "not-the-lease":
				rec := httptest.NewRecorder()
				next.ServeHTTP(rec, req)
				code := rec.Code
				if code == http.StatusConflict {
					code = http.StatusBadRequest
				}
				w.WriteHeader(code)
				w.Write(rec.Body.Bytes())
			default:
				next.ServeHTTP(w, req)
			}
		})
	})
	defer r.stop()
	rep, err := CheckSecondary(context.Background(), r.config())
	must(t, err)
	var b bytes.Buffer
	rep.Write(&b)
	failed := map[string]bool{}
	for _, x := range rep.Results {
		if x.Status == Fail {
			failed[x.Title] = true
		}
	}
	for _, want := range []string{`GET /dut/contract states "protocol"`, "a mode change disarmed is refused 409",
		"after a disarm, a mode change is refused 409"} {
		if !failed[want] {
			t.Errorf("not caught: %s\n%s", want, b.String())
		}
	}
	if len(failed) != 3 {
		t.Errorf("%d failures, want 3:\n%s", len(failed), b.String())
	}
}

// A master on package peer, as ktestd's is, against the reference rack:
// connected, faulted, disconnected, shown another contract and another
// protocol, and back.
func TestMasterConforms(t *testing.T) {
	if testing.Short() {
		t.Skip("a run through every phase takes some seconds")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ready := make(chan string, 1)
	var rep *Report
	done := make(chan error, 1)
	go func() {
		var err error
		rep, err = Master(ctx, MasterConfig{Addr: "127.0.0.1:0", Dir: dir, Phase: 1200 * time.Millisecond,
			Connect: 10 * time.Second, Ready: func(u string) { ready <- u }})
		done <- err
	}()
	u := <-ready
	cfg, err := peer.ParseConfig("ref=" + u + ",token=" + filepath.Join(dir, "master.token") +
		",cacert=" + filepath.Join(dir, "cert.pem") + ",contract=" + filepath.Join(dir, "dut-interface.json"))
	must(t, err)
	c, err := peer.New(cfg)
	must(t, err)
	id, err := contract.Identify(cfg.Contract)
	must(t, err)
	mctx, mcancel := context.WithCancel(ctx)
	defer mcancel()
	link := &peer.Link{Client: c, Contract: id, LeaseTTL: 3 * time.Second, Retry: 100 * time.Millisecond}
	view := peer.NewView()
	go link.Run(mctx, func(e peer.Event) bool { view.Apply(e); return true })

	must(t, <-done)
	var b bytes.Buffer
	rep.Write(&b)
	if rep.Failed() != 0 {
		t.Fatalf("package peer's master does not conform:\n%s", b.String())
	}
	for _, x := range rep.Results {
		if x.Status == Skip {
			t.Errorf("skipped: %s %s: %s", x.Section, x.Title, x.Detail)
		}
	}
	t.Logf("\n%s", b.String())
}

// A master that never asks for the contract: it subscribes, leases and arms
// at once, and again whenever its subscription ends.
func TestMasterBroken(t *testing.T) {
	if testing.Short() {
		t.Skip("a run through every phase takes some seconds")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ready := make(chan string, 1)
	var rep *Report
	done := make(chan error, 1)
	go func() {
		var err error
		rep, err = Master(ctx, MasterConfig{Addr: "127.0.0.1:0", Dir: dir, Phase: 500 * time.Millisecond,
			Connect: 10 * time.Second, Reconnect: 2 * time.Second, Ready: func(u string) { ready <- u }})
		done <- err
	}()
	u := <-ready
	cfg, err := peer.ParseConfig("ref=" + u + ",token=" + filepath.Join(dir, "master.token") +
		",cacert=" + filepath.Join(dir, "cert.pem") + ",contract=" + filepath.Join(dir, "dut-interface.json"))
	must(t, err)
	c, err := peer.New(cfg)
	must(t, err)
	mctx, mcancel := context.WithCancel(ctx)
	defer mcancel()
	go func() {
		for mctx.Err() == nil {
			if tok, err := c.Lease(mctx, "", 30*time.Second); err == nil {
				c.Arm(mctx, tok)
			}
			if s, err := c.Subscribe(mctx, []string{"mon.*"}); err == nil {
				for {
					if _, err := s.Next(); err != nil {
						break
					}
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	must(t, <-done)
	var b bytes.Buffer
	rep.Write(&b)
	failed := map[string]bool{}
	for _, x := range rep.Results {
		if x.Status == Fail {
			failed[x.Title] = true
		}
	}
	for _, want := range []string{"it asks for the contract before it subscribes or takes the lease",
		"on reconnecting after the link is lost, it asks for the contract first",
		"the clock is sampled at least once a second"} {
		if !failed[want] {
			t.Errorf("not caught: %s\n%s", want, b.String())
		}
	}
}
