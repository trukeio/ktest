// Package monitor is a DUT rack's account of whether the DUT behaves: the
// contract file that declares what the master may see and ask of it
// (dut-interface.json, handoff §4.2), and the evaluator that turns the
// recording's samples into monitor states (§4.3).
//
// A monitor is an expression of the monitor profile over the contract's
// inputs and its other monitors, and a state:
//
//	ok        the expression holds, and has held for its debounce
//	dev       it does not, and has not for its debounce
//	unknown   it has no answer: an input not yet seen, gone stale, or unknown
//	inactive  the mode under way is not one the monitor watches
//	settling  the mode it watches was entered less than the mode's settle ago
//
// The evaluator is pure: it reads no clock, and the same records in the same
// order give the same transitions at the same timestamps, to the
// nanosecond. A transition is stamped with the sample that caused it, or with
// the deadline at which it became true — an age crossing its threshold, a
// debounce running out, an input going stale — never with when it happened
// to be evaluated. The daemon runs it on the records as they are written;
// monitor-check runs it on a recording, offline, through the same code.
package monitor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rveen/ktest/channel"
	"github.com/rveen/ktest/expr"
	"github.com/rveen/ktest/procedure"
)

// SchemaVersion is the contract schema this package reads.
const SchemaVersion = 1

// Channels resolves the channel names a contract's inputs give.
type Channels interface {
	Lookup(name string) (channel.Ref, error)
}

// Contract is a loaded, checked dut-interface.json.
type Contract struct {
	Name string
	Path string
	// Hash is the sha256 of the contract file's bytes, hex: what both racks
	// record, and what the master compares with its own copy.
	Hash string
	// ResourceHash is the sha256 of the actions' resource file, hex, "" for
	// a contract with no actions. The contract names keywords; what they do
	// is in the resource, so a changed resource is a changed contract in all
	// but its hash, and is recorded beside it.
	ResourceHash string

	Inputs   map[string]*Input
	Monitors map[string]*Monitor
	// Order is the monitors in dependency order: each after every monitor it
	// reads.
	Order    []string
	Modes    map[string]*Mode
	Resource string // path, relative to the contract
	Actions  map[string]string
	Enums    map[string]*expr.EnumType
	Timeouts map[string]time.Duration

	// names maps every name an expression reads to what it is.
	names map[string]nameRef
}

// Input is one of the contract's inputs: an alias for a channel.
type Input struct {
	Alias   string
	Channel string
	Ref     channel.Ref
	// MaxAge is how old the input's latest sample may be before the monitors
	// reading it are unknown. 0 is the default, 3 × the input's observed
	// period. A held input is never stale.
	MaxAge time.Duration
	Enum   *expr.EnumType
}

// Monitor is one declared monitor.
type Monitor struct {
	Name     string
	OK       string
	Debounce time.Duration
	Modes    []string // the modes it watches; none for all
	Reads    []string // the monitors its expression reads
	Inputs   []string // the inputs its expression reads, by alias
}

// Mode is one operating mode of the DUT.
type Mode struct {
	Name   string
	Action string // the entry action, "" for none
	Settle time.Duration
}

// Reserved is the monitor whose dev asks the master for protection (§4.2).
const Reserved = "abort"

// nameRef is what an expression's name is: a monitor, or a channel of an
// input.
type nameRef struct {
	monitor string
	alias   string
	ref     channel.Ref
}

type contractFile struct {
	Contract string                     `json:"contract"`
	Schema   int                        `json:"schema"`
	Inputs   map[string]json.RawMessage `json:"inputs"`
	Monitors map[string]monitorFile     `json:"monitors"`
	Modes    map[string]modeFile        `json:"modes"`
	Resource string                     `json:"resource"`
	Actions  map[string]string          `json:"actions"`
	Enums    map[string][]string        `json:"enums"`
	Timeouts map[string]string          `json:"timeouts"`
}

type monitorFile struct {
	OK       string   `json:"ok"`
	Debounce string   `json:"debounce"`
	Modes    []string `json:"modes"`
}

type modeFile struct {
	Action string `json:"action"`
	Settle string `json:"settle"`
}

// inputFile is an input written as an object, for what a channel name alone
// cannot say: "Alive": {"channel": "DutStatus", "max_age": "50ms"}.
type inputFile struct {
	Channel string `json:"channel"`
	MaxAge  string `json:"max_age"`
	Enum    string `json:"enum"`
}

