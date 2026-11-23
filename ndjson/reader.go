package ndjson

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/trukeio/logb"
)

// Kind is what a message is (§7.5).
type Kind int

const (
	KindMeta Kind = iota + 1
	KindSchema
	KindHold
	KindRecord
)

// Message is one line, decoded.
type Message struct {
	Kind Kind

	// Meta is a meta message's keys, sorted by key: JSON keeps no order.
	Meta []logb.Meta

	// Schema is the stream a schema, hold or record message is about, as a
	// Logb schema: the time axis first, field t, in nanoseconds, then the
	// carried fields in the order the schema message gave them, each at a
	// byte boundary, little-endian. It is not the layout the secondary
	// recorded in, which NDJSON does not carry, so read it by field name.
	Schema *logb.Schema

	// T is a hold or record's time, nanoseconds since recording.epoch.
	T int64

	// Rec is a hold or record's values, laid out by Schema, t included.
	Rec []byte

	// Present says, by index into Schema.Fields, which fields the message
	// gave a value. A field it left out reads as zero in Rec; for a guarded
	// field the guard says so as well.
	Present []bool
}

// Reader reads messages from an NDJSON stream.
type Reader struct {
	sc      *bufio.Scanner
	schemas map[string]*layout

	// Meta collects every meta message's keys as they arrive, sorted within
	// each message; a key repeated later is not added again.
	Meta []logb.Meta
}

// NewReader reads from r.
func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	return &Reader{sc: sc, schemas: map[string]*layout{}}
}

// layout is how a stream's values go into a record.
type layout struct {
	s      *logb.Schema
	byName map[string]*slot
}

type slot struct {
	index  int // into s.Fields
	typ    string
	labels map[string]uint64 // enum: label → code
	max    int               // string, bytes: bytes
}

