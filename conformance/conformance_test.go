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
	"regexp"
	"strings"
	"sync"
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
		// reset takes a while, as a real one does, so that a second action
		// asked for meanwhile can be refused.
		Actions: secondary.ActionFunc(func(ctx context.Context, a string) error {
			if a == "reset" {
				time.Sleep(200 * time.Millisecond)
			}
			return nil
		}),
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
	for _, enc := range []string{peer.EncodingLogb, peer.EncodingNDJSON} {
		t.Run(enc, func(t *testing.T) { testMasterConforms(t, enc) })
	}
}

func testMasterConforms(t *testing.T, enc string) {
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
		",cacert=" + filepath.Join(dir, "cert.pem") + ",contract=" + filepath.Join(dir, "dut-interface.json") +
		",encoding=" + enc)
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
	wantEnc := map[string]string{peer.EncodingLogb: "application/vnd.logb", peer.EncodingNDJSON: "application/x-ndjson"}[enc]
	for _, x := range rep.Results {
		if x.Title == "the master reads the stream as" && !strings.HasPrefix(x.Detail, wantEnc) {
			t.Errorf("reported %q, want %s", x.Detail, wantEnc)
		}
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

// secondaryFailures runs the secondary checks against a rack served
// through wrap, and returns the titles that failed and those skipped.
func secondaryFailures(t *testing.T, wrap func(http.Handler) http.Handler) (failed, skipped map[string]bool, report string) {
	t.Helper()
	r := newRack(t, wrap)
	defer r.stop()
	rep, err := CheckSecondary(context.Background(), r.config())
	must(t, err)
	var b bytes.Buffer
	rep.Write(&b)
	failed, skipped = map[string]bool{}, map[string]bool{}
	for _, x := range rep.Results {
		switch x.Status {
		case Fail:
			failed[x.Title] = true
		case Skip:
			skipped[x.Title] = true
		}
	}
	return failed, skipped, b.String()
}

// onNDJSON serves next, but passes every line of an NDJSON subscription
// through edit.
func onNDJSON(edit func(line []byte) []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path == "/stream/sub" && strings.Contains(req.Header.Get("Accept"), "ndjson") {
				w = &editWriter{ResponseWriter: w, edit: edit}
			}
			next.ServeHTTP(w, req)
		})
	}
}

// editWriter edits what is written to it a line at a time; the encoder
// writes each line with one Write.
type editWriter struct {
	http.ResponseWriter
	edit func([]byte) []byte
}

func (w *editWriter) Write(b []byte) (int, error) {
	if _, err := w.ResponseWriter.Write(w.edit(b)); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (w *editWriter) Flush() { w.ResponseWriter.(http.Flusher).Flush() }

// An NDJSON encoder that writes t as a JSON number: exact here, but not in
// a reader that holds numbers as binary64 once a run passes 2^53 ns.
func TestSecondaryNDJSONNumericTime(t *testing.T) {
	re := regexp.MustCompile(`"t":"(-?[0-9]+)"`)
	failed, _, rep := secondaryFailures(t, onNDJSON(func(l []byte) []byte { return re.ReplaceAll(l, []byte(`"t":$1`)) }))
	want := "integers wider than 53 bits, t among them, are decimal strings; labelled enums are labels"
	if !failed[want] || len(failed) != 1 {
		t.Errorf("want exactly %q to fail:\n%s", want, rep)
	}
}

// An NDJSON encoder that loses a monitor's records: the stream reads, but
// is not the Logb one.
func TestSecondaryNDJSONLosesRecords(t *testing.T) {
	var mu sync.Mutex
	n := 0
	failed, _, rep := secondaryFailures(t, onNDJSON(func(l []byte) []byte {
		if !bytes.HasPrefix(l, []byte(`{"rec":"mon.watermark"`)) {
			return l
		}
		mu.Lock()
		defer mu.Unlock()
		if n++; n%5 == 0 {
			return nil // every fifth watermark
		}
		return l
	}))
	want := "the records are the same in both encodings, to the nanosecond"
	if !failed[want] || len(failed) != 1 {
		t.Errorf("want exactly %q to fail:\n%s", want, rep)
	}
}

// A rack from before NDJSON, that ignores Accept: Logb for everyone.
func TestSecondaryIgnoresAccept(t *testing.T) {
	failed, skipped, rep := secondaryFailures(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req.Header.Del("Accept")
			next.ServeHTTP(w, req)
		})
	})
	for _, want := range []string{"a subscription in an encoding it does not serve is refused 406",
		"the stream is served as NDJSON (SHOULD)"} {
		if !failed[want] {
			t.Errorf("not caught: %s\n%s", want, rep)
		}
	}
	if len(failed) != 2 || len(skipped) != len(ndjsonTitles) {
		t.Errorf("%d failures and %d skips, want 2 and %d:\n%s", len(failed), len(skipped), len(ndjsonTitles), rep)
	}
}

// A rack that serves Logb only, and says so: it conforms, the NDJSON checks
// skipped.
func TestSecondaryLogbOnly(t *testing.T) {
	failed, skipped, rep := secondaryFailures(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path == "/stream/sub" && strings.Contains(req.Header.Get("Accept"), "ndjson") {
				http.Error(w, "Logb only", http.StatusNotAcceptable)
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	if len(failed) != 0 || len(skipped) != len(ndjsonTitles) {
		t.Errorf("%d failures and %d skips, want none and %d:\n%s", len(failed), len(skipped), len(ndjsonTitles), rep)
	}
}