var ident = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Load reads and checks a contract against the channels it will be evaluated
// over. It is strict as panel.json is: an unknown key, an input that names
// no channel, a monitor that reads itself through others, a unit that does
// not fit — each refuses the whole contract, with a sentence.
func Load(path string, ch Channels) (*Contract, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := parse(path, raw, ch)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func parse(path string, raw []byte, ch Channels) (*Contract, error) {
	var f contractFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("there is more after the contract's closing brace")
	}
	sum := sha256.Sum256(raw)
	c := &Contract{Name: f.Contract, Path: path, Hash: hex.EncodeToString(sum[:]),
		Inputs: map[string]*Input{}, Monitors: map[string]*Monitor{}, Modes: map[string]*Mode{},
		Resource: f.Resource, Actions: map[string]string{}, Enums: map[string]*expr.EnumType{},
		Timeouts: map[string]time.Duration{}, names: map[string]nameRef{}}

	switch {
	case f.Contract == "":
		return nil, fmt.Errorf(`"contract" is required: the contract's name, which both racks record`)
	case f.Schema != SchemaVersion:
		return nil, fmt.Errorf(`"schema" is %d, and this reads schema %d`, f.Schema, SchemaVersion)
	case len(f.Monitors) == 0:
		return nil, fmt.Errorf(`a contract with no "monitors" would watch nothing`)
	case len(f.Inputs) == 0:
		return nil, fmt.Errorf(`a contract with no "inputs" has nothing for its monitors to read`)
	}

	for _, name := range slices.Sorted(maps.Keys(f.Enums)) {
		vals := f.Enums[name]
		if !ident.MatchString(name) {
			return nil, fmt.Errorf("enum %q: a name is a letter, then letters, digits or _", name)
		}
		if len(vals) == 0 {
			return nil, fmt.Errorf("enum %s has no values", name)
		}
		for i, v := range vals {
			if slices.Contains(vals[:i], v) {
				return nil, fmt.Errorf("enum %s has %q twice; its order would be ambiguous", name, v)
			}
		}
		c.Enums[name] = &expr.EnumType{Name: name, Values: vals}
	}

	for _, alias := range slices.Sorted(maps.Keys(f.Inputs)) {
		in, err := c.input(alias, f.Inputs[alias], ch)
		if err != nil {
			return nil, err
		}
		c.Inputs[alias] = in
	}

	for _, name := range slices.Sorted(maps.Keys(f.Modes)) {
		m := f.Modes[name]
		if !ident.MatchString(name) {
			return nil, fmt.Errorf("mode %q: a name is a letter, then letters, digits or _", name)
		}
		settle, err := duration(m.Settle, true)
		if err != nil {
			return nil, fmt.Errorf("mode %s: settle: %v", name, err)
		}
		if m.Action != "" {
			if _, ok := f.Actions[m.Action]; !ok {
				return nil, fmt.Errorf("mode %s enters with action %q, which \"actions\" does not declare", name, m.Action)
			}
		}
		c.Modes[name] = &Mode{Name: name, Action: m.Action, Settle: settle}
	}

	for _, name := range slices.Sorted(maps.Keys(f.Monitors)) {
		m := f.Monitors[name]
		switch {
		case !ident.MatchString(name):
			return nil, fmt.Errorf("monitor %q: a name is a letter, then letters, digits or _", name)
		case name == "watermark" || name == "gap":
			return nil, fmt.Errorf("monitor %s: mon.%s is the evaluator's own channel; name the monitor otherwise", name, name)
		case c.Inputs[name] != nil:
			return nil, fmt.Errorf("monitor %s has the name of an input; an expression could not tell them apart", name)
		case m.OK == "":
			return nil, fmt.Errorf(`monitor %s: "ok" is required, the expression that holds while the DUT behaves`, name)
		}
		deb, err := duration(m.Debounce, true)
		if err != nil {
			return nil, fmt.Errorf("monitor %s: debounce: %v", name, err)
		}
		for _, md := range m.Modes {
			if c.Modes[md] == nil {
				return nil, fmt.Errorf("monitor %s watches mode %q, which \"modes\" does not declare", name, md)
			}
		}
		c.Monitors[name] = &Monitor{Name: name, OK: m.OK, Debounce: deb, Modes: m.Modes}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Monitors)) {
		m := c.Monitors[name]
		p, err := c.compile(m)
		if err != nil {
			return nil, fmt.Errorf("monitor %s: %q: %v", name, m.OK, err)
		}
		for _, n := range p.Inputs() {
			r := c.names[n]
			if r.monitor != "" {
				m.Reads = append(m.Reads, r.monitor)
			} else if !slices.Contains(m.Inputs, r.alias) {
				m.Inputs = append(m.Inputs, r.alias)
			}
		}
	}
	order, err := c.order()
	if err != nil {
		return nil, err
	}
	c.Order = order

	if err := c.actions(path, f.Actions); err != nil {
		return nil, err
	}

	for _, name := range slices.Sorted(maps.Keys(f.Timeouts)) {
		if c.Monitors[name] == nil && c.Modes[name] == nil && c.Actions[name] == "" {
			return nil, fmt.Errorf("timeout %s: a timeout is for a monitor, a mode or an action, and %s is none of them", name, name)
		}
		d, err := duration(f.Timeouts[name], false)
		if err != nil {
			return nil, fmt.Errorf("timeout %s: %v", name, err)
		}
		c.Timeouts[name] = d
	}
	return c, nil
}

