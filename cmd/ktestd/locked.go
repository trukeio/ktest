//go:build linux

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/trukeio/ktest/procedure"
	"github.com/trukeio/ktest/procedure/lib"
)

// # Locked mode
//
// -locked names a file of the sha256 sums a validated configuration had
// (handoff §4.9): the daemon's binary, the keyword library's version, the
// limits, the instrument configuration and every procedure file.
//
//	{
//	  "schema": 1,
//	  "on_mismatch": "refuse",
//	  "binary": "9f2c…",
//	  "library": "ktest-lib 0.1.0",
//	  "limits": "51ab…",
//	  "instruments": "e07d…",
//	  "procedures": {"sweep.robot": "c4e1…", "common.resource": "08aa…"}
//	}
//
// Each run is held to it when it starts. A run whose sums all match is
// accredited. One that differs anywhere is refused, or, with "on_mismatch":
// "mark", runs and is recorded as not accredited, with what differed. Either
// way procedure.run says the mode was locked and which file locked it.
//
// The file is checked against the run, not against the daemon's start: a
// daemon whose binary has changed under the file still records, drives its
// instruments and serves the rack, as a daemon in engineering mode would. What
// it may not do is call a run accredited.
//
// -lock-hashes writes the file this configuration would need, so that the
// file is made from the configuration that was validated, not typed.

// errLocked is a run refused because something differs from the locked sums.
var errLocked = errors.New("locked")

// lockFile is -locked's file, as written.
type lockFile struct {
	Schema      int               `json:"schema"`
	OnMismatch  string            `json:"on_mismatch,omitempty"`
	Binary      string            `json:"binary"`
	Library     string            `json:"library"`
	Limits      string            `json:"limits"`
	Instruments string            `json:"instruments"`
	Procedures  map[string]string `json:"procedures"`
}

// builtinLimits stands for the limits when the daemon runs without -limits:
// the defaults are part of the binary, which has its own sum.
const builtinLimits = "built-in"

// lockState is what the daemon is, as the lock file sees it: the sums it took
// at start, which do not change while it runs.
type lockState struct {
	Binary      string
	Library     string
	Limits      string
	Instruments string
}

// locked is -locked, loaded: the file's sums and the daemon's own.
type locked struct {
	path string
	hash string // sha256 of the file itself, recorded with every run
	want lockFile
	have lockState
	mark bool // on a mismatch run and mark, rather than refuse
}

// loadLocked reads a lock file strictly: unknown keys, a sum that is not one,
// a procedure named outside the procedures directory and a file that locks no
// procedure all refuse the start.
func loadLocked(path string, have lockState) (*locked, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var f lockFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, nil, fmt.Errorf("%s: more than one JSON value", path)
	}
	if f.Schema != 1 {
		return nil, nil, fmt.Errorf("%s: schema %d; this daemon reads schema 1", path, f.Schema)
	}
	lk := &locked{path: path, want: f, have: have}
	switch f.OnMismatch {
	case "", "refuse":
	case "mark":
		lk.mark = true
	default:
		return nil, nil, fmt.Errorf(`%s: on_mismatch is "refuse" (the default) or "mark", not %q`, path, f.OnMismatch)
	}
	for _, s := range []struct{ name, v string }{{"binary", f.Binary}, {"instruments", f.Instruments}} {
		if !isSum(s.v) {
			return nil, nil, fmt.Errorf("%s: %s is a sha256, 64 hex digits, not %q", path, s.name, s.v)
		}
	}
	if f.Limits != builtinLimits && !isSum(f.Limits) {
		return nil, nil, fmt.Errorf("%s: limits is the sha256 of limits.json, or %q for a daemon run without -limits; not %q",
			path, builtinLimits, f.Limits)
	}
	if f.Library == "" {
		return nil, nil, fmt.Errorf("%s: library is the keyword library's version, such as %q", path, have.Library)
	}
	if len(f.Procedures) == 0 {
		return nil, nil, fmt.Errorf("%s: no procedures: a lock that holds no procedure file would accredit nothing", path)
	}
	for _, name := range slices.Sorted(maps.Keys(f.Procedures)) {
		if !filepath.IsLocal(name) || filepath.ToSlash(filepath.Clean(name)) != name {
			return nil, nil, fmt.Errorf("%s: procedure %q: a procedure file is named as -lock-hashes names it, "+
				"relative to the procedures directory and inside it", path, name)
		}
		if !isSum(f.Procedures[name]) {
			return nil, nil, fmt.Errorf("%s: procedure %s: %q is not a sha256, 64 hex digits", path, name, f.Procedures[name])
		}
	}
	sum := sha256.Sum256(raw)
	lk.hash = hex.EncodeToString(sum[:])
	return lk, raw, nil
}

