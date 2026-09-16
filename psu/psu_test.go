package psu

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// start runs a two-channel simulator on a loopback port and dials it.
func start(t *testing.T) (*Supply, *Sim, *lockedBuf, string) {
	t.Helper()
	log := &lockedBuf{}
	sim := &Sim{Model: "psusim", Transcript: log, Channels: []SimChannel{
		{VMax: 30, IMax: 3, Load: 10},
		{VMax: 6, IMax: 5, Load: 100},
	}}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go sim.Serve(l)
	s, err := Dial(l.Addr().String(), Keysight, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, sim, log, l.Addr().String()
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

func TestIdentify(t *testing.T) {
	s, _, _, _ := start(t)
	id, err := s.Identify()
	if err != nil || id != "ktest,psusim,0,0.1" {
		t.Fatalf("*IDN? = %q, %v", id, err)
	}
}

// TestRegulation: constant voltage while the load draws less than the current
// setpoint, constant current once it would draw more, nothing with the output
// off — the measured account comes from the load, not from the setpoints.
func TestRegulation(t *testing.T) {
	s, _, _, _ := start(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SetVoltage(1, 5))
	must(s.SetCurrent(1, 1))
	if v, i := meas(t, s, 1); v != 0 || i != 0 {
		t.Errorf("output off: measured %v V, %v A", v, i)
	}
	must(s.SetOutput(1, true))
	if on, err := s.Output(1); err != nil || !on {
		t.Fatalf("output read back %v, %v", on, err)
	}
	if v, i := meas(t, s, 1); !near(v, 5) || !near(i, 0.5) {
		t.Errorf("5 V into 10 ohm under a 1 A limit: %v V, %v A, want constant voltage 5 V, 0.5 A", v, i)
	}
	must(s.SetCurrent(1, 0.2))
	if v, i := meas(t, s, 1); !near(v, 2) || !near(i, 0.2) {
		t.Errorf("5 V into 10 ohm under a 0.2 A limit: %v V, %v A, want constant current 2 V, 0.2 A", v, i)
	}
	must(s.SetOutput(1, false))
	if v, i := meas(t, s, 1); v != 0 || i != 0 {
		t.Errorf("output switched off: measured %v V, %v A", v, i)
	}
}

func meas(t *testing.T, s *Supply, ch int) (float64, float64) {
	t.Helper()
	v, err := s.MeasureVoltage(ch)
	if err != nil {
		t.Fatal(err)
	}
	i, err := s.MeasureCurrent(ch)
	if err != nil {
		t.Fatal(err)
	}
	return v, i
}

// TestClamp: a setpoint beyond the rating is applied as the rating, and the
// only word of it is in the error queue — the case §4 keeps set and applied
// apart for.
func TestClamp(t *testing.T) {
	s, _, _, _ := start(t)
	if err := s.SetVoltage(2, 12); err != nil {
		t.Fatal(err)
	}
	v, err := s.Voltage(2)
	if err != nil || v != 6 {
		t.Fatalf("12 V set on a 6 V channel reads back as %v, %v; want 6", v, err)
	}
	errs, err := s.Errors()
	if err != nil || len(errs) != 1 || !strings.HasPrefix(errs[0], "-222") {
		t.Fatalf("errors %q, %v; want one -222", errs, err)
	}
	if errs, _ := s.Errors(); len(errs) != 0 {
		t.Errorf("the queue was not emptied: %q", errs)
	}
}

func TestBadCommands(t *testing.T) {
	s, sim, _, addr := start(t)
	// Dial's *CLS travels over the connection; a round trip makes sure it has
	// been handled before errors are queued behind its back.
	if _, err := s.Identify(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"FROB 1", "VOLT 5", "VOLT 5,(@3)", "OUTP MAYBE,(@1)", "VOLT lots,(@1)"} {
		sim.Exec(c)
	}
	errs, err := s.Errors()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-113", "-109", "-222", "-224", "-224"}
	if len(errs) != len(want) {
		t.Fatalf("errors %q, want codes %v", errs, want)
	}
	for i, w := range want {
		if !strings.HasPrefix(errs[i], w) {
			t.Errorf("error %d is %q, want %s", i, errs[i], w)
		}
	}

	// Several commands on one line, as SCPI allows, answered in order.
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "VOLT 1.5,(@1);VOLT? (@1);OUTP? (@1)\n")
	r := bufio.NewReader(c)
	a, _ := r.ReadString('\n')
	b, _ := r.ReadString('\n')
	if strings.TrimSpace(a) != "1.500000E+00" || strings.TrimSpace(b) != "0" {
		t.Errorf("answers %q %q", a, b)
	}
}

func TestTranscript(t *testing.T) {
	s, _, log, _ := start(t)
	s.SetVoltage(1, 3.3)
	s.Voltage(1)
	got := log.String()
	for _, want := range []string{"*CLS", "VOLT 3.3,(@1)", "VOLT? (@1)"} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript lacks %q:\n%s", want, got)
		}
	}
}
