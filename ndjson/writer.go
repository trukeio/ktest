package ndjson

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"strconv"
	"unicode/utf8"

	"github.com/trukeio/logb"
)

// Writer writes messages as NDJSON lines, each with one Write to the
// underlying writer, followed by Flush if it is set.
type Writer struct {
	w io.Writer
	// Flush, if set, is called after every line: an HTTP response is flushed
	// per message (§7.1).
	Flush func()

	meta    map[string]string // keys sent, with the value they were sent with
	schemas map[string]*plan  // by stream name: what has been sent of each
	line    bytes.Buffer
}

// NewWriter writes to w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w, meta: map[string]string{}, schemas: map[string]*plan{}}
}

// plan is how one stream's fields are written.
type plan struct {
	s      *logb.Schema
	json   []byte  // the schema message, to tell a restatement from a change
	fields []field // in schema order, axis and uncarried fields left out
}

type field struct {
	index  int    // into the logb schema's Fields
	name   string // its JSON name
	typ    string
	wide   bool              // an integer that travels as a string
	labels map[uint64]string // enum: code → label
	conv   bool              // a numeric conversion: carry the converted float
}

// Meta writes the metadata keys not yet sent. A key sent before is not sent
// again, even with another value: §7.3 makes a key's value the one it first
// had. A message is written only when there is something new.
func (w *Writer) Meta(kvs []logb.Meta) error {
	add := map[string]string{}
	for _, kv := range kvs {
		if _, ok := w.meta[kv.Key]; ok {
			continue
		}
		if _, ok := add[kv.Key]; ok {
			continue
		}
		add[kv.Key] = kv.Value
	}
	if len(add) == 0 {
		return nil
	}
	maps.Copy(w.meta, add)
	b, err := marshal(map[string]map[string]string{"meta": add})
	if err != nil {
		return err
	}
	return w.write(b)
}

// Schema writes a stream's schema, unless the same one was written already;
// Logb restates schemas at every segment, NDJSON need not (§7.5). A stream
// without a time axis is not carried: every record has a time (§7.2).
func (w *Writer) Schema(s *logb.Schema) error {
	if s.AxisKind != logb.AxisTime {
		return nil
	}
	p, err := newPlan(s)
	if err != nil {
		return err
	}
	if old, ok := w.schemas[s.Name]; ok && bytes.Equal(old.json, p.json) {
		old.s = s // the new binding decodes this segment's records
		return nil
	}
	w.schemas[s.Name] = p
	return w.write(p.json)
}

func newPlan(s *logb.Schema) (*plan, error) {
	p := &plan{s: s}
	js := jsonSchema{Schema: s.Name, Fields: []jsonField{}, Meta: s.Meta}
	axis := -1
	if s.AxisMode == logb.AxisExplicit {
		axis = int(s.AxisField)
	}
	carried := map[int]bool{}
	for i := range s.Fields {
		f := &s.Fields[i]
		if i == axis || f.Type == logb.TypeComplex {
			continue
		}
		jf, fp := describe(f)
		fp.index = i
		carried[i] = true
		js.Fields = append(js.Fields, jf)
		p.fields = append(p.fields, fp)
	}
	// A guard names its field by name; one on a field not carried is dropped,
	// and the guarded field is then simply sometimes absent.
	k := 0
	for i := range s.Fields {
		if !carried[i] {
			continue
		}
		f := &s.Fields[i]
		if f.Guarded && int(f.GuardField) < len(s.Fields) && carried[int(f.GuardField)] {
			js.Fields[k].Guard = &jsonGuard{Field: s.Fields[f.GuardField].Name, Value: f.GuardValue}
		}
		k++
	}
	b, err := marshal(js)
	if err != nil {
		return nil, fmt.Errorf("ndjson: schema %s: %v", s.Name, err)
	}
	p.json = b
	return p, nil
}

