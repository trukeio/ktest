// Package channel resolves channel names against the streams of a recording,
// live or on disk, and decodes their values for the expression language.
//
// A channel is a stream's field, named stream.field: psu1.ch1.meas.v is the
// field v of psu1.ch1.meas. Field names have dots in them too (v.set), so a
// name is tried at every dot, and exactly one split may name a stream that
// has that field; a name two streams would answer is refused, never guessed.
// A stream named whole, with no field, is an event: it has samples and no
// value (expr.Event).
//
// The daemon's procedures, a contract's monitors and monitor-check all name
// channels this way, through this package, so that a name means the same to
// each.
package channel

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/expr"
)

// Ref is what a channel name resolves to.
type Ref struct {
	Name   string
	Schema *logb.Schema
	Field  int       // -1 for a stream named whole
	Type   expr.Type // expr.Event for a stream named whole
	// Held is a field whose last value stands until the next record: it does
	// not go stale between records, since there is nothing between them to
	// miss.
	Held bool
}

// Stream is the name of the stream the channel is in.
func (r Ref) Stream() string { return r.Schema.Name }

// Decode reads the channel's value from one of its stream's records. A field
// the record does not carry, a guard that does not hold, is not known.
func (r Ref) Decode(rec []byte) expr.Value {
	if r.Field < 0 {
		return expr.Value{Known: true}
	}
	return Decode(r.Schema, rec, r.Field)
}

// Decode reads field f of a record of s.
func Decode(s *logb.Schema, rec []byte, f int) expr.Value {
	b := &logb.Batch{Schema: s, Count: 1, Data: rec}
	v, err := b.Value(0, f)
	if err != nil {
		return expr.Unknown
	}
	switch x := v.(type) {
	case bool:
		return expr.BoolValue(x)
	case string:
		return expr.StrValue(x)
	case float64:
		return expr.Num(x)
	case uint64:
		return expr.Num(float64(x))
	case int64:
		return expr.Num(float64(x))
	}
	return expr.Unknown
}

// FieldType is the type a field has in an expression.
func FieldType(f *logb.Field) (expr.Type, error) {
	switch f.Conv.(type) {
	case logb.ValueToText, logb.RangeToText:
		return expr.Type{Kind: expr.String}, nil
	}
	switch f.Type {
	case logb.TypeBool:
		return expr.Type{Kind: expr.Bool}, nil
	case logb.TypeString:
		return expr.Type{Kind: expr.String}, nil
	case logb.TypeUint, logb.TypeSint, logb.TypeFloat:
		return expr.NumberIn(f.Unit), nil
	}
	return expr.Type{}, fmt.Errorf("field %s holds bytes; an expression reads numbers, truth values and text", f.Name)
}

// Table is the streams of a recording, and each one's latest record. It is
// fed as the recording is written (Stream, Record) and read from other
// goroutines, so it has a lock of its own.
type Table struct {
	mu      sync.RWMutex
	streams map[string]*entry
}

type entry struct {
	s    *logb.Schema
	rec  []byte
	t    time.Time
	have bool
}

// NewTable makes an empty table.
func NewTable() *Table { return &Table{streams: map[string]*entry{}} }

// Stream declares a stream.
func (c *Table) Stream(s *logb.Schema) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.streams[s.Name]; !ok {
		c.streams[s.Name] = &entry{s: s}
	}
}

// Record keeps a copy of rec as its stream's latest record, taken at t.
func (c *Table) Record(s *logb.Schema, t time.Time, rec []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.streams[s.Name]
	if !ok {
		e = &entry{s: s}
		c.streams[s.Name] = e
	}
	e.rec = append(e.rec[:0], rec...)
	e.t, e.have = t, true
}

// Lookup resolves a channel name.
func (c *Table) Lookup(name string) (Ref, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lookup(name)
}

func (c *Table) lookup(name string) (Ref, error) {
	var found []Ref
	if e, ok := c.streams[name]; ok {
		found = append(found, Ref{Name: name, Schema: e.s, Field: -1, Type: expr.Type{Kind: expr.Event}})
	}
	for i := len(name) - 1; i > 0; i-- {
		if name[i] != '.' {
			continue
		}
		e, ok := c.streams[name[:i]]
		if !ok {
			continue
		}
		for k := range e.s.Fields {
			f := &e.s.Fields[k]
			if k == int(e.s.AxisField) || f.Name != name[i+1:] {
				continue
			}
			t, err := FieldType(f)
			if err != nil {
				return Ref{}, fmt.Errorf("%s: %v", name, err)
			}
			found = append(found, Ref{Name: name, Schema: e.s, Field: k, Type: t, Held: f.Hold})
		}
	}
	switch len(found) {
	case 0:
		return Ref{}, fmt.Errorf("%s is not a channel here: no stream is named so, and none has that field", name)
	case 1:
		return found[0], nil
	}
	var what []string
	for _, r := range found {
		if r.Field < 0 {
			what = append(what, "the stream "+r.Stream())
		} else {
			what = append(what, r.Stream()+" and its field "+r.Schema.Fields[r.Field].Name)
		}
	}
	return Ref{}, fmt.Errorf("%s is ambiguous: it names %s", name, strings.Join(what, ", and "))
}

// Resolve types a channel name: an expr.Resolver.
func (c *Table) Resolve(name string) (expr.Type, error) {
	r, err := c.Lookup(name)
	return r.Type, err
}

// Value is a channel's latest value, in its schema's unit, and when it was
// taken in Unix nanoseconds. A field the latest record does not carry has
// none.
func (c *Table) Value(name string) (expr.Value, int64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, err := c.lookup(name)
	if err != nil {
		return expr.Unknown, 0, false
	}
	e := c.streams[r.Stream()]
	if !e.have {
		return expr.Unknown, 0, false
	}
	v := r.Decode(e.rec)
	if !v.Known {
		return v, 0, false
	}
	return v, e.t.UnixNano(), true
}
