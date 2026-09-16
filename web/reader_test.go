//go:build linux

package web

// The rack decodes Logb in the browser, with its own reader in
// src/logb/reader.ts. These tests hold that reader to logb's Go reader: both
// read the same recordings and must return the same values, in the same order,
// whether the bytes arrive whole or in pieces. They need node, and skip
// without it.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rveen/logb"
	"github.com/rveen/logb/dbc"

	"github.com/rveen/ktest/can"
	"github.com/rveen/ktest/record"
)

func TestReaderAgreesWithGo(t *testing.T) {
	node := needNode(t)
	dir := t.TempDir()
	for _, fx := range []struct {
		name  string
		write func(t *testing.T) []byte
	}{
		{"ktestd", writeKtestdFixture},
		{"spec", writeSpecFixture},
		{"scope", writeScopeFixture},
	} {
		path := filepath.Join(dir, fx.name+".logb")
		if err := os.WriteFile(path, fx.write(t), 0o644); err != nil {
			t.Fatal(err)
		}
		want := goDump(t, path)
		recs := 0
		for _, l := range want {
			if strings.Contains(l, `"k":"rec"`) || strings.Contains(l, `"k":"hold"`) {
				recs++
			}
		}
		for _, chunk := range []string{"0", "1", "7", "random"} {
			got := nodeDump(t, node, path, chunk)
			compareDumps(t, fx.name+" in pieces of "+chunk, got, want)
		}
		t.Logf("%s: %d records and restatements agree, fed whole, a byte at a time, 7 at a time and at random",
			fx.name, recs)
	}
}

// TestReaderRefusesCodecs: a DATA frame compressed with a codec the browser
// reader does not carry is refused, and reported, not misread — and the frames
// around it are read as usual.
func TestReaderRefusesCodecs(t *testing.T) {
	node := needNode(t)
	var buf bytes.Buffer
	w, err := logb.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	s := &logb.Schema{UUID: uid("zstd"), Name: "z", RecordBits: 32, AxisKind: logb.AxisTime,
		AxisMode: logb.AxisImplicit, AxisExp: -3, AxisUnit: "s", AxisStep: logb.TickVal(1),
		Fields: []logb.Field{{Name: "v", BitOffset: 0, BitWidth: 32, Type: logb.TypeUint}}}
	must(t, w.AddStream(s))
	recs := func(vs ...uint32) []byte {
		b := make([]byte, 4*len(vs))
		for i, v := range vs {
			binary.LittleEndian.PutUint32(b[4*i:], v)
		}
		return b
	}
	w.Codec = logb.CodecNone
	must(t, w.WriteData(s, logb.TickVal(0), 0, 2, recs(1, 2)))
	w.Codec = logb.CodecZstd
	must(t, w.WriteData(s, logb.TickVal(2), 0, 3, recs(3, 4, 5)))
	w.Codec = logb.CodecNone
	must(t, w.WriteData(s, logb.TickVal(5), 0, 1, recs(6)))
	must(t, w.Close())
	path := filepath.Join(t.TempDir(), "z.logb")
	must(t, os.WriteFile(path, buf.Bytes(), 0o644))

	got := strings.Join(nodeDump(t, node, path, "0"), "\n")
	for _, want := range []string{`"v":{"i":"1"}`, `"v":{"i":"2"}`, `"v":{"i":"6"}`, `{"k":"refused","n":1}`,
		`{"k":"truncated","v":false}`} {
		if !strings.Contains(got, want) {
			t.Errorf("want %s in:\n%s", want, got)
		}
	}
	for _, v := range []string{"3", "4", "5"} {
		if strings.Contains(got, `"v":{"i":"`+v+`"}`) {
			t.Errorf("record %s of a zstd frame was read:\n%s", v, got)
		}
	}
}

// ---- the recordings ----

