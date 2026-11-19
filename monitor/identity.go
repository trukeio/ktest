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
	"slices"
)

// Identity is what a rack that does not evaluate a contract needs of it: the
// master, which names the DUT's modes, actions and monitors in procedures and
// compares the contract with the DUT rack's, but never reads the DUT's
// channels. It is read as strictly as Load reads a contract, and only the
// channels are left unresolved.
type Identity struct {
	Name         string
	Path         string
	Hash         string // sha256 of the contract file, hex
	ResourceHash string // sha256 of its resource, hex; "" with none
	Monitors     []string
	Modes        []string
	Actions      []string
}

// Identify reads a contract's identity.
func Identify(path string) (*Identity, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f contractFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	switch {
	case f.Contract == "":
		return nil, fmt.Errorf(`%s: "contract" is required`, path)
	case f.Schema != SchemaVersion:
		return nil, fmt.Errorf(`%s: "schema" is %d, and this reads schema %d`, path, f.Schema, SchemaVersion)
	}
	sum := sha256.Sum256(raw)
	id := &Identity{Name: f.Contract, Path: path, Hash: hex.EncodeToString(sum[:]),
		Monitors: slices.Sorted(maps.Keys(f.Monitors)), Modes: slices.Sorted(maps.Keys(f.Modes)),
		Actions: slices.Sorted(maps.Keys(f.Actions))}
	if f.Resource != "" {
		rb, err := os.ReadFile(filepath.Join(filepath.Dir(path), f.Resource))
		if err != nil {
			return nil, fmt.Errorf("%s: resource: %v", path, err)
		}
		rs := sha256.Sum256(rb)
		id.ResourceHash = hex.EncodeToString(rs[:])
	}
	return id, nil
}
