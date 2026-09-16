//go:build linux

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"testing"
	"time"

	"github.com/rveen/logb"

	dso "github.com/rveen/ktest/scope"
)

// The scope's record layouts are byte offsets written in one place and read in
// another — the constants above the schema, and the schema's own field offsets.
// Nothing checks that the two agree except this: a field declared at the wrong
// byte decodes to a plausible number from the neighbouring one, which is the
// silent wrong answer §5 exists to refuse, and it looks entirely fine.
//
// So every record is written as the daemon writes it and read back through
// logb, and the values must be the ones that went in.

// readBack writes one record of s and decodes it, returning the fields by name.
// A field the record does not carry — a guard that does not hold — is absent
// from the map, which is the distinction the test exists to make.
func readBack(t *testing.T, s *logb.Schema, rec []byte) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	w, err := logb.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteData(s, logb.TickVal(0), 0, 1, rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := logb.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for {
		b, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for f, fd := range b.Schema.Fields {
			v, err := b.Value(0, f)
			if errors.Is(err, logb.ErrFieldAbsent) {
				continue
			}
			if err != nil {
				t.Fatalf("%s: %v", fd.Name, err)
			}
			out[fd.Name] = v
		}
	}
	return out
}

func TestVertRecordLayout(t *testing.T) {
	s := vertSchema("scope1", 1)
	rec := make([]byte, s.RecordBytes())
	le := binary.LittleEndian
	le.PutUint32(rec[svSeq:], 42)
	rec[svEvent] = evApplied
	// on.set, on.applied, connected, vdiv.known, ofst.known, on.known.
	rec[svFlags] = 1 | 2 | 4 | 8 | 16 | 32
	le.PutUint64(rec[svVDivSet:], math.Float64bits(0.3))
	le.PutUint64(rec[svOfstSet:], math.Float64bits(-1.5))
	le.PutUint64(rec[svVDivApp:], math.Float64bits(0.2))
	le.PutUint64(rec[svOfstApp:], math.Float64bits(-1.5))
	copy(rec[svErr:], "not answering")

	got := readBack(t, s, rec)
	for name, want := range map[string]any{
		"seq": uint64(42), "event": uint64(evApplied),
		"connected": true, "vdiv.known": true, "ofst.known": true, "on.known": true,
		"vdiv.set": 0.3, "ofst.set": -1.5, "on.set": true,
		"vdiv.applied": 0.2, "ofst.applied": -1.5, "on.applied": true,
	} {
		if got[name] != want {
			t.Errorf("%s: %v, want %v", name, got[name], want)
		}
	}
	if s, _ := got["error"].(string); s != "not answering" {
		t.Errorf("error: %q", s)
	}

	// And the guards: with nothing known and the scope not answering, every
	// account is absent rather than zero. A setpoint nobody has set is not
	// 0 V, and the difference is the whole of §6.2.
	clear(rec)
	got = readBack(t, s, rec)
	for _, name := range []string{"vdiv.set", "ofst.set", "on.set", "vdiv.applied", "ofst.applied", "on.applied"} {
		if v, ok := got[name]; ok {
			t.Errorf("%s came back as %v, and nothing has been set or read", name, v)
		}
	}
}