const fixtureDBC = `VERSION "fixture"

BO_ 256 EngineData: 8 ECM
 SG_ EngineSpeed : 0|16@1+ (0.25,0) [0|16383.75] "rpm"  Tester
 SG_ CoolantTemp : 16|8@1+ (1,-40) [-40|215] "degC"  Tester
 SG_ EngineRunning : 37|1@1+ (1,0) [0|1] ""  Tester

BO_ 512 VehicleStatus: 8 ABS
 SG_ VehicleSpeed : 7|16@0+ (0.01,0) [0|655.35] "km/h"  Tester
 SG_ Odometer : 23|24@0+ (0.1,0) [0|1677721.5] "km"  Tester
 SG_ Gear : 40|4@1+ (1,0) [0|15] ""  Tester

BO_ 768 Mux: 8 ECM
 SG_ Selector M : 0|8@1+ (1,0) [0|255] ""  Tester
 SG_ Temperature m1 : 8|16@1+ (0.1,-40) [-40|100] "degC"  Tester
 SG_ Pressure m2 : 8|16@1- (0.5,0) [-1000|1000] "kPa"  Tester

VAL_ 512 Gear 0 "Neutral" 1 "First" 2 "Second" 15 "Reverse" ;
`

// writeKtestdFixture is a recording in the shape ktestd writes the rack's
// stream: codec none, every stream record declares, and short segments so that
// held values are restated several times over.
func writeKtestdFixture(t *testing.T) []byte {
	db, err := dbc.Parse(strings.NewReader(fixtureDBC))
	must(t, err)
	t0 := time.Unix(1789000000, 0)
	ticks := 0
	var buf bytes.Buffer
	w, err := record.New(&buf, record.Config{
		Bus: "vcan0", DB: db, Stamp: can.StampTimestampNS, Codec: logb.CodecNone,
		Segment: 50 * time.Millisecond, PerFrame: 16, TX: true,
		Meta:   map[string]string{"daemon.version": "fixture"},
		Attach: map[string][]byte{"panel.json": []byte(`{"widgets":[]}`), "limits.json": []byte(`{"min_period":"1ms"}`)},
		Now: func() time.Time {
			ticks++
			return t0.Add(time.Duration(ticks) * time.Microsecond)
		},
	})
	must(t, err)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	enc := func(msg string, v map[string]any) []byte {
		p, err := db.Message(msg).Encode(v)
		must(t, err)
		return p
	}
	frame := func(f *can.Frame, ms int, local bool) *can.Frame {
		f.Time, f.Stamped, f.Local = at(ms), true, local
		return f
	}
	gears := []string{"Neutral", "First", "Second", "Reverse"}
	seq := uint32(0)

	for k := 0; k < 200; k++ {
		must(t, w.Frame(frame(can.New(0x100, enc("EngineData", map[string]any{
			"EngineSpeed": float64(k) * 12.25, "CoolantTemp": float64(k%100) - 20, "EngineRunning": k%3 == 0,
		})), k, k%2 == 0)))
		if k%4 == 1 {
			must(t, w.Frame(frame(can.New(0x200, enc("VehicleStatus", map[string]any{
				"VehicleSpeed": float64(k) * 1.37, "Odometer": 1000 + float64(k)*0.1, "Gear": gears[k/4%4],
			})), k, false)))
		}
		if k%5 == 2 {
			v := map[string]any{"Selector": 1.0, "Temperature": float64(k%140) - 40}
			if k%10 == 7 {
				v = map[string]any{"Selector": 2.0, "Pressure": float64(k) - 100}
			}
			must(t, w.Frame(frame(can.New(0x300, enc("Mux", v)), k, false)))
		}
		if k%7 == 3 {
			must(t, w.Frame(frame(can.New(0x80000000|0x1ABCDEF, []byte{byte(k), 2, 3}), k, false)))
		}
		if k%11 == 5 {
			p := make([]byte, 16)
			for i := range p {
				p[i] = byte(k + i)
			}
			f := can.New(0x123, p)
			f.Flags = can.FlagBRS
			must(t, w.Frame(frame(f, k, true)))
		}
		if k == 50 {
			must(t, w.Frame(frame(can.New(0x20000000|0x4, make([]byte, 8)), k, false)))
		}
		if k%13 == 0 {
			f := can.New(0x7FF, []byte{0xAA})
			must(t, w.Frame(f)) // unstamped: the recorder's clock stands in
		}
		if k%50 == 10 {
			seq++
			f := can.New(0x124, []byte{byte(k)})
			must(t, w.Requested(seq, f, at(k), nil))
			e := frame(can.New(0x124, []byte{byte(k)}), k, true)
			e.Time, e.Confirmed = e.Time.Add(50*time.Microsecond), true
			must(t, w.Applied(seq, e))
		}
		if k == 60 {
			seq++
			must(t, w.Requested(seq, can.New(0x125, make([]byte, 12)), at(k), syscall.EMSGSIZE))
		}
		switch k {
		case 20:
			must(t, w.Control(at(k), &record.Control{Seq: 1, Request: record.ControlLeaseTake, Holder: "rolf",
				By: "rolf", Via: record.ViaTLS, TTL: 30 * time.Second}))
		case 21:
			must(t, w.Control(at(k), &record.Control{Seq: 2, Request: record.ControlArm, Armed: true,
				Holder: "rolf", By: "rolf", Via: record.ViaTLS, TTL: 30 * time.Second}))
		case 22:
			must(t, w.Control(at(k), &record.Control{Seq: 3, Request: record.ControlSend, Verdict: record.VerdictForbidden,
				Armed: true, Holder: "rolf", By: "dash", Via: record.ViaTLS, TTL: 30 * time.Second,
				Frame: can.New(0x100, []byte{1})}))
		}
		task := can.New(0x100, enc("EngineData", map[string]any{"EngineSpeed": 1000.0, "CoolantTemp": 90.0, "EngineRunning": true}))
		switch k {
		case 30:
			must(t, w.Cyclic(at(k), &record.Cyclic{Seq: 4, Event: record.CyclicStart, Active: true,
				Set:     record.CyclicTask{Frame: task, Period: 20 * time.Millisecond},
				Applied: &record.CyclicTask{Frame: task, Period: 20 * time.Millisecond}}))
		case 180:
			must(t, w.Cyclic(at(k), &record.Cyclic{Seq: 5, Event: record.CyclicStop,
				Set: record.CyclicTask{Frame: task, Period: 20 * time.Millisecond}}))
		}
	}
	must(t, w.Close())
	return buf.Bytes()
}

