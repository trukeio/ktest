package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/can"
	"github.com/trukeio/ktest/conformance"
	"github.com/trukeio/ktest/record"
)

// A master's recording as ktestd writes the part -faults reads: its rules,
// its peer's clock, and the watermark age it declares. The DUT rack's clock
// runs 40 ms ahead of the master's here, so the fault times, which are the
// rack's, must be moved onto the master's to match.
func faultRecording(t *testing.T, t0 time.Time, rules []ruleEvent) string {
	t.Helper()
	var buf bytes.Buffer
	w, err := record.New(&buf, record.Config{Bus: "vcan1", Stamp: can.StampTimestampNS, Codec: logb.CodecNone,
		Meta: map[string]string{"rules.monitoring_lost.watermark_age": "100ms"}, Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := w.NewStream(record.Schema("rules", 8*48, []logb.Field{
		{Name: "seq", BitOffset: 64, BitWidth: 32, Type: logb.TypeUint},
		{Name: "event", BitOffset: 96, BitWidth: 8, Type: logb.TypeUint},
		{Name: "rule", BitOffset: 128, BitWidth: 256, Type: logb.TypeString},
	}, nil))
	if err != nil {
		t.Fatal(err)
	}
	cs, err := w.NewStream(record.Schema("ref.clock", 8*32, []logb.Field{
		{Name: "offset", BitOffset: 64, BitWidth: 64, Type: logb.TypeSint, Unit: "ns"},
		{Name: "uncertainty", BitOffset: 128, BitWidth: 64, Type: logb.TypeSint, Unit: "ns"},
		{Name: "rtt", BitOffset: 192, BitWidth: 64, Type: logb.TypeSint, Unit: "ns"},
	}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(t0, cs, func(rec []byte) {
		binary.LittleEndian.PutUint64(rec[8:], uint64(40*time.Millisecond))
		binary.LittleEndian.PutUint64(rec[16:], uint64(100*time.Microsecond))
	}); err != nil {
		t.Fatal(err)
	}
	for _, e := range rules {
		if err := w.Write(time.Unix(0, e.t), rs, func(rec []byte) {
			rec[12] = byte(e.event)
			copy(rec[16:48], e.rule)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "lab.logb")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFaults(t *testing.T) {
	t0 := time.Unix(1_795_000_000, 0)
	// On the rack's clock: an abort from 1 s to 2 s, a frozen rack from 3 s
	// to 4 s. On the master's, 40 ms earlier.
	ff := conformance.FaultFile{Faults: []conformance.Fault{
		{Kind: conformance.FaultAbort, Start: t0.Add(1 * time.Second), End: t0.Add(2 * time.Second)},
		{Kind: conformance.FaultFrozen, Start: t0.Add(3 * time.Second), End: t0.Add(4 * time.Second)},
	}}
	fp := filepath.Join(t.TempDir(), "faults.json")
	b, _ := json.Marshal(ff)
	if err := os.WriteFile(fp, b, 0o644); err != nil {
		t.Fatal(err)
	}
	at := func(ms int) int64 { return t0.Add(time.Duration(ms)*time.Millisecond - 40*time.Millisecond).UnixNano() }
	good := []ruleEvent{
		{at(1020), 1, "dut_abort"}, {at(2020), 3, "dut_abort"},
		{at(3110), 1, "monitoring_lost"}, {at(4010), 3, "monitoring_lost"},
	}
	for _, c := range []struct {
		name  string
		rules []ruleEvent
		want  string
	}{
		{"a master that protects", good, ""},
		{"too slow on an abort", []ruleEvent{{at(1300), 1, "dut_abort"}, good[1], good[2], good[3]},
			"took hold 300.0 ms after it began, beyond 250ms"},
		{"never reacts to the frozen rack", good[:2], "monitoring_lost never took hold"},
		{"slow beyond the watermark age and the reaction time", []ruleEvent{good[0], good[1],
			{at(3400), 1, "monitoring_lost"}, {at(4010), 3, "monitoring_lost"}}, "beyond 350ms"},
		{"clears while the abort lasts", []ruleEvent{{at(1020), 1, "dut_abort"}, {at(1500), 3, "dut_abort"}, good[2], good[3]},
			"cleared 500.0 ms before it ended"},
		{"never clears", []ruleEvent{good[0], good[2], good[3]}, "dut_abort never cleared"},
		{"held from before", []ruleEvent{{at(500), 1, "dut_abort"}, good[1], good[2], good[3]}, "already held"},
	} {
		p := faultRecording(t, t0, c.rules)
		n, _, err := compareFaults(p, fp, "", 250*time.Millisecond, 5*time.Second, true)
		switch {
		case c.want == "" && (err != nil || n != 2):
			t.Errorf("%s: %d, %v", c.name, n, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}