// Next is the next message. A line that is not a message this reader knows
// (another kind, or blank) is skipped, as §7.5 says a reader must; a record
// or hold of a stream with no schema yet is an error.
func (r *Reader) Next() (Message, error) {
	for r.sc.Scan() {
		line := bytes.TrimSpace(r.sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(line, &top); err != nil {
			return Message{}, fmt.Errorf("ndjson: a line is not a JSON object: %v", err)
		}
		// A schema message has a "meta" key of its own, so meta is
		// recognised last.
		switch {
		case top["rec"] != nil:
			return r.values(KindRecord, top["rec"], top)
		case top["hold"] != nil:
			return r.values(KindHold, top["hold"], top)
		case top["schema"] != nil:
			return r.schema(line)
		case top["meta"] != nil:
			return r.meta(top["meta"])
		}
	}
	if err := r.sc.Err(); err != nil {
		return Message{}, err
	}
	return Message{}, io.EOF
}

func (r *Reader) meta(raw json.RawMessage) (Message, error) {
	var kv map[string]string
	if err := json.Unmarshal(raw, &kv); err != nil {
		return Message{}, fmt.Errorf("ndjson: meta: every value is a string (§7.3): %v", err)
	}
	m := Message{Kind: KindMeta}
	for k, v := range kv {
		m.Meta = append(m.Meta, logb.Meta{Key: k, Value: v})
	}
	slices.SortFunc(m.Meta, func(a, b logb.Meta) int { return cmpStr(a.Key, b.Key) })
	for _, kv := range m.Meta {
		if !slices.ContainsFunc(r.Meta, func(x logb.Meta) bool { return x.Key == kv.Key }) {
			r.Meta = append(r.Meta, kv)
		}
	}
	return m, nil
}

func cmpStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// axisExp is nanoseconds: §7.2.
const axisExp = -9

func (r *Reader) schema(line []byte) (Message, error) {
	var js jsonSchema
	if err := json.Unmarshal(line, &js); err != nil {
		return Message{}, fmt.Errorf("ndjson: schema: %v", err)
	}
	if js.Schema == "" {
		return Message{}, errors.New("ndjson: a schema without a stream name")
	}
	l := &layout{byName: map[string]*slot{}}
	fields := []logb.Field{{Name: "t", BitWidth: 64, Type: logb.TypeSint, Unit: "ns",
		Desc: "axis: nanoseconds since recording.epoch"}}
	at := uint32(64)
	guards := map[int]*jsonGuard{}
	for _, jf := range js.Fields {
		if jf.Name == "" || l.byName[jf.Name] != nil {
			continue
		}
		f := logb.Field{Name: jf.Name, Unit: jf.Unit, Desc: jf.Desc, Hold: jf.Held, Meta: jf.Meta, BitOffset: at}
		sl := &slot{typ: jf.Type}
		switch jf.Type {
		case typeUint, typeSint, typeEnum:
			if jf.Bits == 0 || jf.Bits > 64 {
				return Message{}, fmt.Errorf("ndjson: schema %s: field %s: %s of %d bits", js.Schema, jf.Name, jf.Type, jf.Bits)
			}
			f.Type, f.BitWidth = logb.TypeUint, jf.Bits
			if jf.Type == typeSint {
				f.Type = logb.TypeSint
			}
			if jf.Type == typeEnum {
				conv := logb.ValueToText{}
				sl.labels = map[string]uint64{}
				codes := make([]uint64, 0, len(jf.Labels))
				for k := range jf.Labels {
					c, err := strconv.ParseUint(k, 10, 64)
					if err != nil {
						return Message{}, fmt.Errorf("ndjson: schema %s: field %s: label key %q is not a code", js.Schema, jf.Name, k)
					}
					codes = append(codes, c)
				}
				slices.Sort(codes)
				for _, c := range codes {
					text := jf.Labels[strconv.FormatUint(c, 10)]
					conv.Keys = append(conv.Keys, float64(c))
					conv.Texts = append(conv.Texts, text)
					sl.labels[text] = c
				}
				f.Conv = conv
			}
		case typeFloat:
			f.Type, f.BitWidth = logb.TypeFloat, 64
		case typeBool:
			f.Type, f.BitWidth = logb.TypeBool, 1
		case typeString, typeBytes:
			if jf.Max == 0 {
				continue // unbounded: a fixed record cannot hold it
			}
			f.Type, f.BitWidth = logb.TypeString, 8*jf.Max
			if jf.Type == typeBytes {
				f.Type = logb.TypeBytes
			}
			sl.max = int(jf.Max)
		default:
			continue // a type this reader does not know
		}
		at += (f.BitWidth + 7) / 8 * 8
		sl.index = len(fields)
		if jf.Guard != nil {
			guards[sl.index] = jf.Guard
		}
		fields = append(fields, f)
		l.byName[jf.Name] = sl
	}
	for i, g := range guards {
		gs, ok := l.byName[g.Field]
		if !ok {
			continue // its guard was not carried: the field is just sometimes absent
		}
		fields[i].Guarded, fields[i].GuardField, fields[i].GuardValue = true, uint16(gs.index), g.Value
	}
	l.s = &logb.Schema{
		UUID:       uuidOf(js.Schema),
		Name:       js.Schema,
		RecordBits: at,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisExplicit,
		AxisExp:    axisExp,
		AxisUnit:   "s",
		AxisScale:  logb.TickVal(1),
		AxisField:  0,
		Fields:     fields,
		Meta:       js.Meta,
	}
	r.schemas[js.Schema] = l
	return Message{Kind: KindSchema, Schema: l.s}, nil
}

// uuidOf names a stream's schema by its name, as an RFC 4122 version 5 UUID,
// so that the same stream read twice is the same logb stream.
func uuidOf(name string) [16]byte {
	h := sha1.Sum([]byte("ktest/ndjson/" + name))
	var u [16]byte
	copy(u[:], h[:16])
	u[6] = u[6]&0x0f | 0x50
	u[8] = u[8]&0x3f | 0x80
	return u
}

func (r *Reader) values(k Kind, nameRaw json.RawMessage, top map[string]json.RawMessage) (Message, error) {
	var name string
	if err := json.Unmarshal(nameRaw, &name); err != nil {
		return Message{}, fmt.Errorf("ndjson: a stream name is a string: %v", err)
	}
	l, ok := r.schemas[name]
	if !ok {
		return Message{}, fmt.Errorf("ndjson: %s before its schema", name)
	}
	t, err := integer(top["t"], true)
	if err != nil {
		return Message{}, fmt.Errorf("ndjson: %s: t: %v", name, err)
	}
	m := Message{Kind: k, Schema: l.s, T: int64(t), Rec: make([]byte, l.s.RecordBytes()),
		Present: make([]bool, len(l.s.Fields))}
	putBits(m.Rec, 0, 64, t)
	m.Present[0] = true
	var v map[string]json.RawMessage
	if raw := top["v"]; raw != nil {
		if err := json.Unmarshal(raw, &v); err != nil {
			return Message{}, fmt.Errorf("ndjson: %s: v: %v", name, err)
		}
	}
	for fname, raw := range v {
		sl, ok := l.byName[fname]
		if !ok {
			continue // a field this reader does not know, or one it cannot hold
		}
		f := &l.s.Fields[sl.index]
		if err := put(m.Rec, f, sl, raw); err != nil {
			return Message{}, fmt.Errorf("ndjson: %s: %s: %v", name, fname, err)
		}
		m.Present[sl.index] = true
	}
	return m, nil
}

// integer reads a JSON number or a string holding a decimal integer, as §7.8
// writes integers narrow and wide. signed says whether a minus is allowed;
// the result is the two's complement bits either way.
func integer(raw json.RawMessage, signed bool) (uint64, error) {
	if raw == nil {
		return 0, errors.New("missing")
	}
	s := string(raw)
	if len(s) >= 2 && s[0] == '"' {
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, err
		}
	}
	if signed {
		v, err := strconv.ParseInt(s, 10, 64)
		return uint64(v), err
	}
	return strconv.ParseUint(s, 10, 64)
}