// writeSpecFixture is what ktestd does not write and the format allows: floats
// of both widths and orders, unaligned fields, every conversion, complex values,
// guards, fixed and variable strings, the transpose filter, frequency and log
// axes, and held values restated by a HOLD frame.
func writeSpecFixture(t *testing.T) []byte {
	var buf bytes.Buffer
	w, err := logb.NewWriter(&buf)
	must(t, err)
	w.Codec = logb.CodecNone

	spec := &logb.Schema{
		UUID: uid("spec"), Name: "spec", RecordBits: 512, AxisKind: logb.AxisTime, AxisMode: logb.AxisImplicit,
		AxisExp: -6, AxisUnit: "s", AxisStep: logb.TickVal(250),
		Fields: []logb.Field{
			{Name: "f32", BitOffset: 0, BitWidth: 32, Type: logb.TypeFloat},
			{Name: "f64be", BitOffset: 36, BitWidth: 64, Type: logb.TypeFloat, BigEndian: true},
			{Name: "u12be", BitOffset: 100, BitWidth: 12, Type: logb.TypeUint, BigEndian: true,
				Conv: logb.Rational{P: [6]float64{0, 2, 1, 0, 0, 4}}},
			{Name: "s7", BitOffset: 112, BitWidth: 7, Type: logb.TypeSint,
				Conv: logb.Table{Keys: []float64{-64, -10, 0, 10}, Vals: []float64{-1, -0.5, 0, 0.5}}},
			{Name: "flag", BitOffset: 119, BitWidth: 1, Type: logb.TypeBool},
			{Name: "u8", BitOffset: 120, BitWidth: 8, Type: logb.TypeUint,
				Conv: logb.Table{Keys: []float64{0, 100, 200}, Vals: []float64{0, 10, 40}, Interp: true}},
			{Name: "zone", BitOffset: 128, BitWidth: 8, Type: logb.TypeUint,
				Conv: logb.RangeToText{Los: []float64{0, 50}, His: []float64{49, 99}, Texts: []string{"low", "high"}, Default: "off"}},
			{Name: "u40", BitOffset: 136, BitWidth: 40, Type: logb.TypeUint},
			{Name: "s64", BitOffset: 176, BitWidth: 64, Type: logb.TypeSint},
			{Name: "name", BitOffset: 240, BitWidth: 128, Type: logb.TypeString},
			{Name: "sel", BitOffset: 368, BitWidth: 4, Type: logb.TypeUint},
			{Name: "gA", BitOffset: 372, BitWidth: 12, Type: logb.TypeUint, Guarded: true, GuardField: 10, GuardValue: 1,
				Conv: logb.Linear{A: -5, B: 0.5}},
			{Name: "gB", BitOffset: 372, BitWidth: 12, Type: logb.TypeSint, Guarded: true, GuardField: 10, GuardValue: 2},
			{Name: "cplx", BitOffset: 384, BitWidth: 128, Type: logb.TypeComplex, Conv: logb.Linear{A: 1, B: 2}},
			{Name: "blob", Type: logb.TypeBytes, Variable: true},
			{Name: "note", Type: logb.TypeString, Variable: true},
		},
	}
	sweep := &logb.Schema{
		UUID: uid("sweep"), Name: "sweep", RecordBits: 64, AxisKind: logb.AxisFrequency, AxisMode: logb.AxisLog,
		AxisUnit: "Hz", AxisStep: logb.FloatVal(math.Pow(10, 0.1)),
		Fields: []logb.Field{{Name: "mag", BitOffset: 0, BitWidth: 64, Type: logb.TypeFloat}},
	}
	freq := &logb.Schema{
		UUID: uid("freq"), Name: "freq", RecordBits: 64, AxisKind: logb.AxisFrequency, AxisMode: logb.AxisExplicit,
		AxisUnit: "Hz", AxisScale: logb.FloatVal(0.5), AxisField: 0,
		Fields: []logb.Field{
			{Name: "hz", BitOffset: 0, BitWidth: 32, Type: logb.TypeUint},
			{Name: "p", BitOffset: 32, BitWidth: 32, Type: logb.TypeFloat, BigEndian: true},
		},
	}
	psu := &logb.Schema{
		UUID: uid("psu"), Name: "psu1.ch1", RecordBits: 72, AxisKind: logb.AxisTime, AxisMode: logb.AxisImplicit,
		AxisExp: -3, AxisUnit: "s", AxisStep: logb.TickVal(100),
		Fields: []logb.Field{
			{Name: "v.set", BitOffset: 0, BitWidth: 64, Type: logb.TypeFloat, Hold: true},
			{Name: "on", BitOffset: 64, BitWidth: 1, Type: logb.TypeBool, Hold: true},
		},
	}
	for _, s := range []*logb.Schema{spec, sweep, freq, psu} {
		must(t, w.AddStream(s))
	}
	must(t, w.BeginSegment(0))

	rng := rand.New(rand.NewSource(3))
	specRecs := func(n int) []byte {
		fixed := make([]byte, 0, n*64)
		var tails []byte
		for i := 0; i < n; i++ {
			r := make([]byte, 64)
			put(r, 0, 32, false, uint64(math.Float32bits(float32(rng.NormFloat64()*1e3))))
			f := rng.NormFloat64() * 1e6
			switch i % 9 {
			case 4:
				f = math.NaN()
			case 5:
				f = math.Inf(-1)
			}
			put(r, 36, 64, true, math.Float64bits(f))
			put(r, 100, 12, true, uint64(rng.Intn(4096)))
			put(r, 112, 7, false, uint64(rng.Intn(128)))
			put(r, 119, 1, false, uint64(i%2))
			put(r, 120, 8, false, uint64(rng.Intn(256)))
			put(r, 128, 8, false, uint64(rng.Intn(120)))
			put(r, 136, 40, false, rng.Uint64()&(1<<40-1))
			s64 := []uint64{1 << 63, 1<<63 - 1, 1<<53 + 1, uint64(rng.Int63())}[i%4]
			if i%4 == 3 && i%8 == 3 {
				s64 = uint64(-rng.Int63n(1 << 40))
			}
			put(r, 176, 64, false, s64)
			name := []string{"e2e", "sixteen-bytes-ok", "", "a\u00e9b"}[i%4]
			copy(r[30:46], name)
			put(r, 368, 4, false, uint64(1+i%3))
			put(r, 372, 12, false, uint64(rng.Intn(4096)))
			put(r, 384, 64, false, math.Float64bits(rng.NormFloat64()))
			put(r, 448, 64, false, math.Float64bits(rng.NormFloat64()))
			fixed = append(fixed, r...)

			blob := make([]byte, rng.Intn(11))
			rng.Read(blob)
			note := strings.Repeat("n", rng.Intn(6))
			tails = binary.LittleEndian.AppendUint32(tails, uint32(len(blob)))
			tails = append(tails, blob...)
			tails = binary.LittleEndian.AppendUint32(tails, uint32(len(note)))
			tails = append(tails, note...)
		}
		return append(fixed, tails...)
	}
	must(t, w.WriteData(spec, logb.TickVal(0), 0, 15, specRecs(15)))
	w.Filter = logb.FilterTranspose
	must(t, w.WriteData(spec, logb.TickVal(15*250), 0, 15, specRecs(15)))
	w.Filter = logb.FilterNone
	must(t, w.WriteData(spec, logb.TickVal(30*250), 0, 10, specRecs(10)))

	f64s := func(vs ...float64) []byte {
		b := make([]byte, 8*len(vs))
		for i, v := range vs {
			binary.LittleEndian.PutUint64(b[8*i:], math.Float64bits(v))
		}
		return b
	}
	must(t, w.WriteData(sweep, logb.FloatVal(10), 0, 5, f64s(1, 0.5, 0.25, 0.125, 0.0625)))
	fr := make([]byte, 0, 32)
	for i, hz := range []uint32{100, 250, 1000, 40000} {
		b := make([]byte, 8)
		binary.LittleEndian.PutUint32(b, hz)
		put(b, 32, 32, true, uint64(math.Float32bits(float32(i)*1.5)))
		fr = append(fr, b...)
	}
	must(t, w.WriteData(freq, logb.FloatVal(-3.25), 0, 4, fr))

	ps := make([]byte, 9)
	binary.LittleEndian.PutUint64(ps, math.Float64bits(12.5))
	ps[8] = 1
	must(t, w.WriteData(psu, logb.TickVal(0), 0, 1, ps))
	must(t, w.SetHold(psu, logb.TickVal(0), 0, []bool{true, true}, ps))
	must(t, w.BeginSegment(int64(time.Second)))
	must(t, w.WriteData(psu, logb.TickVal(1000), 0, 1, ps))
	must(t, w.Close())
	return buf.Bytes()
}

