// Package contract reads the DUT contract file, dut-interface.json
// (doc/peer-protocol.md §3), as far as both racks need it: its format, read
// strictly, its hashes, and the names it declares.
//
// It is what a master needs of a contract, which names the DUT's modes,
// actions and monitors in its procedures and compares the contract with the
// DUT rack's, but never evaluates it. A DUT rack evaluates it with package
// monitor, which reads the same File and checks the rest: the inputs against
// its channels, the expressions, the actions against their resource.
package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/trukeio/ktest/expr"
)

// SchemaVersion is the contract schema this package reads: the file's
// "schema".
const SchemaVersion = 1

// File is a contract as written. Unknown keys are refused when it is read.
type File struct {
	Contract string                     `json:"contract"`
	Schema   int                        `json:"schema"`
	Inputs   map[string]json.RawMessage `json:"inputs"` // a channel name, or an InputObject
	Monitors map[string]Monitor         `json:"monitors"`
	Modes    map[string]Mode            `json:"modes"`
	Resource string                     `json:"resource"`
	Actions  map[string]string          `json:"actions"`
	Enums    map[string][]string        `json:"enums"`
	Timeouts map[string]string          `json:"timeouts"`
}

// Monitor is one monitor as written.
type Monitor struct {
	OK       string   `json:"ok"`
	Debounce string   `json:"debounce"`
	Modes    []string `json:"modes"`
}

// Mode is one mode as written.
type Mode struct {
	Action string `json:"action"`
	Settle string `json:"settle"`
}

// InputObject is an input written as an object, for what a channel name
// alone cannot say: "Alive": {"channel": "DutStatus", "max_age": "50ms"}.
type InputObject struct {
	Channel string `json:"channel"`
	MaxAge  string `json:"max_age"`
	Enum    string `json:"enum"`
}

// Parse reads a contract's bytes strictly: an unknown key, or anything after
// the closing brace, refuses it. It checks the schema and that the contract
// is named; what the names mean is the reader's to check.
func Parse(raw []byte) (*File, error) {
	var f File
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("there is more after the contract's closing brace")
	}
	switch {
	case f.Contract == "":
		return nil, fmt.Errorf(`"contract" is required: the contract's name, which both racks record`)
	case f.Schema != SchemaVersion:
		return nil, fmt.Errorf(`"schema" is %d, and this reads schema %d`, f.Schema, SchemaVersion)
	}
	return &f, nil
}

// Hash is the contract hash of a file's bytes: sha256, lowercase hex.
func Hash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Duration reads one of a contract's durations, written as expressions write
// them: 500ms, 2s, 1min. "" is 0 if zero allows it, and an error otherwise;
// so is a duration of 0.
func Duration(s string, zero bool) (time.Duration, error) {
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

// Identity is what a master needs of a contract: its name, its hashes, and
// the names it declares.
type Identity struct {
	Name         string
	Path         string
	Hash         string // sha256 of the contract file, hex
	ResourceHash string // sha256 of its resource, hex; "" with none
	Monitors     []string
	Modes        []string
	Actions      []string
	// Settle is each mode's settle time: how long after it is applied its
	// monitors are settling, and a master waits before judging them.
	Settle map[string]time.Duration
}

// Identify reads a contract's identity, and hashes its resource.
func Identify(path string) (*Identity, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	id := &Identity{Name: f.Contract, Path: path, Hash: Hash(raw),
		Monitors: slices.Sorted(maps.Keys(f.Monitors)), Modes: slices.Sorted(maps.Keys(f.Modes)),
		Actions: slices.Sorted(maps.Keys(f.Actions)), Settle: map[string]time.Duration{}}
	for name, m := range f.Modes {
		d, err := Duration(m.Settle, true)
		if err != nil {
			return nil, fmt.Errorf("%s: mode %s: settle: %v", path, name, err)
		}
		id.Settle[name] = d
	}
	if f.Resource != "" {
		rb, err := os.ReadFile(filepath.Join(filepath.Dir(path), f.Resource))
		if err != nil {
			return nil, fmt.Errorf("%s: resource: %v", path, err)
		}
		id.ResourceHash = Hash(rb)
	}
	return id, nil
}

// Has says whether the contract declares a monitor, mode or action of that
// name, with a sentence when it does not. kind is "monitor", "mode" or
// "action".
func (id *Identity) Has(kind, name string) error {
	var names []string
	switch kind {
	case "monitor":
		names = id.Monitors
	case "mode":
		names = id.Modes
	case "action":
		names = id.Actions
	default:
		return fmt.Errorf("a contract declares monitors, modes and actions, not %ss", kind)
	}
	if !slices.Contains(names, name) {
		return fmt.Errorf("contract %s has no %s %q; its %ss are %s", id.Name, kind, name, kind, joinOr(names))
	}
	return nil
}

func joinOr(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	s := names[0]
	for _, n := range names[1:] {
		s += ", " + n
	}
	return s
}