func (c *Contract) input(alias string, raw json.RawMessage, ch Channels) (*Input, error) {
	if !ident.MatchString(alias) {
		return nil, fmt.Errorf("input %q: a name is a letter, then letters, digits or _", alias)
	}
	var f inputFile
	if err := json.Unmarshal(raw, &f.Channel); err != nil {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			return nil, fmt.Errorf(`input %s: a channel name, or {"channel": ..., "max_age": ..., "enum": ...}: %v`, alias, err)
		}
	}
	if f.Channel == "" {
		return nil, fmt.Errorf("input %s names no channel", alias)
	}
	ref, err := ch.Lookup(f.Channel)
	if err != nil {
		return nil, fmt.Errorf("input %s: %v", alias, err)
	}
	in := &Input{Alias: alias, Channel: f.Channel, Ref: ref}
	if f.MaxAge != "" {
		if ref.Held {
			return nil, fmt.Errorf("input %s: %s is held, its value standing until the next record, so it never goes stale; "+
				"it takes no max_age", alias, f.Channel)
		}
		if in.MaxAge, err = duration(f.MaxAge, false); err != nil {
			return nil, fmt.Errorf("input %s: max_age: %v", alias, err)
		}
	}
	if f.Enum != "" {
		e := c.Enums[f.Enum]
		switch {
		case e == nil:
			return nil, fmt.Errorf("input %s reads enum %q, which \"enums\" does not declare", alias, f.Enum)
		case ref.Type.Kind != expr.String:
			return nil, fmt.Errorf("input %s: enum %s is read from text, and %s is %s", alias, f.Enum, f.Channel, ref.Type)
		}
		in.Enum = e
	}
	return in, nil
}

// duration reads a duration as expressions write one, "20ms".
func duration(s string, zero bool) (time.Duration, error) {
	if s == "" {
		if zero {
			return 0, nil
		}
		return 0, fmt.Errorf("a duration is required, such as 500ms")
	}
	v, u, err := expr.ParseQuantity(s)
	if err != nil || !u.SameDimension(expr.ParseUnit("s")) {
		return 0, fmt.Errorf("%q is not a duration, such as 500ms", s)
	}
	if v < 0 || v == 0 && !zero {
		return 0, fmt.Errorf("%s: a duration here is longer than zero", s)
	}
	return time.Duration(v*1e9 + 0.5), nil
}

// Resolve types an expression's names: monitors, read as true while ok, and
// the inputs' channels, Alias or Alias.field.
func (c *Contract) Resolve(name string) (expr.Type, error) {
	if r, ok := c.names[name]; ok {
		if r.monitor != "" {
			return expr.Type{Kind: expr.Bool}, nil
		}
		return c.typeOf(r), nil
	}
	if strings.HasPrefix(name, "$") {
		return expr.Type{}, fmt.Errorf("%s: a monitor reads inputs and monitors, not variables", name)
	}
	if c.Monitors[name] != nil {
		c.names[name] = nameRef{monitor: name}
		return expr.Type{Kind: expr.Bool}, nil
	}
	alias, rest, sub := strings.Cut(name, ".")
	in := c.Inputs[alias]
	if in == nil {
		return expr.Type{}, fmt.Errorf("%s is not an input or a monitor of this contract; the inputs are %s and the monitors %s",
			name, strings.Join(slices.Sorted(maps.Keys(c.Inputs)), ", "), strings.Join(slices.Sorted(maps.Keys(c.Monitors)), ", "))
	}
	r := nameRef{alias: alias, ref: in.Ref}
	if sub {
		lk, ok := c.inputLookup(in, rest)
		if !ok {
			return expr.Type{}, fmt.Errorf("%s: %s is %s, which has no field %s", name, alias, in.Channel, rest)
		}
		r.ref = lk
	}
	c.names[name] = r
	return c.typeOf(r), nil
}