// ---- the Go side of the comparison ----

func goDump(t *testing.T, path string) []string {
	f, err := os.Open(path)
	must(t, err)
	defer f.Close()
	r, err := logb.NewReader(bufio.NewReader(f))
	must(t, err)
	var lines []string
	r.OnHold = func(h *logb.Hold) {
		v := map[string]any{}
		for i, fd := range h.Schema.Fields {
			if !h.Has(i) {
				continue
			}
			val, err := h.Value(i)
			if errors.Is(err, logb.ErrFieldAbsent) {
				continue
			}
			must(t, err)
			v[fd.Name] = encValue(val)
		}
		lines = append(lines, jsonLine(t, map[string]any{"k": "hold", "s": h.Schema.Name,
			"axis": encAxis(h.Schema, h.AxisBase), "v": v}))
	}
	for {
		b, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		must(t, err)
		if b.Run != nil {
			lines = append(lines, jsonLine(t, map[string]any{"k": "run", "s": b.Schema.Name,
				"id": b.Run.ID, "index": b.Run.Index, "params": b.Run.Params}))
		}
		for i := 0; i < int(b.Count); i++ {
			v := map[string]any{}
			for f, fd := range b.Schema.Fields {
				val, err := b.Value(i, f)
				if errors.Is(err, logb.ErrFieldAbsent) {
					continue
				}
				must(t, err)
				v[fd.Name] = encValue(val)
			}
			ax, err := b.Axis(i)
			must(t, err)
			lines = append(lines, jsonLine(t, map[string]any{"k": "rec", "s": b.Schema.Name, "i": i,
				"axis": encAxis(b.Schema, ax), "v": v}))
		}
	}
	for _, m := range r.Meta {
		lines = append(lines, jsonLine(t, map[string]any{"k": "meta", "key": m.Key, "value": m.Value}))
	}
	names := make([]string, 0, len(r.Attachments))
	for n := range r.Attachments {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		h := fnv.New32a()
		h.Write(r.Attachments[n])
		lines = append(lines, jsonLine(t, map[string]any{"k": "attach", "name": n, "len": len(r.Attachments[n]),
			"fnv": hex.EncodeToString(h.Sum(nil))}))
	}
	lines = append(lines, jsonLine(t, map[string]any{"k": "refused", "n": len(r.Unsupported)}))
	lines = append(lines, jsonLine(t, map[string]any{"k": "truncated", "v": r.Truncated}))
	return lines
}

