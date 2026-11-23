package ndjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/can"
	"github.com/trukeio/ktest/record"
)

// The fields of the stream the tests record, by index: t is 0.
const (
	fState = 1 + iota
	fUpto
	fCause
	fVolts
	fF
	fOn
	fGated
	fBlob
	fZ
)

func mixSchema() *logb.Schema {
	return record.Schema("mix", 8*64, []logb.Field{
		{Name: "state", BitOffset: 64, BitWidth: 8, Type: logb.TypeUint, Hold: true,
			Conv: logb.ValueToText{Keys: []float64{0, 1}, Texts: []string{"ok", "dev"}, Default: "?"}},
		{Name: "upto", BitOffset: 128, BitWidth: 64, Type: logb.TypeSint, Unit: "ns", Hold: true},
		{Name: "cause", BitOffset: 192, BitWidth: 128, Type: logb.TypeString},
		{Name: "volts", BitOffset: 320, BitWidth: 16, Type: logb.TypeUint, Unit: "V", Conv: logb.Linear{A: 0, B: 0.5}},
		{Name: "f", BitOffset: 352, BitWidth: 32, Type: logb.TypeFloat},
		{Name: "on", BitOffset: 384, BitWidth: 1, Type: logb.TypeBool},
		{Name: "gated", BitOffset: 400, BitWidth: 16, Type: logb.TypeUint, Guarded: true, GuardField: fOn, GuardValue: 1},
		{Name: "blob", BitOffset: 416, BitWidth: 32, Type: logb.TypeBytes},
		{Name: "z", BitOffset: 448, BitWidth: 64, Type: logb.TypeComplex},
	}, map[string]string{"contract": "9f2c"})
}

type mixRec struct {
	at    time.Duration
	state byte
	upto  int64
	cause string
	volts uint16
	f     float32
	on    bool
	gated uint16
	blob  [4]byte
}

var t0 = time.Unix(1_795_172_700, 0)