func isSum(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// daemonDiffs says what of the daemon itself differs from the file, which
// is the same for every run: said once at start, and again with each run.
func (lk *locked) daemonDiffs() []string {
	var d []string
	if lk.have.Binary != lk.want.Binary {
		d = append(d, fmt.Sprintf("the binary (sha256 %.12s…, locked %.12s…)", lk.have.Binary, lk.want.Binary))
	}
	if lk.have.Library != lk.want.Library {
		d = append(d, fmt.Sprintf("the library (%s, locked %s)", lk.have.Library, lk.want.Library))
	}
	if lk.have.Limits != lk.want.Limits {
		d = append(d, fmt.Sprintf("the limits (%.12s…, locked %.12s…)", lk.have.Limits, lk.want.Limits))
	}
	if lk.have.Instruments != lk.want.Instruments {
		d = append(d, fmt.Sprintf("the instrument configuration (%.12s…, locked %.12s…)", lk.have.Instruments, lk.want.Instruments))
	}
	return d
}

// check holds a loaded procedure to the file, and says what differs: nothing
// for a run that may be accredited. file is the procedure as the run names
// it, relative to the procedures directory; the suite's files are relative
// to the procedure's own directory.
func (lk *locked) check(file string, s *procedure.Suite) []string {
	d := lk.daemonDiffs()
	dir := filepath.Dir(file)
	for _, rel := range s.Files {
		name := filepath.ToSlash(filepath.Clean(filepath.Join(dir, rel)))
		want, listed := lk.want.Procedures[name]
		switch {
		case !listed:
			d = append(d, fmt.Sprintf("%s, which the lock does not list", name))
		case want != s.Sums[rel]:
			d = append(d, fmt.Sprintf("%s (sha256 %.12s…, locked %.12s…)", name, s.Sums[rel], want))
		}
	}
	return d
}

// diffSentence puts what differs into one sentence.
func diffSentence(lockPath string, d []string) string {
	return "differs from " + filepath.Base(lockPath) + ": " + strings.Join(d, "; ")
}

// lockSums makes the file that would lock this configuration: the daemon's
// own sums and every regular file under the procedures directory, which is
// more than any one run uses, so that one file locks them all.
func lockSums(have lockState, procDir string) (*lockFile, error) {
	f := &lockFile{Schema: 1, OnMismatch: "refuse", Binary: have.Binary, Library: have.Library,
		Limits: have.Limits, Instruments: have.Instruments, Procedures: map[string]string{}}
	err := filepath.WalkDir(procDir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if p != procDir && strings.HasPrefix(e.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(procDir, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		f.Procedures[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(f.Procedures) == 0 {
		return nil, fmt.Errorf("%s holds no files to lock", procDir)
	}
	return f, nil
}

// lockStateOf takes the daemon's own sums: its binary as it runs, the
// library it was built with, the limits file's bytes, and the instrument
// configuration.
func lockStateOf(dbcPath string, limRaw []byte, bus string, meta map[string]string,
	psuPoll, scopePoll time.Duration) (lockState, error) {
	// /proc/self/exe is the file this process was started from, even if
	// another has since been installed at its path.
	bin, err := sumFile("/proc/self/exe")
	if err != nil {
		return lockState{}, fmt.Errorf("the daemon's own binary: %v", err)
	}
	limits := builtinLimits
	if limRaw != nil {
		limits = sumBytes(limRaw)
	}
	dbcSum := "none"
	if dbcPath != "" {
		if dbcSum, err = sumFile(dbcPath); err != nil {
			return lockState{}, err
		}
	}
	inst := instrumentConfig(bus, dbcSum, meta, map[string]string{
		"poll.psu": psuPoll.String(), "poll.scope": scopePoll.String()})
	return lockState{Binary: bin, Library: lib.Version, Limits: limits, Instruments: sumBytes([]byte(inst))}, nil
}

// sumFile is the sha256 of a file's bytes, hex.
func sumFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// sumBytes is the sha256 of b, hex.
func sumBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// instrumentConfig is the instrument configuration as one text, for its sum:
// the bus, the database that decodes it, and each instrument's line of the
// recording's provenance, which is everything the daemon was told about how
// to reach an instrument and how to read it. One line each, sorted, so that
// the order of the flags does not change it.
func instrumentConfig(bus, dbcSum string, meta map[string]string, extra map[string]string) string {
	lines := []string{"bus=" + bus, "dbc=" + dbcSum}
	for k, v := range meta {
		if strings.HasPrefix(k, "psu.") || strings.HasPrefix(k, "scope.") {
			lines = append(lines, k+"="+v)
		}
	}
	for k, v := range extra {
		lines = append(lines, k+"="+v)
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n") + "\n"
}