// describe is a field as §7.6 types it, and how its values are written.
func describe(f *logb.Field) (jsonField, field) {
	jf := jsonField{Name: f.Name, Unit: f.Unit, Held: f.Hold, Desc: f.Desc, Meta: f.Meta}
	fp := field{name: f.Name}
	numericConv := false
	var vt *logb.ValueToText
	switch c := f.Conv.(type) {
	case nil, logb.Identity:
	case logb.ValueToText:
		vt = &c
	case *logb.ValueToText:
		vt = c
	case logb.RangeToText, *logb.RangeToText:
		// Not in the messages: the raw number is carried.
	default:
		numericConv = true
	}
	switch f.Type {
	case logb.TypeUint, logb.TypeSint:
		switch {
		case numericConv:
			jf.Type, fp.typ, fp.conv = typeFloat, typeFloat, true
		case vt != nil:
			jf.Type, fp.typ = typeEnum, typeEnum
			jf.Bits = f.BitWidth
			jf.Labels, fp.labels = map[string]string{}, map[uint64]string{}
			for i, k := range vt.Keys {
				if i >= len(vt.Texts) || k < 0 || k != math.Trunc(k) || k > math.MaxUint64/2 {
					continue
				}
				jf.Labels[strconv.FormatUint(uint64(k), 10)] = vt.Texts[i]
				fp.labels[uint64(k)] = vt.Texts[i]
			}
			fp.wide = f.BitWidth > maxExact
		default:
			jf.Type = typeUint
			if f.Type == logb.TypeSint {
				jf.Type = typeSint
			}
			fp.typ = jf.Type
			jf.Bits = f.BitWidth
			fp.wide = f.BitWidth > maxExact
		}
	case logb.TypeFloat:
		jf.Type, fp.typ, fp.conv = typeFloat, typeFloat, numericConv
	case logb.TypeBool:
		jf.Type, fp.typ = typeBool, typeBool
	case logb.TypeString, logb.TypeBytes:
		jf.Type = typeString
		if f.Type == logb.TypeBytes {
			jf.Type = typeBytes
		}
		fp.typ = jf.Type
		if f.Variable {
			jf.Variable = true
		} else {
			jf.Max = f.BitWidth / 8
		}
	}
	return jf, fp
}

// Hold writes a stream's restated held values.
func (w *Writer) Hold(h *logb.Hold) error {
	p, ok := w.schemas[h.Schema.Name]
	if !ok {
		return nil
	}
	d, err := h.AxisBase.Duration(h.Schema.AxisExp)
	if err != nil {
		return fmt.Errorf("ndjson: hold of %s: %v", h.Schema.Name, err)
	}
	return w.message("hold", h.Schema.Name, int64(d), p, func(f int) (any, bool) {
		if !h.Has(p.fields[f].index) {
			return nil, false
		}
		return value(p, f, h.Raw, h.Value)
	})
}

// Batch writes each record of a batch, one line each.
func (w *Writer) Batch(b *logb.Batch) error {
	p, ok := w.schemas[b.Schema.Name]
	if !ok {
		return nil
	}
	for i := 0; i < int(b.Count); i++ {
		ax, err := b.Axis(i)
		if err != nil {
			return fmt.Errorf("ndjson: %s: %v", b.Schema.Name, err)
		}
		d, err := ax.Duration(b.Schema.AxisExp)
		if err != nil {
			return fmt.Errorf("ndjson: %s: %v", b.Schema.Name, err)
		}
		raw := func(f int) (any, error) { return b.Raw(i, f) }
		conv := func(f int) (any, error) { return b.Value(i, f) }
		if err := w.message("rec", b.Schema.Name, int64(d), p, func(f int) (any, bool) {
			return value(p, f, raw, conv)
		}); err != nil {
			return err
		}
	}
	return nil
}

// value is field f's value as its plan carries it, or false if the record has
// none: an unsatisfied guard, or a value that cannot be read.
func value(p *plan, f int, raw, conv func(int) (any, error)) (any, bool) {
	fp := &p.fields[f]
	get := raw
	if fp.conv {
		get = conv
	}
	v, err := get(fp.index)
	if err != nil {
		return nil, false
	}
	return v, true
}