// recordMix records recs into a Logb stream as a DUT rack would: one record
// per frame, segments of 100 ms, so a record after 100 ms opens a segment that
// restates the held fields.
func recordMix(t *testing.T, recs []mixRec) []byte {
	t.Helper()
	var buf bytes.Buffer
	now := t0
	w, err := record.New(&buf, record.Config{Bus: "vcan0", Stamp: can.StampTimestampNS, Codec: logb.CodecNone,
		PerFrame: 1, Live: true, Segment: 100 * time.Millisecond, Meta: map[string]string{"daemon.version": "test"},
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	st, err := w.NewStream(mixSchema())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		now = t0.Add(r.at)
		err := w.Write(now, st, func(rec []byte) {
			rec[8] = r.state
			putBits(rec, 128, 64, uint64(r.upto))
			copy(rec[24:40], r.cause)
			putBits(rec, 320, 16, uint64(r.volts))
			putBits(rec, 352, 32, uint64(math.Float32bits(r.f)))
			if r.on {
				rec[48] = 1
			}
			putBits(rec, 400, 16, uint64(r.gated))
			copy(rec[52:56], r.blob[:])
			putBits(rec, 448, 32, uint64(math.Float32bits(2)))
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var mixRecs = []mixRec{
	{at: 10 * time.Millisecond, state: 1, upto: 1795172712355000000, cause: `late: <&> "x"`, volts: 25, f: 1.5,
		on: true, gated: 7, blob: [4]byte{1, 2, 3, 4}},
	{at: 20 * time.Millisecond, state: 0, upto: -5, cause: "ñandú", volts: 0, f: float32(math.NaN())},
	{at: 150 * time.Millisecond, state: 1, upto: 1, cause: "", volts: 65535, f: float32(math.Inf(-1)), on: true, gated: 0},
}

func transcode(t *testing.T, lb []byte) []string {
	t.Helper()
	var out bytes.Buffer
	lines := 0
	w := NewWriter(&out)
	w.Flush = func() { lines++ }
	if err := Transcode(w, bytes.NewReader(lb)); err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if lines != len(got) {
		t.Fatalf("%d lines but %d flushes: a line per message, flushed (§7.1)", len(got), lines)
	}
	for _, l := range got {
		if !json.Valid([]byte(l)) {
			t.Fatalf("not JSON: %s", l)
		}
	}
	return got
}

// The lines a Logb stream becomes, as §7.8 writes them.
func TestTranscode(t *testing.T) {
	lines := transcode(t, recordMix(t, mixRecs))

	var meta map[string]map[string]string
	if err := json.Unmarshal([]byte(lines[0]), &meta); err != nil || meta["meta"] == nil {
		t.Fatalf("the first line is not the metadata: %s", lines[0])
	}
	if meta["meta"]["recording.epoch"] != "1795172700000000000" || meta["meta"]["daemon.version"] != "test" {
		t.Errorf("metadata %v", meta["meta"])
	}

	var schemas, recs, holds []string
	for _, l := range lines[1:] {
		switch {
		case strings.HasPrefix(l, `{"schema":`):
			schemas = append(schemas, l)
		case strings.HasPrefix(l, `{"rec":`):
			recs = append(recs, l)
		case strings.HasPrefix(l, `{"hold":`):
			holds = append(holds, l)
		case strings.HasPrefix(l, `{"meta":`):
			t.Errorf("metadata again, though every key was sent: %s", l)
		}
	}
	var mix []string
	for _, s := range schemas {
		if strings.Contains(s, `"schema":"mix"`) {
			mix = append(mix, s)
		}
	}
	if len(mix) != 1 {
		t.Fatalf("mix's schema sent %d times; once, though each segment restates it", len(mix))
	}
	wantSchema := `{"schema":"mix","fields":[` +
		`{"name":"state","type":"enum","bits":8,"held":true,"labels":{"0":"ok","1":"dev"}},` +
		`{"name":"upto","type":"sint","bits":64,"unit":"ns","held":true},` +
		`{"name":"cause","type":"string","max":16},` +
		`{"name":"volts","type":"float","unit":"V"},` +
		`{"name":"f","type":"float"},` +
		`{"name":"on","type":"bool"},` +
		`{"name":"gated","type":"uint","bits":16,"guard":{"field":"on","value":1}},` +
		`{"name":"blob","type":"bytes","max":4}],` +
		`"meta":{"contract":"9f2c"}}`
	if mix[0] != wantSchema {
		t.Errorf("schema\n got %s\nwant %s", mix[0], wantSchema)
	}

	want := []string{
		`{"rec":"mix","t":"10000000","v":{"state":"dev","upto":"1795172712355000000","cause":"late: <&> \"x\"","volts":12.5,"f":1.5,"on":true,"gated":7,"blob":"AQIDBA=="}}`,
		`{"rec":"mix","t":"20000000","v":{"state":"ok","upto":"-5","cause":"ñandú","volts":0,"f":null,"on":false,"blob":"AAAAAA=="}}`,
		`{"rec":"mix","t":"150000000","v":{"state":"dev","upto":"1","cause":"","volts":32767.5,"f":null,"on":true,"gated":0,"blob":"AAAAAA=="}}`,
	}
	var mixRecsGot []string
	for _, r := range recs {
		if strings.HasPrefix(r, `{"rec":"mix"`) {
			mixRecsGot = append(mixRecsGot, r)
		}
	}
	if strings.Join(mixRecsGot, "\n") != strings.Join(want, "\n") {
		t.Errorf("records\n got %s\nwant %s", strings.Join(mixRecsGot, "\n"), strings.Join(want, "\n"))
	}

	// The segment the third record opens restates what the second held.
	wantHold := `{"hold":"mix","t":"20000000","v":{"state":"ok","upto":"-5"}}`
	found := false
	for _, h := range holds {
		found = found || h == wantHold
	}
	if !found {
		t.Errorf("no %s among the holds %v", wantHold, holds)
	}
}

// What a master reads from the NDJSON is what it reads from the Logb, value
// for value (§7.9), by field name.
func TestRoundTrip(t *testing.T) {
	lb := recordMix(t, mixRecs)
	lines := transcode(t, lb)

	type row map[string]any
	read := func(b *logb.Batch, i int, conv map[string]bool) row {
		r := row{}
		for f := range b.Schema.Fields {
			fd := &b.Schema.Fields[f]
			if f == 0 || fd.Type == logb.TypeComplex {
				continue
			}
			var v any
			var err error
			if conv[fd.Name] {
				v, err = b.Value(i, f)
			} else {
				v, err = b.Raw(i, f)
			}
			if errors.Is(err, logb.ErrFieldAbsent) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if x, ok := v.(float64); ok && math.IsInf(x, 0) {
				v = math.NaN() // §7.8: not finite is null, read back as NaN
			}
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			r[fd.Name] = v
		}
		ax, _ := b.Axis(i)
		d, _ := ax.Duration(b.Schema.AxisExp)
		r["t"] = int64(d)
		return r
	}

	var want []row
	lr, err := logb.NewReader(bytes.NewReader(lb))
	if err != nil {
		t.Fatal(err)
	}
	for {
		b, err := lr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if b.Schema.Name != "mix" {
			continue
		}
		for i := 0; i < int(b.Count); i++ {
			want = append(want, read(b, i, map[string]bool{"volts": true}))
		}
	}

	var got []row
	nr := NewReader(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	for {
		m, err := nr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if m.Kind != KindRecord || m.Schema.Name != "mix" {
			continue
		}
		if err := m.Schema.Validate(); err != nil {
			t.Fatalf("the schema the reader built is not one Logb writes: %v", err)
		}
		b := &logb.Batch{Schema: m.Schema, Count: 1, Data: m.Rec}
		got = append(got, read(b, 0, nil))
	}
	if len(got) != len(want) {
		t.Fatalf("%d records read back, %d written", len(got), len(want))
	}
	for i := range want {
		for k, w := range want[i] {
			g, ok := got[i][k]
			if !ok {
				t.Errorf("record %d: %s missing", i, k)
				continue
			}
			if !same(g, w) {
				t.Errorf("record %d: %s = %v (%T), want %v (%T)", i, k, g, g, w, w)
			}
		}
		for k := range got[i] {
			if _, ok := want[i][k]; !ok {
				t.Errorf("record %d: %s present, absent in the Logb", i, k)
			}
		}
	}
	if fmtMeta(nr.Meta) != fmtMeta(lr.Meta) {
		t.Errorf("metadata %s, want %s", fmtMeta(nr.Meta), fmtMeta(lr.Meta))
	}
}

func same(a, b any) bool {
	if x, ok := a.(float64); ok {
		y, ok := b.(float64)
		return ok && (x == y || math.IsNaN(x) && math.IsNaN(y))
	}
	return a == b
}

func fmtMeta(m []logb.Meta) string {
	seen := map[string]string{}
	for _, kv := range m {
		if _, ok := seen[kv.Key]; !ok {
			seen[kv.Key] = kv.Value
		}
	}
	b, _ := json.Marshal(seen)
	return string(b)
}

// §7.8's example, line by line, as the reader reads it.
func TestReaderExample(t *testing.T) {
	in := `{"meta":{"recording.epoch":"1795172700000000000","peer.protocol":"1","contract":"acme-bcm-v3"}}
{"schema":"mon.alive","fields":[{"name":"state","type":"enum","bits":8,"held":true,"labels":{"0":"ok","1":"dev","2":"unknown","3":"inactive","4":"settling"}},{"name":"from","type":"enum","bits":8,"labels":{"0":"ok","1":"dev","2":"unknown","3":"inactive","4":"settling"}},{"name":"cause","type":"string","max":64}],"meta":{"contract":"9f2c…"}}
{"schema":"mon.watermark","fields":[{"name":"upto","type":"sint","bits":64,"unit":"ns","held":true}]}
{"hold":"mon.alive","t":"11870000000","v":{"state":"ok"}}
{"future":"a message kind this reader does not know"}
{"rec":"mon.alive","t":"12345678901","v":{"state":"dev","from":"ok","cause":"DutStatus.counter: delta 2","extra":1}}
{"rec":"mon.watermark","t":"12355000000","v":{"upto":"1795172712355000000"}}
`
	r := NewReader(strings.NewReader(in))
	var kinds []Kind
	var last Message
	for {
		m, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, m.Kind)
		if m.Kind == KindRecord && m.Schema.Name == "mon.alive" {
			last = m
		}
		if m.Kind == KindRecord && m.Schema.Name == "mon.watermark" {
			b := &logb.Batch{Schema: m.Schema, Count: 1, Data: m.Rec}
			if v, _ := b.Raw(0, 1); v != int64(1795172712355000000) {
				t.Errorf("upto %v", v)
			}
		}
	}
	if want := []Kind{KindMeta, KindSchema, KindSchema, KindHold, KindRecord, KindRecord}; len(kinds) != len(want) {
		t.Fatalf("kinds %v, want %v", kinds, want)
	}
	if last.T != 12345678901 {
		t.Errorf("t %d", last.T)
	}
	b := &logb.Batch{Schema: last.Schema, Count: 1, Data: last.Rec}
	for f, want := range map[int]any{1: uint64(1), 2: uint64(0), 3: "DutStatus.counter: delta 2"} {
		if v, _ := b.Raw(0, f); v != want {
			t.Errorf("field %s = %v, want %v", last.Schema.Fields[f].Name, v, want)
		}
	}
	if v, _ := b.Value(0, 1); v != "dev" {
		t.Errorf("state reads %v through its labels", v)
	}
	if last.Rec[8] != 1 {
		t.Errorf("state is not at byte 8, where a Logb mon stream has it: % x", last.Rec[:10])
	}
}

func TestReaderRefuses(t *testing.T) {
	for _, in := range []string{
		`not json`,
		`{"rec":"mon.alive","t":"1","v":{}}`,
		`{"schema":"s","fields":[{"name":"e","type":"enum","bits":8,"labels":{"0":"ok"}}]}` + "\n" +
			`{"rec":"s","t":"1","v":{"e":"nope"}}`,
		`{"schema":"s","fields":[{"name":"n","type":"uint","bits":65}]}`,
	} {
		r := NewReader(strings.NewReader(in + "\n"))
		var err error
		for err == nil {
			_, err = r.Next()
		}
		if errors.Is(err, io.EOF) {
			t.Errorf("%q read without complaint", in)
		}
	}
}
