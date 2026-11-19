//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rveen/logb"

	"github.com/rveen/ktest/channel"
	"github.com/rveen/ktest/procedure/lib"
	"github.com/rveen/ktest/record"
)

// The daemon as a validated configuration had it, in these tests.
var testHave = lockState{
	Binary:      strings.Repeat("b1", 32),
	Library:     lib.Version,
	Limits:      strings.Repeat("11", 32),
	Instruments: strings.Repeat("e1", 32),
}

const lockProc = `*** Settings ***
Resource    common.resource

*** Test Cases ***
Hello
    Say
`

const lockResource = `*** Keywords ***
Say
    Log    hello
`

// lockDir writes a procedures directory and the lock file -lock-hashes would
// make of it, with on_mismatch set as given ("" leaves the default).
func lockDir(t *testing.T, onMismatch string) (dir, lockPath string) {
	t.Helper()
	root := t.TempDir()
	dir = filepath.Join(root, "procedures")
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "hello.robot"), lockProc)
	write(t, filepath.Join(dir, "common.resource"), lockResource)
	write(t, filepath.Join(dir, "lib", "other.robot"), lockProc) // locked, never run
	f, err := lockSums(testHave, dir)
	if err != nil {
		t.Fatal(err)
	}
	f.OnMismatch = onMismatch
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	lockPath = filepath.Join(root, "locked.json")
	write(t, lockPath, string(b))
	return dir, lockPath
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLockSums: -lock-hashes names every file by its path under the
// procedures directory, with its sum, and what it writes loads.
func TestLockSums(t *testing.T) {
	dir, lp := lockDir(t, "")
	f, err := lockSums(testHave, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sortedKeys(f.Procedures), ","); got != "common.resource,hello.robot,lib/other.robot" {
		t.Errorf("procedures %s", got)
	}
	if f.Procedures["hello.robot"] != sumBytes([]byte(lockProc)) {
		t.Errorf("hello.robot: %s", f.Procedures["hello.robot"])
	}
	lk, raw, err := loadLocked(lp, testHave)
	if err != nil {
		t.Fatal(err)
	}
	if lk.mark || lk.hash != sumBytes(raw) || len(lk.daemonDiffs()) != 0 {
		t.Errorf("mark %v, hash %s, diffs %v", lk.mark, lk.hash, lk.daemonDiffs())
	}
	if _, err := lockSums(testHave, t.TempDir()); err == nil || !strings.Contains(err.Error(), "holds no files") {
		t.Errorf("an empty directory: %v", err)
	}
}