func TestTimebaseRecordLayout(t *testing.T) {
	s := timebaseSchema("scope1")
	rec := make([]byte, s.RecordBytes())
	le := binary.LittleEndian
	le.PutUint32(rec[stSeq:], 7)
	rec[stEvent] = evReadback
	rec[stFlags] = 1 | 2 | 4 | 8 // connected, tdiv.known, trdl.known, msiz.known
	le.PutUint64(rec[stTDivSet:], math.Float64bits(2e-4))
	le.PutUint64(rec[stTrdlSet:], math.Float64bits(-1e-3))
	le.PutUint64(rec[stMsizSet:], math.Float64bits(700e3))
	le.PutUint64(rec[stTDivApp:], math.Float64bits(2e-4))
	le.PutUint64(rec[stTrdlApp:], math.Float64bits(-1e-3))
	le.PutUint64(rec[stMsizApp:], math.Float64bits(700e3))
	le.PutUint64(rec[stSara:], math.Float64bits(2.5e8))

	got := readBack(t, s, rec)
	for name, want := range map[string]any{
		"seq": uint64(7), "event": uint64(evReadback), "connected": true,
		"tdiv.set": 2e-4, "trdl.set": -1e-3, "msiz.set": 700e3,
		"tdiv.applied": 2e-4, "trdl.applied": -1e-3, "msiz.applied": 700e3, "sara": 2.5e8,
	} {
		if got[name] != want {
			t.Errorf("%s: %v, want %v", name, got[name], want)
		}
	}
}

// The acquisition record is the one with a guard on a flag bit, and the guard
// is an index into the field list that nothing but arithmetic keeps right. It
// was wrong once — it pointed one field past its flag, so the scope's own frame
// time was absent from every record while the flag beside it said it was there.
func TestAcqRecordLayout(t *testing.T) {
	s := acqSchema("scope1")
	rec := make([]byte, s.RecordBytes())
	le := binary.LittleEndian
	le.PutUint32(rec[saRun:], 9)
	rec[saChan] = 2
	rec[saEvent] = acqRecorded
	rec[saSign] = byte(dso.SignSub255)
	rec[saFlags] = 1 | 2 | 4 | 8 // trigger_delay, full_rate, frame_time.known, exact
	le.PutUint32(rec[saPoints:], 14000)
	le.PutUint32(rec[saBuckets:], 3000)
	le.PutUint32(rec[saFrame:], 3)
	for at, v := range map[uint32]float64{
		saVDiv: 0.5, saOfst: -0.25, saTDiv: 1e-3, saSara: 1e6, saTrdl: 2e-3, saMsiz: 14e3,
		saStep: 1e-6, saFirst: -7e-3, saFTime: 312.650814, saReadout: 0.0053,
	} {
		le.PutUint64(rec[at:], math.Float64bits(v))
	}

	got := readBack(t, s, rec)
	for name, want := range map[string]any{
		"run": uint64(9), "channel": uint64(2), "event": uint64(acqRecorded), "sign": uint64(1),
		"trigger_delay": true, "full_rate": true, "frame_time.known": true, "exact": true,
		"points": uint64(14000), "buckets": uint64(3000), "frame": uint64(3),
		"vdiv": 0.5, "ofst": -0.25, "tdiv": 1e-3, "sara": 1e6, "trdl": 2e-3, "msiz": 14e3,
		"step": 1e-6, "first": -7e-3, "frame_time": 312.650814, "readout": 0.0053,
	} {
		if got[name] != want {
			t.Errorf("%s: %v, want %v", name, got[name], want)
		}
	}

	// A plain single acquisition carries no trigger time at all, and the field
	// must then be absent rather than zero: zero on the scope's clock is a
	// time, not a way of saying there is none.
	rec[saFlags] &^= 4
	got = readBack(t, s, rec)
	if v, ok := got["frame_time"]; ok {
		t.Errorf("frame_time came back as %v with frame_time.known clear", v)
	}
	if got["frame_time.known"] != false {
		t.Errorf("frame_time.known: %v", got["frame_time.known"])
	}
}