func encValue(v any) any {
	switch x := v.(type) {
	case uint64:
		return map[string]string{"i": strconv.FormatUint(x, 10)}
	case int64:
		return map[string]string{"i": strconv.FormatInt(x, 10)}
	case float64:
		return encFloat(x)
	case bool:
		return x
	case string:
		return map[string]string{"s": x}
	case []byte:
		return map[string]string{"x": hex.EncodeToString(x)}
	case complex128:
		return map[string]any{"c": []any{encFloat(real(x)), encFloat(imag(x))}}
	}
	return map[string]string{"unknown": strconv.Quote(strings.TrimSpace(strings.ReplaceAll(
		strings.ReplaceAll(string(must1(json.Marshal(v))), "\n", " "), "\t", " ")))}
}

func encFloat(x float64) any {
	switch {
	case math.IsNaN(x):
		return map[string]string{"f": "NaN"}
	case math.IsInf(x, 1):
		return map[string]string{"f": "+Inf"}
	case math.IsInf(x, -1):
		return map[string]string{"f": "-Inf"}
	}
	return x
}

func encAxis(s *logb.Schema, a logb.AxisVal) any {
	if s.AxisKind == logb.AxisTime {
		return map[string]string{"i": strconv.FormatInt(a.Ticks(), 10)}
	}
	return encFloat(a.Float())
}