func (c *Contract) typeOf(r nameRef) expr.Type {
	if in := c.Inputs[r.alias]; in != nil && in.Enum != nil && r.ref.Name == in.Ref.Name {
		return expr.Type{Kind: expr.Enum, Enum: in.Enum}
	}
	return r.ref.Type
}

// inputLookup finds a field of an input that names a stream, in the stream's
// own schema.
func (c *Contract) inputLookup(in *Input, field string) (channel.Ref, bool) {
	s := in.Ref.Schema
	if in.Ref.Field >= 0 {
		return channel.Ref{}, false
	}
	for k := range s.Fields {
		f := &s.Fields[k]
		if k == int(s.AxisField) || f.Name != field {
			continue
		}
		t, err := channel.FieldType(f)
		if err != nil {
			return channel.Ref{}, false
		}
		return channel.Ref{Name: in.Channel + "." + field, Schema: s, Field: k, Type: t, Held: f.Hold}, true
	}
	return channel.Ref{}, false
}

func (c *Contract) compile(m *Monitor) (*expr.Program, error) {
	return expr.Compile(m.OK, expr.Monitor, c)
}

// order sorts the monitors so that each comes after those it reads, and
// refuses a cycle, naming it.
func (c *Contract) order() ([]string, error) {
	var out []string
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(n string, path []string) error
	visit = func(n string, path []string) error {
		switch state[n] {
		case 2:
			return nil
		case 1:
			i := slices.Index(path, n)
			return fmt.Errorf("monitors read each other in a circle: %s", strings.Join(append(path[i:], n), " → "))
		}
		state[n] = 1
		for _, d := range c.Monitors[n].Reads {
			if err := visit(d, append(path, n)); err != nil {
				return err
			}
		}
		state[n] = 2
		out = append(out, n)
		return nil
	}
	for _, n := range slices.Sorted(maps.Keys(c.Monitors)) {
		if err := visit(n, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// actions checks that every action names a keyword of the resource file.
// What the keywords may call is the DUT rack's library, which the rack
// checks when it loads the resource to run them.
func (c *Contract) actions(path string, acts map[string]string) error {
	if len(acts) == 0 {
		if c.Resource != "" {
			return fmt.Errorf(`"resource" is given, and there are no "actions" to take from it`)
		}
		return nil
	}
	if c.Resource == "" {
		return fmt.Errorf(`"actions" are keywords of a "resource" file, and none is given`)
	}
	rp := filepath.Join(filepath.Dir(path), c.Resource)
	b, err := os.ReadFile(rp)
	if err != nil {
		return fmt.Errorf("resource: %v", err)
	}
	sum := sha256.Sum256(b)
	c.ResourceHash = hex.EncodeToString(sum[:])
	f, err := procedure.Parse(c.Resource, string(b))
	if err != nil {
		return fmt.Errorf("resource: %v", err)
	}
	if len(f.Tests) > 0 {
		return fmt.Errorf("resource %s has test cases; it holds the actions' keywords and nothing else", c.Resource)
	}
	have := map[string]bool{}
	for _, k := range f.Keywords {
		have[strings.ToLower(strings.Join(strings.Fields(strings.ReplaceAll(k.Name, "_", " ")), ""))] = true
	}
	for _, name := range slices.Sorted(maps.Keys(acts)) {
		kw := acts[name]
		if !ident.MatchString(name) {
			return fmt.Errorf("action %q: a name is a letter, then letters, digits or _", name)
		}
		if !have[strings.ToLower(strings.Join(strings.Fields(strings.ReplaceAll(kw, "_", " ")), ""))] {
			return fmt.Errorf("action %s is keyword %q, which %s does not define", name, kw, c.Resource)
		}
		c.Actions[name] = kw
	}
	return nil
}
