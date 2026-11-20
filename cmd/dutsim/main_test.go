//go:build linux

package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/trukeio/ktest/can"
	"github.com/trukeio/ktest/dut"
	"github.com/trukeio/ktest/psu"
)

// The bus the end-to-end tests run on. They are skipped without it; per §8
// interfaces are opened, not configured, so the test does not make one.
const testBus = "vcan0"

// syncBuf is a transcript both the test and dutsim's goroutine touch.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// bench is dutsim on vcan, powered from a psusim channel, with a socket of
// the test's own watching the bus.
type bench struct {
	t     *testing.T
	sup   *psu.Sim
	watch *can.Conn
	codec *dut.Codec
	tr    *syncBuf
	done  chan error
}

func newBench(t *testing.T, b dut.Behaviour) *bench {
	t.Helper()
	if _, err := net.InterfaceByName(testBus); err != nil {
		t.Skipf("%s is missing: %v", testBus, err)
	}
	sup := &psu.Sim{Model: "psusim", Channels: []psu.SimChannel{{VMax: 30, IMax: 3, Load: 10}}}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go sup.Serve(l)
	for _, c := range []string{"CURR 3,(@1)", "VOLT 12,(@1)"} {
		sup.Exec(c)
	}

	watch, err := can.Open(testBus, can.Options{Stamp: can.StampTimestampNS})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { watch.Close() })
	watch.SetReadTimeout(20 * time.Millisecond)
	codec, err := dut.NewCodec()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	bn := &bench{t: t, sup: sup, watch: watch, codec: codec, tr: &syncBuf{}, done: make(chan error, 1)}
	o := options{
		iface: testBus, supply: l.Addr().String(), ch: 1,
		poll: 2 * time.Millisecond, period: 10 * time.Millisecond, transcript: bn.tr,
		cfg: dut.Config{Behaviour: b, VMin: 9, VRestore: 9.5, Boot: 50 * time.Millisecond, Speed: 3000},
	}
	go func() { bn.done <- run(ctx, o, func(string) {}) }()
	t.Cleanup(func() {
		cancel()
		if err := <-bn.done; err != nil {
			t.Errorf("dutsim: %v", err)
		}
		if t.Failed() {
			t.Logf("transcript:\n%s", bn.tr.String())
		}
	})
	return bn
}

func (b *bench) supply(cmd string) { b.sup.Exec(cmd) }

// alive collects the DutStatus counters and EngineData speeds seen in d.
func (b *bench) alive(d time.Duration) (counters []int, speeds []int) {
	b.t.Helper()
	var f can.Frame
	for end := time.Now().Add(d); time.Now().Before(end); {
		if err := b.watch.Recv(&f); err != nil {
			if errors.Is(err, unix.EAGAIN) {
				continue
			}
			b.t.Fatal(err)
		}
		switch f.Arbitration() {
		case b.codec.Status.ID:
			counters = append(counters, int(f.Data[0]&15))
		case b.codec.Engine.ID:
			speeds = append(speeds, int(f.Data[0])|int(f.Data[1])<<8)
		}
	}
	return counters, speeds
}

func (b *bench) reset() {
	b.t.Helper()
	p, err := b.codec.ResetMsg.Encode(map[string]any{"Request": 1})
	if err != nil {
		b.t.Fatal(err)
	}
	if err := b.watch.Send(can.New(b.codec.ResetMsg.ID, p)); err != nil {
		b.t.Fatal(err)
	}
}

func (b *bench) talks(what string, d time.Duration) []int {
	b.t.Helper()
	c, _ := b.alive(d)
	if len(c) < int(d/(10*time.Millisecond))/2 {
		b.t.Fatalf("%s: %d alive frames in %v", what, len(c), d)
	}
	return c
}

func (b *bench) silent(what string, d time.Duration) {
	b.t.Helper()
	if c, _ := b.alive(d); len(c) != 0 {
		b.t.Fatalf("%s: %d alive frames in %v, want none", what, len(c), d)
	}
}

// The reset behaviour, end to end: silent unpowered, talking once powered,
// silent through a dip and after it, and talking again, from counter 0, after
// a reset frame.
func TestResetOnBus(t *testing.T) {
	b := newBench(t, dut.Reset)
	b.silent("output off", 100*time.Millisecond)
	b.supply("OUTP ON,(@1)")
	b.alive(80 * time.Millisecond) // the boot
	c := b.talks("powered", 200*time.Millisecond)
	for i := 1; i < len(c); i++ {
		if c[i] != (c[i-1]+1)%16 {
			t.Fatalf("counter skips: %v", c)
		}
	}
	b.supply("VOLT 6,(@1)")
	b.alive(20 * time.Millisecond)
	b.silent("dipped", 100*time.Millisecond)
	b.supply("VOLT 12,(@1)")
	b.silent("restored, not reset", 200*time.Millisecond)
	b.reset()
	b.alive(40 * time.Millisecond) // inside the boot
	if c := b.talks("reset", 100*time.Millisecond); c[0] != 0 {
		t.Fatalf("first counter after a reset is %d, want 0: %v", c[0], c)
	}
	tr := b.tr.String()
	for _, want := range []string{"state hung", "supply ok 12V", "reset\n", "state running"} {
		if !strings.Contains(tr, want) {
			t.Errorf("transcript lacks %q", want)
		}
	}
}

// Glitch keeps the counter going through the dip and reports the glitch speed.
func TestGlitchOnBus(t *testing.T) {
	b := newBench(t, dut.Glitch)
	b.supply("OUTP ON,(@1)")
	b.alive(80 * time.Millisecond)
	if _, s := b.alive(50 * time.Millisecond); len(s) == 0 || s[len(s)-1] != 3000 {
		t.Fatalf("speeds %v, want 3000", s)
	}
	b.supply("VOLT 5,(@1)")
	b.alive(20 * time.Millisecond)
	c, s := b.alive(100 * time.Millisecond)
	if len(c) < 5 || len(s) == 0 || s[len(s)-1] != 0 {
		t.Fatalf("glitched: counters %v speeds %v", c, s)
	}
	for i := 1; i < len(c); i++ {
		if c[i] != (c[i-1]+1)%16 {
			t.Fatalf("counter skips through a glitch: %v", c)
		}
	}
}

// A supply that stops answering stops the DUT with an error and a line in the
// transcript, rather than leaving it running on a voltage it no longer knows.
func TestSupplyLost(t *testing.T) {
	if _, err := net.InterfaceByName(testBus); err != nil {
		t.Skipf("%s is missing: %v", testBus, err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		c, err := l.Accept()
		if err == nil {
			c.Close()
		}
		l.Close()
	}()
	tr := &syncBuf{}
	o := options{iface: testBus, supply: l.Addr().String(), ch: 1, poll: 5 * time.Millisecond,
		period: 10 * time.Millisecond, transcript: tr,
		cfg: dut.Config{Behaviour: dut.Restore, VMin: 9, VRestore: 9.5, Speed: 3000}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := run(ctx, o, func(string) {}); err == nil || !strings.HasPrefix(err.Error(), "supply:") {
		t.Fatalf("got %v, want a supply error", err)
	}
	if !strings.Contains(tr.String(), "supply unreadable") {
		t.Fatalf("transcript does not say the supply was lost:\n%s", tr.String())
	}
}