// ---- running and comparing ----

func needNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed, so the rack's reader is not checked")
	}
	return node
}

func nodeDump(t *testing.T, node, path, chunk string) []string {
	t.Helper()
	cmd := exec.Command(node, "--experimental-strip-types", "--no-warnings", "test/dump.ts", path, chunk)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node test/dump.ts %s %s: %v\n%s", filepath.Base(path), chunk, err, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("node reported: %s", stderr.String())
	}
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n")
}

// compareDumps compares line by line as JSON values, not as text: key order
// and number spelling differ between Go and JavaScript and mean nothing.
func compareDumps(t *testing.T, what string, got, want []string) {
	t.Helper()
	bad := 0
	for i := 0; i < max(len(got), len(want)); i++ {
		var g, w any
		gs, ws := "(nothing)", "(nothing)"
		if i < len(got) {
			gs = got[i]
			json.Unmarshal([]byte(gs), &g)
		}
		if i < len(want) {
			ws = want[i]
			json.Unmarshal([]byte(ws), &w)
		}
		if !reflect.DeepEqual(g, w) {
			bad++
			if bad <= 5 {
				t.Errorf("%s, line %d:\n  browser: %s\n  go:      %s", what, i+1, gs, ws)
			}
		}
	}
	if bad > 5 {
		t.Errorf("%s: %d lines differ in all", what, bad)
	}
}

