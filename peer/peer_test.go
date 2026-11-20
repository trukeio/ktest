//go:build linux

package peer

import (
	"context"
	"encoding/pem"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/can"
	"github.com/trukeio/ktest/record"
	"github.com/trukeio/ktest/stream"
)

// rack serves h over TLS and returns a client for it.
func rack(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cert := filepath.Join(dir, "rack.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(cert, b, 0o644); err != nil {
		t.Fatal(err)
	}
	tok := filepath.Join(dir, "token")
	if err := os.WriteFile(tok, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig("dut=" + srv.URL + ",token=" + tok + ",cacert=" + cert + ",contract=c.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseConfig(t *testing.T) {
	c, err := ParseConfig("dut=https://rack:8443,token=t,cacert=c.pem,contract=d.json")
	if err != nil || c.Name != "dut" || c.URL != "https://rack:8443" || c.Token != "t" || c.CACert != "c.pem" || c.Contract != "d.json" {
		t.Fatalf("%+v, %v", c, err)
	}
	for _, bad := range []struct{ flag, want string }{
		{"d.u.t=https://rack:8443,token=t,contract=d", "no dots"},
		{"https://rack:8443,token=t,contract=d", "want name="},
		{"dut=http://rack:8443,token=t,contract=d", "https listener"},
		{"dut=https://rack:8443,contract=d", "token= is required"},
		{"dut=https://rack:8443,token=t", "contract= is required"},
		{"dut=https://rack:8443,token=t,contract=d,colour=red", "not a peer setting"},
	} {
		if _, err := ParseConfig(bad.flag); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s: %v, want %q", bad.flag, err, bad.want)
		}
	}
}

// The DUT rack's clock is shifted by a known amount, and its answers are
// delayed unevenly, before and after it reads its clock: the estimate is
// within its stated uncertainty of the shift, which is all it claims.
func TestClockOffset(t *testing.T) {
	for _, shift := range []time.Duration{0, 250 * time.Millisecond, -3 * time.Second, 37 * time.Hour} {
		t.Run(shift.String(), func(t *testing.T) {
			r := rand.New(rand.NewPCG(uint64(shift), 7))
			var mu sync.Mutex
			delay := func() time.Duration {
				mu.Lock()
				defer mu.Unlock()
				return time.Duration(r.IntN(4000)) * time.Microsecond
			}
			c := rack(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Header.Get("Authorization") != "Bearer secret" {
					http.Error(w, `{"error":"who are you"}`, http.StatusUnauthorized)
					return
				}
				time.Sleep(delay())
				fmt.Fprintf(w, "{\"t\":%d}\n", time.Now().Add(shift).UnixNano())
				w.(http.Flusher).Flush()
				time.Sleep(delay())
			}))
			var o Offsets
			var e Estimate
			for i := 0; i < 12; i++ {
				s, err := c.Clock(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				e = o.Add(s)
				if d := (e.Offset - shift).Abs(); d > e.Uncertainty {
					t.Fatalf("sample %d: offset %v, shift %v: %v out, more than the %v it claims", i, e.Offset, shift, d, e.Uncertainty)
				}
			}
			if e.Uncertainty > 5*time.Millisecond {
				t.Errorf("uncertainty %v after 12 samples on loopback", e.Uncertainty)
			}
		})
	}
}

// A refusal comes back as the DUT rack's sentence and number.
func TestRefused(t *testing.T) {
	c := rack(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error":"another client holds the instrument's lease","seq":12}`)
	}))
	_, err := c.Lease(context.Background(), "master", 30*time.Second)
	re, ok := err.(*RefusedError)
	if !ok || re.Seq != 12 || re.Status != http.StatusConflict || !strings.Contains(re.Msg, "another client") {
		t.Fatalf("got %v", err)
	}
}

// A rack that states no protocol is version 1; one that states a version
// this package does not speak is not to be gone on with.
func TestProtocol(t *testing.T) {
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{`{"contract":"c","sha256":"ab"}`, true},
		{`{"protocol":1,"contract":"c","sha256":"ab"}`, true},
		{`{"protocol":2,"contract":"c","sha256":"ab"}`, false},
	} {
		c := rack(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			fmt.Fprint(w, tc.body)
		}))
		in, err := c.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := in.Supported(); (err == nil) != tc.ok {
			t.Errorf("%s: Supported() = %v, want ok %v", tc.body, err, tc.ok)
		}
	}
}

// The subscription, against a real recorder writing a live stream into a
// real hub: the state of a held stream as the next segment restates it, then
// each record as written, each on the DUT rack's clock to the nanosecond.
func TestSubscribe(t *testing.T) {
	hub := stream.New()
	t0 := time.Unix(1_763_000_000, 0)
	now := t0
	w, err := record.New(hub, record.Config{Bus: "vcan0", Stamp: can.StampTimestampNS, Codec: logb.CodecNone,
		PerFrame: 1, Live: true, Segment: 100 * time.Millisecond, Meta: map[string]string{"daemon.version": "test"},
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	mon, err := w.NewStream(record.Schema("mon.alive", 8*16, []logb.Field{
		{Name: "state", BitOffset: 64, BitWidth: 8, Type: logb.TypeUint, Hold: true},
		{Name: "cause", BitOffset: 72, BitWidth: 56, Type: logb.TypeString},
	}, nil))
	if err != nil {
		t.Fatal(err)
	}
	write := func(at time.Duration, state byte, cause string) {
		if err := w.Write(t0.Add(at), mon, func(rec []byte) { rec[8] = state; copy(rec[9:16], cause) }); err != nil {
			t.Fatal(err)
		}
	}
	write(10*time.Millisecond, 1, "dev") // before anyone listens

	c := rack(t, http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if got := req.URL.Query().Get("ch"); got != "mon.*" {
			t.Errorf("ch=%q", got)
		}
		hub.ServeStreams(rw, req, func(name string) bool { return strings.HasPrefix(name, "mon.") })
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := c.Subscribe(ctx, []string{"mon.*"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	go func() {
		time.Sleep(50 * time.Millisecond)
		// The first opens a segment, which restates the state of 10 ms.
		write(120*time.Millisecond, 0, "ok")
		write(131*time.Millisecond, 1, "dev")
	}()
	type got struct {
		t        time.Duration
		state    byte
		restated bool
	}
	var items []got
	for len(items) < 3 {
		it, err := s.Next()
		if err != nil {
			t.Fatal(err)
		}
		if it.Schema.Name != "mon.alive" {
			t.Fatalf("stream %s", it.Schema.Name)
		}
		items = append(items, got{time.Duration(it.PeerT - t0.UnixNano()), it.Rec[8], it.Restated})
	}
	want := []got{{10 * time.Millisecond, 1, true}, {120 * time.Millisecond, 0, false}, {131 * time.Millisecond, 1, false}}
	if fmt.Sprint(items) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", items, want)
	}
}