// The envelope's layout is written by appendEnvelope and declared by envSchema,
// which are two descriptions of the same bytes. A bucket that held no sample
// must come back absent: a zero volt there is a measurement nobody made, and is
// exactly what min/max decimation exists to avoid inventing.
func TestEnvelopeRecordLayout(t *testing.T) {
	e := &dso.Envelope{
		Min: []float32{-1.25, float32(math.NaN()), 0},
		Max: []float32{2.5, float32(math.NaN()), 0},
		N:   []uint32{7, 0, 1},
	}
	buf := appendEnvelope(nil, e)
	if len(buf) != 3*seBytes {
		t.Fatalf("%d bytes for 3 buckets of %d", len(buf), seBytes)
	}
	s := envSchema("scope1", 1, time.Microsecond)
	for i, want := range []struct {
		present  bool
		n        uint64
		min, max float64
	}{
		{true, 7, -1.25, 2.5},
		{false, 0, 0, 0},
		{true, 1, 0, 0},
	} {
		got := readBack(t, s, buf[i*seBytes:(i+1)*seBytes])
		if got["present"] != want.present || got["n"] != want.n {
			t.Errorf("bucket %d: present=%v n=%v, want %v and %d", i, got["present"], got["n"], want.present, want.n)
		}
		mn, hasMin := got["min"]
		mx, hasMax := got["max"]
		if hasMin != want.present || hasMax != want.present {
			t.Errorf("bucket %d: min present %v, max present %v, want %v", i, hasMin, hasMax, want.present)
			continue
		}
		if want.present && (mn != want.min || mx != want.max) {
			t.Errorf("bucket %d: [%v %v], want [%v %v]", i, mn, mx, want.min, want.max)
		}
	}
}

// The -scope flag names an instrument, its channels and which of them the
// recording keeps at full rate (§10.4).
func TestScopeFlag(t *testing.T) {
	var f scopeFlags
	if err := f.Set("scope1=192.168.1.30:5025,2,full=ch1+ch2"); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("scope2=host:5025"); err != nil {
		t.Fatal(err)
	}
	if got := f[0]; got.name != "scope1" || got.addr != "192.168.1.30:5025" || got.channels != 2 ||
		!got.full[1] || !got.full[2] {
		t.Errorf("scope1 parsed as %+v", got)
	}
	// Two channels by default, and full rate off: continuous full-rate capture
	// is a decision someone made, never a default (§10.4).
	if got := f[1]; got.channels != 2 || len(got.full) != 0 {
		t.Errorf("scope2 parsed as %+v", got)
	}
	for _, bad := range []string{
		"scope1=host:5025",            // named twice
		"scope3",                      // no address
		"scope3=host:5025,0",          // no channels
		"scope3=host:5025,2,full=ch3", // a channel it has not
		"scope3=host:5025,2,nosuch=1", // an option that is not one
		"has.a.dot=host:5025",         // a name that would break a channel's
	} {
		if err := f.Set(bad); err == nil {
			t.Errorf("-scope %q was accepted", bad)
		}
	}
}

// A channel names the instrument, the part of it, the quantity and the account
// (§4). The part is what differs between instruments, and chanOf is what tells
// a scope's timebase from one of its channels.
func TestParseChannelParts(t *testing.T) {
	for _, c := range []struct {
		in               string
		inst, part, what string
		ch               int
	}{
		{"psu1.ch2.v.set", "psu1", "ch2", "v", 2},
		{"psu1.ch10.out.set", "psu1", "ch10", "out", 10},
		{"scope1.ch1.vert.vdiv.set", "scope1", "ch1.vert", "vdiv", 1},
		{"scope1.timebase.tdiv.set", "scope1", "timebase", "tdiv", 0},
	} {
		cn, err := parseChannel(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if cn.inst != c.inst || cn.part != c.part || cn.what != c.what {
			t.Errorf("%s: %+v", c.in, cn)
		}
		if got := chanOf(cn.part); got != c.ch {
			t.Errorf("%s: channel %d, want %d", c.in, got, c.ch)
		}
		if cn.String() != c.in {
			t.Errorf("%s came back as %s", c.in, cn)
		}
	}
	for _, bad := range []string{"psu1.ch1.v", "psu1.ch1.v.applied", "v.set", "psu1..v.set", "psu1.ch1..set"} {
		if _, err := parseChannel(bad); err == nil {
			t.Errorf("%q was accepted as a channel that can be set", bad)
		}
	}
}