func put(rec []byte, f *logb.Field, sl *slot, raw json.RawMessage) error {
	switch sl.typ {
	case typeUint, typeSint:
		v, err := integer(raw, sl.typ == typeSint)
		if err != nil {
			return err
		}
		putBits(rec, f.BitOffset, f.BitWidth, v)
	case typeEnum:
		var label string
		if json.Unmarshal(raw, &label) == nil {
			c, ok := sl.labels[label]
			if !ok {
				return fmt.Errorf("%q is not one of its labels", label)
			}
			putBits(rec, f.BitOffset, f.BitWidth, c)
			return nil
		}
		v, err := integer(raw, false)
		if err != nil {
			return err
		}
		putBits(rec, f.BitOffset, f.BitWidth, v)
	case typeFloat:
		x := math.NaN() // null: not finite (§7.8)
		if string(raw) != "null" {
			if err := json.Unmarshal(raw, &x); err != nil {
				return err
			}
		}
		putBits(rec, f.BitOffset, 64, math.Float64bits(x))
	case typeBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return err
		}
		if b {
			putBits(rec, f.BitOffset, 1, 1)
		}
	case typeString:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		copy(rec[f.BitOffset/8:], cut(s, sl.max))
	case typeBytes:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return err
		}
		if len(b) > sl.max {
			b = b[:sl.max]
		}
		copy(rec[f.BitOffset/8:], b)
	}
	return nil
}

// cut is s in at most n bytes, cut at a character boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// putBits writes v's low width bits at bit off, little-endian.
func putBits(rec []byte, off, width uint32, v uint64) {
	for i := uint32(0); i < width; i++ {
		bit := off + i
		if int(bit/8) >= len(rec) {
			return
		}
		if v>>i&1 != 0 {
			rec[bit/8] |= 1 << (bit % 8)
		} else {
			rec[bit/8] &^= 1 << (bit % 8)
		}
	}
}