// TestLoadLockedRefusals: a lock file is read as strictly as limits.json, and
// each refusal says what is wrong.
func TestLoadLockedRefusals(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	good := map[string]any{"schema": 1, "binary": sum, "library": lib.Version, "limits": "built-in",
		"instruments": sum, "procedures": map[string]string{"a.robot": sum}}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for kk, vv := range good {
			m[kk] = vv
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	for name, c := range map[string]struct {
		file any
		want string
	}{
		"unknown key":    {with("binaries", sum), `unknown field "binaries"`},
		"schema":         {with("schema", 2), "schema 2; this daemon reads schema 1"},
		"on_mismatch":    {with("on_mismatch", "warn"), `on_mismatch is "refuse" (the default) or "mark", not "warn"`},
		"binary":         {with("binary", "abc"), `binary is a sha256, 64 hex digits, not "abc"`},
		"upper hex":      {with("instruments", strings.ToUpper(sum)), "instruments is a sha256"},
		"limits":         {with("limits", "none"), `limits is the sha256 of limits.json, or "built-in"`},
		"library":        {with("library", nil), "library is the keyword library's version"},
		"no procedures":  {with("procedures", map[string]string{}), "a lock that holds no procedure file would accredit nothing"},
		"outside":        {with("procedures", map[string]string{"../a.robot": sum}), `procedure "../a.robot": a procedure file is named`},
		"unclean":        {with("procedures", map[string]string{"x/../a.robot": sum}), `procedure "x/../a.robot"`},
		"procedure sum":  {with("procedures", map[string]string{"a.robot": "x"}), `procedure a.robot: "x" is not a sha256`},
		"two values":     {`{"schema":1} {}`, "more than one JSON value"},
		"not an object":  {`[]`, "cannot unmarshal array"},
		"a good one too": {good, ""},
	} {
		var b []byte
		if s, ok := c.file.(string); ok {
			b = []byte(s)
		} else {
			b, _ = json.Marshal(c.file)
		}
		p := filepath.Join(t.TempDir(), "locked.json")
		write(t, p, string(b))
		_, _, err := loadLocked(p, testHave)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

// TestLockedDaemonDiffs: each of the daemon's own sums is held to the file.
func TestLockedDaemonDiffs(t *testing.T) {
	_, lp := lockDir(t, "")
	for what, have := range map[string]lockState{
		"the binary":                   {testHave.Binary[:62] + "00", testHave.Library, testHave.Limits, testHave.Instruments},
		"the library":                  {testHave.Binary, "ktest-lib 9", testHave.Limits, testHave.Instruments},
		"the limits":                   {testHave.Binary, testHave.Library, builtinLimits, testHave.Instruments},
		"the instrument configuration": {testHave.Binary, testHave.Library, testHave.Limits, strings.Repeat("e2", 32)},
	} {
		lk, _, err := loadLocked(lp, have)
		if err != nil {
			t.Fatal(err)
		}
		d := lk.daemonDiffs()
		if len(d) != 1 || !strings.HasPrefix(d[0], what) {
			t.Errorf("%s: %v", what, d)
		}
	}
}

// TestInstrumentConfig: the instrument configuration's text is the same
// whatever the order of the flags, and other provenance does not enter it.
func TestInstrumentConfig(t *testing.T) {
	a := instrumentConfig("vcan0", "none", map[string]string{"psu.psu1.addr": "h:1", "scope.s1.addr": "h:2",
		"version": "dev", "limits.source": "x"}, map[string]string{"poll.psu": "100ms"})
	b := instrumentConfig("vcan0", "none", map[string]string{"scope.s1.addr": "h:2", "psu.psu1.addr": "h:1",
		"version": "other"}, map[string]string{"poll.psu": "100ms"})
	if a != b {
		t.Errorf("%q\n%q", a, b)
	}
	want := "bus=vcan0\ndbc=none\npoll.psu=100ms\npsu.psu1.addr=h:1\nscope.s1.addr=h:2\n"
	if a != want {
		t.Errorf("%q, want %q", a, want)
	}
}

// procBench is a daemon with no instruments and a loop that does only what
// procedures ask of it, recording to memory.
type procBench struct {
	p    *procs
	l    *loop
	file *record.Writer
	buf  bytes.Buffer
	done chan struct{}
}

func newProcBench(t *testing.T, dir string, lk *locked) *procBench {
	t.Helper()
	b := &procBench{done: make(chan struct{})}
	cfg := record.Config{Bus: "vcan0", Codec: logb.CodecNone, Live: true, Meta: map[string]string{}}
	mk := func(w io.Writer) *record.Writer {
		rw, err := record.New(w, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return rw
	}
	b.file = mk(&b.buf)
	w := &tee{file: b.file, rack: mk(io.Discard), sub: mk(io.Discard)}
	d := &daemon{bus: "vcan0", supplies: map[string]int{}, scopes: map[string]int{},
		reqs: make(chan *request), closing: make(chan struct{}), gone: make(chan struct{}),
		jobs: make(chan func(*loop), 64)}
	b.p = newProcs(d, dir, channel.NewTable())
	b.p.lock = lk
	if err := b.p.declare(w); err != nil {
		t.Fatal(err)
	}
	b.l = &loop{d: d, w: w}
	go func() {
		defer close(b.done)
		for {
			select {
			case r := <-d.reqs:
				b.l.seq++
				if r.kind == reqJob {
					r.reply <- r.job(b.l, time.Now(), b.l.seq)
				} else {
					r.reply <- reply{seq: b.l.seq, err: errors.New("this bench has no instruments")}
				}
			case f := <-d.jobs:
				f(b.l)
			case <-d.closing:
				return
			}
		}
	}()
	return b
}

// start asks for a run as the local operator does, and waits for the run to
// end if one started.
func (b *procBench) start(t *testing.T, file, test string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(runBody{File: file, Test: test})
	r := withCaller(httptest.NewRequest("POST", "/procedures/run", bytes.NewReader(body)),
		caller{role: roleOperate, via: record.ViaUnix})
	w := httptest.NewRecorder()
	b.p.start(w, r)
	for deadline := time.Now().Add(5 * time.Second); ; {
		b.p.mu.Lock()
		idle := b.p.cur == nil
		b.p.mu.Unlock()
		if idle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run did not end")
		}
		time.Sleep(time.Millisecond)
	}
	return w
}

// runs closes the bench and reads back every procedure.run record.
func (b *procBench) runs(t *testing.T) []map[string]any {
	t.Helper()
	close(b.p.d.closing)
	<-b.done
	if err := b.file.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := logb.NewReader(bytes.NewReader(b.buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for {
		bt, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if bt.Schema.Name != "procedure.run" {
			continue
		}
		for i := range int(bt.Count) {
			m := map[string]any{}
			for f, fd := range bt.Schema.Fields {
				if v, err := bt.Value(i, f); err == nil {
					m[fd.Name] = v
				}
			}
			out = append(out, m)
		}
	}
}

// event picks the records of one event, in order.
func event(rs []map[string]any, ev uint8) []map[string]any {
	var out []map[string]any
	for _, r := range rs {
		if r["event"] == uint64(ev) {
			out = append(out, r)
		}
	}
	return out
}

// TestLockedRun: a run whose files and daemon match the lock is accredited; a
// procedure with one byte changed, in its own file or in a resource it pulls
// in, is refused or marked as the file says; and engineering mode accredits
// nothing.
func TestLockedRun(t *testing.T) {
	t.Run("matching run is accredited", func(t *testing.T) {
		dir, lp := lockDir(t, "")
		lk, _, err := loadLocked(lp, testHave)
		if err != nil {
			t.Fatal(err)
		}
		b := newProcBench(t, dir, lk)
		if w := b.start(t, "hello.robot", "Hello"); w.Code != http.StatusAccepted {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		rs := b.runs(t)
		starts, ends := event(rs, runStart), event(rs, runEnd)
		if len(starts) != 1 || len(ends) != 1 {
			t.Fatalf("records %v", rs)
		}
		for _, r := range []map[string]any{starts[0], ends[0]} {
			if r["accredited"] != true || r["mode"] != "locked" || r["lock"] != lk.hash || r["message"] != "" {
				t.Errorf("accredited %v, mode %v, lock %v, message %q", r["accredited"], r["mode"], r["lock"], r["message"])
			}
		}
		if ends[0]["outcome"] != uint64(outcomeCode("ok")) {
			t.Errorf("outcome %v", ends[0]["outcome"])
		}
	})

	for _, changed := range []string{"hello.robot", "common.resource"} {
		t.Run("changed "+changed+" is refused", func(t *testing.T) {
			dir, lp := lockDir(t, "")
			lk, _, err := loadLocked(lp, testHave)
			if err != nil {
				t.Fatal(err)
			}
			flipByte(t, filepath.Join(dir, changed))
			b := newProcBench(t, dir, lk)
			w := b.start(t, "hello.robot", "Hello")
			want := "differs from locked.json: " + changed + " (sha256 "
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), want) {
				t.Errorf("%d %s", w.Code, w.Body)
			}
			rs := b.runs(t)
			if len(event(rs, runStart)) != 0 {
				t.Error("a refused run started")
			}
			ref := event(rs, runRefused)
			if len(ref) != 1 || !strings.Contains(ref[0]["message"].(string), want) || ref[0]["mode"] != "locked" ||
				ref[0]["accredited"] != false {
				t.Errorf("refusal %v", ref)
			}
		})
		t.Run("changed "+changed+" is marked", func(t *testing.T) {
			dir, lp := lockDir(t, "mark")
			lk, _, err := loadLocked(lp, testHave)
			if err != nil {
				t.Fatal(err)
			}
			flipByte(t, filepath.Join(dir, changed))
			b := newProcBench(t, dir, lk)
			if w := b.start(t, "hello.robot", "Hello"); w.Code != http.StatusAccepted {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			rs := b.runs(t)
			starts, ends := event(rs, runStart), event(rs, runEnd)
			if len(starts) != 1 || len(ends) != 1 {
				t.Fatalf("records %v", rs)
			}
			msg := starts[0]["message"].(string)
			if starts[0]["accredited"] != false || starts[0]["mode"] != "locked" ||
				!strings.HasPrefix(msg, "not accredited: it differs from locked.json: "+changed) {
				t.Errorf("accredited %v, mode %v, message %q", starts[0]["accredited"], starts[0]["mode"], msg)
			}
			if ends[0]["accredited"] != false || ends[0]["outcome"] != uint64(outcomeCode("ok")) {
				t.Errorf("end %v", ends[0])
			}
		})
	}

	t.Run("a file the lock does not list is refused", func(t *testing.T) {
		dir, lp := lockDir(t, "")
		lk, _, err := loadLocked(lp, testHave)
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, "new.robot"), lockProc)
		b := newProcBench(t, dir, lk)
		w := b.start(t, "new.robot", "Hello")
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "new.robot, which the lock does not list") {
			t.Errorf("%d %s", w.Code, w.Body)
		}
		b.runs(t)
	})

	t.Run("a changed binary refuses a matching procedure", func(t *testing.T) {
		dir, lp := lockDir(t, "")
		have := testHave
		have.Binary = strings.Repeat("00", 32)
		lk, _, err := loadLocked(lp, have)
		if err != nil {
			t.Fatal(err)
		}
		b := newProcBench(t, dir, lk)
		w := b.start(t, "hello.robot", "Hello")
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "the binary (sha256 000000000000…") {
			t.Errorf("%d %s", w.Code, w.Body)
		}
		b.runs(t)
	})

	t.Run("engineering mode accredits nothing", func(t *testing.T) {
		dir, _ := lockDir(t, "")
		b := newProcBench(t, dir, nil)
		if w := b.start(t, "hello.robot", "Hello"); w.Code != http.StatusAccepted {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		rs := b.runs(t)
		s := event(rs, runStart)
		if len(s) != 1 || s[0]["accredited"] != false || s[0]["mode"] != "engineering" || s[0]["lock"] != "" {
			t.Errorf("%v", s)
		}
	})
}

// flipByte changes one byte of a file in a way that still parses: the last
// letter of "hello" or "Hello" becomes a capital.
func flipByte(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.LastIndex(b, []byte("llo"))
	if i < 0 {
		t.Fatalf("%s has no llo", path)
	}
	b[i+2] = 'O'
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