func (w *Writer) message(kind, stream string, t int64, p *plan, get func(int) (any, bool)) error {
	l := &w.line
	l.Reset()
	l.WriteString(`{"`)
	l.WriteString(kind)
	l.WriteString(`":`)
	appendString(l, stream)
	l.WriteString(`,"t":"`)
	l.WriteString(strconv.FormatInt(t, 10))
	l.WriteString(`","v":{`)
	n := 0
	for i := range p.fields {
		v, ok := get(i)
		if !ok {
			continue
		}
		if n > 0 {
			l.WriteByte(',')
		}
		n++
		appendString(l, p.fields[i].name)
		l.WriteByte(':')
		appendValue(l, &p.fields[i], v)
	}
	l.WriteString("}}\n")
	return w.write(l.Bytes())
}

func appendValue(l *bytes.Buffer, fp *field, v any) {
	switch x := v.(type) {
	case uint64:
		if fp.labels != nil {
			if s, ok := fp.labels[x]; ok {
				appendString(l, s)
				return
			}
		}
		appendInt(l, strconv.FormatUint(x, 10), fp.wide)
	case int64:
		if fp.labels != nil && x >= 0 {
			if s, ok := fp.labels[uint64(x)]; ok {
				appendString(l, s)
				return
			}
		}
		appendInt(l, strconv.FormatInt(x, 10), fp.wide)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			l.WriteString("null")
			return
		}
		l.Write(strconv.AppendFloat(nil, x, 'g', -1, 64))
	case bool:
		l.WriteString(strconv.FormatBool(x))
	case string:
		appendString(l, x)
	case []byte:
		l.WriteByte('"')
		l.WriteString(base64.StdEncoding.EncodeToString(x))
		l.WriteByte('"')
	default:
		// A text conversion that missed (logb's Default), or anything else a
		// conversion returns that is not a number: the messages have no such
		// value.
		l.WriteString("null")
	}
}

func appendInt(l *bytes.Buffer, s string, wide bool) {
	if wide {
		l.WriteByte('"')
		l.WriteString(s)
		l.WriteByte('"')
		return
	}
	l.WriteString(s)
}

// appendString writes s as a JSON string, without the HTML escaping
// encoding/json does by default: these lines are read by programs and by
// people at a terminal, not pasted into pages.
func appendString(l *bytes.Buffer, s string) {
	if !utf8.ValidString(s) {
		s = string([]rune(s)) // invalid bytes become U+FFFD, as encoding/json would
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	l.Write(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
}

func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil // Encode ends it with the newline a line needs
}

func (w *Writer) write(b []byte) error {
	if _, err := w.w.Write(b); err != nil {
		return err
	}
	if w.Flush != nil {
		w.Flush()
	}
	return nil
}

// Transcode reads a Logb stream from src and writes it to w as NDJSON, message
// by message as the frames arrive, until src ends or a write fails. Metadata
// is written before the first frame that is not metadata, so that a stream's
// opening keys arrive as one message, first (§7.5).
func Transcode(w *Writer, src io.Reader) error {
	r, err := logb.NewReader(src)
	if err != nil {
		return fmt.Errorf("ndjson: the source is not Logb: %v", err)
	}
	var werr error
	keep := func(err error) {
		if werr == nil {
			werr = err
		}
	}
	r.OnFrame = func(f logb.Frame) {
		if f.Type != logb.FrameMeta {
			keep(w.Meta(r.Meta))
		}
	}
	r.OnSchema = func(s *logb.Schema, _ uint16) { keep(w.Schema(s)) }
	r.OnHold = func(h *logb.Hold) { keep(w.Hold(h)) }
	for {
		b, err := r.Next()
		if werr != nil {
			return werr
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if r.Truncated {
					return io.ErrUnexpectedEOF
				}
				return nil
			}
			return err
		}
		if err := w.Batch(b); err != nil {
			return err
		}
	}
}