// ---- helpers ----

// put writes v into a record's bits by logb's rule (SPEC §6.2), for records
// built by hand here.
func put(rec []byte, off, width uint32, bigEndian bool, v uint64) {
	for i := uint32(0); i < width; i++ {
		p := off + i
		var b byte
		if bigEndian {
			b = byte(v>>(width-1-i)) & 1
			rec[p/8] = rec[p/8]&^(1<<(7-p%8)) | b<<(7-p%8)
		} else {
			b = byte(v>>i) & 1
			rec[p/8] = rec[p/8]&^(1<<(p%8)) | b<<(p%8)
		}
	}
}

func uid(s string) [16]byte { return uuid.NewSHA1(uuid.NameSpaceOID, []byte("ktest/web/"+s)) }

func jsonLine(t *testing.T, v any) string {
	b, err := json.Marshal(v)
	must(t, err)
	return string(b)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func must1[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// writeScopeFixture is a scope's share of a recording: an acquisition's samples
// on a uniform axis with no axis bytes, the min/max envelope that reduces it,
// and the RUN frame that says what both were taken under (§5.4).
//
// It is here because the browser reads waveforms now, and the three things that
// makes it do are all things a reader can get plausibly wrong: an implicit
// uniform axis, where a wrong step misplaces every sample and nothing says so;
// guarded fields, where a bucket that held no sample must come back absent and
// never as a zero volt; and the RUN frame, which is where an acquisition's
// settings live and which a reader that only decoded records would drop without
// noticing. The timebase changes half way, which under §5.2 opens a segment, so
// the run and the schema are both restated and the reader has to follow.
func writeScopeFixture(t *testing.T) []byte {
	var buf bytes.Buffer
	w, err := logb.NewWriter(&buf)
	must(t, err)
	w.Codec = logb.CodecNone

	samples := &logb.Schema{
		UUID: uid("scope1.ch1"), Name: "scope1.ch1", RecordBits: 32,
		AxisKind: logb.AxisTime, AxisMode: logb.AxisImplicit, AxisExp: -9, AxisUnit: "s",
		AxisStep: logb.TickVal(1000), // 1 µs a sample
		Fields: []logb.Field{
			{Name: "v", BitOffset: 0, BitWidth: 32, Type: logb.TypeFloat, Unit: "V"},
		},
	}
	env := &logb.Schema{
		UUID: uid("scope1.ch1.env"), Name: "scope1.ch1.env", RecordBits: 128,
		AxisKind: logb.AxisTime, AxisMode: logb.AxisImplicit, AxisExp: -9, AxisUnit: "s",
		AxisStep: logb.TickVal(2000), // 70 samples reduced to 35 buckets
		Fields: []logb.Field{
			{Name: "present", BitOffset: 0, BitWidth: 1, Type: logb.TypeBool},
			{Name: "n", BitOffset: 32, BitWidth: 32, Type: logb.TypeUint},
			{Name: "min", BitOffset: 64, BitWidth: 32, Type: logb.TypeFloat, Unit: "V",
				Guarded: true, GuardField: 0, GuardValue: 1},
			{Name: "max", BitOffset: 96, BitWidth: 32, Type: logb.TypeFloat, Unit: "V",
				Guarded: true, GuardField: 0, GuardValue: 1},
		},
		Meta: map[string]string{"kind": "envelope", "of": "scope1.ch1"},
	}
	for _, s := range []*logb.Schema{samples, env} {
		must(t, w.AddStream(s))
	}
	must(t, w.BeginSegment(0))

	volts := func(n int, phase float64) []byte {
		b := make([]byte, 4*n)
		for i := 0; i < n; i++ {
			v := float32(math.Sin(phase + float64(i)*0.01))
			binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(v))
		}
		return b
	}
	// A bucket that held no sample is absent, never zero: the guard is the
	// whole point of the field, and a reader that handed back a zero volt there
	// would be inventing a measurement.
	buckets := func(n int, empty func(int) bool) []byte {
		b := make([]byte, 16*n)
		for i := 0; i < n; i++ {
			rec := b[16*i:]
			if empty(i) {
				continue
			}
			rec[0] = 1
			binary.LittleEndian.PutUint32(rec[4:], uint32(3+i%5))
			binary.LittleEndian.PutUint32(rec[8:], math.Float32bits(float32(-1-0.001*float64(i))))
			binary.LittleEndian.PutUint32(rec[12:], math.Float32bits(float32(1+0.001*float64(i))))
		}
		return b
	}

	// The first acquisition: 5 µs a division at 1 MSa/s, so 70 µs of screen and
	// 70 samples, the trigger 35 µs in. The geometry is the dialect's own rule —
	// sample 0 at -(tdiv x 14 / 2), one sample every 1/sara — because the rack
	// worker computes where to draw the trigger from exactly these numbers, and
	// a fixture whose axis did not add up would check that arithmetic against
	// nothing.
	must(t, w.AddRun(&logb.Run{ID: 1, Index: 1, Params: map[string]string{
		"instrument": "scope1", "channel": "1", "vdiv": "0.5", "ofst": "0.25",
		"tdiv": "5e-06", "sara": "1e+06", "sign": "sub256", "first": "-35µs",
		"conversion": "volts = code x (vdiv / 25) - ofst",
	}}))
	must(t, w.WriteData(samples, logb.TickVal(-35_000), 1, 70, volts(70, 0)))
	// The buckets are centres, so the first is half a bucket in.
	must(t, w.WriteData(env, logb.TickVal(-34_000), 1, 35, buckets(35, func(i int) bool { return i == 7 || i == 13 })))
	w.EndRun(1)

	// The timebase changes, which under §5.2 must open a segment before the
	// step does: the schema carries axis_step and is only restated at a
	// boundary, so a change inside one would misplace every sample after it.
	samples.AxisStep = logb.TickVal(2000)
	env.AxisStep = logb.TickVal(4000)
	must(t, w.BeginSegment(1))
	// 10 µs a division at 500 kSa/s: 140 µs of screen, the trigger 70 µs in.
	// The acquisition is placed at 1 s on the recording's timeline, so sample 0
	// is at 1 s - 70 µs.
	must(t, w.AddRun(&logb.Run{ID: 2, Index: 2, Params: map[string]string{
		"instrument": "scope1", "channel": "1", "vdiv": "0.5", "ofst": "0.25",
		"tdiv": "1e-05", "sara": "5e+05", "sign": "sub256", "first": "-70µs",
		"frame_time": "00: 05: 12. 650814",
	}}))
	must(t, w.WriteData(samples, logb.TickVal(999_930_000), 2, 70, volts(70, 1.5)))
	must(t, w.WriteData(env, logb.TickVal(999_932_000), 2, 35, buckets(35, func(int) bool { return false })))
	w.EndRun(2)

	// A run that ended is not restated: the segment after it must carry the
	// schemas and no RUN frame, which is what keeps a scope's preamble the size
	// of what the segment holds rather than of the whole run.
	must(t, w.BeginSegment(2))
	must(t, w.WriteData(samples, logb.TickVal(2_000_000_000), 2, 8, volts(8, 3)))

	must(t, w.Close())
	return buf.Bytes()
}
