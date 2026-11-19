// Command monitor-check replays a recording through a contract's monitors,
// offline, with the evaluator the DUT rack runs live (handoff §4.10).
//
//	monitor-check [-json] [-expect ok | -expect dev@t1,t2,...] [-tol 50ms] [-mode mode.applied] \
//	    dut-interface.json recording.logb
//
// It prints every transition, stamped in seconds from the start of the
// recording, with what caused it. The evaluator is pure, so the transitions
// are the ones the rack computed live from the same records, to the
// nanosecond, and a contract can be tried against yesterday's recording
// before it is signed off.
//
// -expect ok fails if any monitor deviates. -expect dev@t1,t2 fails unless a
// deviation begins within -tol of each time given, and also if one begins
// anywhere else: the list says where the deviations are, all of them.
//
// The mode comes from the recording's -mode channel, a text field holding the
// mode's name. Without one, monitors that watch a mode stay inactive, and
// monitor-check says so.
//
// Exit status: 0 the expectation holds (or none was given), 1 it does not,
// 2 the contract or the recording could not be read.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rveen/logb"

	"github.com/rveen/ktest/channel"
	"github.com/rveen/ktest/expr"
	"github.com/rveen/ktest/monitor"
)

func main() {
	jsonOut := flag.Bool("json", false, "print the transitions as JSON")
	expect := flag.String("expect", "", "ok, or dev@t1,t2,...: the deviations expected, in seconds from the recording's start")
	tol := flag.Duration("tol", 50*time.Millisecond, "how far from an expected time a deviation may begin")
	mode := flag.String("mode", "mode.applied", "the channel holding the DUT's mode")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: monitor-check [flags] dut-interface.json recording.logb\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}
	exp, err := parseExpect(*expect)
	if err != nil {
		fmt.Fprintf(os.Stderr, "monitor-check: -expect: %v\n", err)
		os.Exit(2)
	}
	res, err := check(flag.Arg(0), flag.Arg(1), *mode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "monitor-check: %v\n", err)
		os.Exit(2)
	}
	for _, n := range res.notes {
		fmt.Fprintf(os.Stderr, "monitor-check: %s\n", n)
	}
	if *jsonOut {
		writeJSON(os.Stdout, res)
	} else {
		writeText(os.Stdout, res)
	}
	if exp == nil {
		return
	}
	if fails := exp.judge(res, *tol); len(fails) > 0 {
		for _, f := range fails {
			fmt.Fprintf(os.Stderr, "monitor-check: %s\n", f)
		}
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "monitor-check: %s\n", exp.describe(*tol))
}

// result is a replay: the transitions, and the recording's time origin.
type result struct {
	contract *monitor.Contract
	start    int64 // the recording's first record, ns on its axis
	end      int64 // its last
	records  int
	tr       []monitor.Transition
	notes    []string
}

type sample struct {
	t      int64
	stream string
	rec    []byte
	mode   *string
}

// check replays recording through contract.
func check(contractPath, recPath, modeChannel string) (*result, error) {
	// The first pass learns the recording's streams, which the contract's
	// inputs are resolved against; the second reads the records.
	table := channel.NewTable()
	if err := scan(recPath, func(r *logb.Reader) {
		r.OnSchema = func(s *logb.Schema, _ uint16) { table.Stream(s) }
	}, nil); err != nil {
		return nil, err
	}
	c, err := monitor.Load(contractPath, table)
	if err != nil {
		return nil, err
	}
	e, err := monitor.NewEvaluator(c)
	if err != nil {
		return nil, err
	}
	res := &result{contract: c, start: math.MaxInt64, end: math.MinInt64}

	var modeRef *channel.Ref
	if ref, err := table.Lookup(modeChannel); err == nil && ref.Field >= 0 && ref.Type.Kind == expr.String {
		modeRef = &ref
	} else if len(c.Modes) > 0 {
		res.notes = append(res.notes, fmt.Sprintf("the recording has no text channel %s, so no mode is ever entered: "+
			"monitors that watch a mode stay inactive", modeChannel))
	}
	want := map[string]bool{}
	for _, s := range e.Streams() {
		want[s] = true
	}

	var samples []sample
	err = scan(recPath, nil, func(b *logb.Batch) error {
		for i := 0; i < int(b.Count); i++ {
			// Every record moves the recording's span; only the inputs' and
			// the mode's are kept.
			ax, err := b.Axis(i)
			if err != nil {
				return err
			}
			d, err := ax.Duration(b.Schema.AxisExp)
			if err != nil {
				return err
			}
			t := int64(d)
			res.start, res.end = min(res.start, t), max(res.end, t)
			name := b.Schema.Name
			isMode := modeRef != nil && name == modeRef.Stream()
			if !want[name] && !isMode {
				continue
			}
			rec, err := b.Record(i)
			if err != nil {
				return err
			}
			rec = slices.Clone(rec)
			res.records++
			if want[name] {
				samples = append(samples, sample{t: t, stream: name, rec: rec})
			}
			if isMode {
				v := channel.Decode(b.Schema, rec, modeRef.Field)
				m := ""
				if v.Known {
					m = v.Str
				}
				samples = append(samples, sample{t: t, mode: &m})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// A recording is written stream by stream, in batches, so its records
	// are in time order within a stream and not across streams. The
	// evaluator takes them in time order; ties keep the file's order.
	sort.SliceStable(samples, func(i, j int) bool { return samples[i].t < samples[j].t })

	lastMode := ""
	for _, s := range samples {
		var tr []monitor.Transition
		if s.mode != nil {
			if *s.mode == lastMode {
				continue // a held mode restated at a segment is not a new entry
			}
			lastMode = *s.mode
			if *s.mode != "" && c.Modes[*s.mode] == nil {
				res.notes = append(res.notes, fmt.Sprintf("the recording enters mode %q, which the contract does not declare; "+
					"taken as no mode", *s.mode))
				tr, err = e.Mode(s.t, "")
			} else {
				tr, err = e.Mode(s.t, *s.mode)
			}
		} else {
			tr, err = e.Record(s.stream, s.t, s.rec)
		}
		if err != nil {
			return nil, err
		}
		res.tr = append(res.tr, tr...)
	}
	// Deadlines up to the recording's end are evaluated, and none after: the
	// recording says nothing about what came after it.
	if res.end > math.MinInt64 {
		tr, err := e.Advance(res.end)
		if err != nil {
			return nil, err
		}
		res.tr = append(res.tr, tr...)
	}
	if res.start == math.MaxInt64 {
		res.start = 0
	}
	return res, nil
}

// scan reads a recording to its end, with setup applied to the reader and
// each batch given to batch.
func scan(path string, setup func(*logb.Reader), batch func(*logb.Batch) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r, err := logb.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	if setup != nil {
		setup(r)
	}
	for {
		b, err := r.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %v", path, err)
		}
		if batch != nil {
			if err := batch(b); err != nil {
				return fmt.Errorf("%s: %s: %v", path, b.Schema.Name, err)
			}
		}
	}
}

func (r *result) rel(t int64) float64 { return float64(t-r.start) / 1e9 }

func writeText(w io.Writer, r *result) {
	fmt.Fprintf(w, "contract %s, sha256 %s\n", r.contract.Name, r.contract.Hash)
	for _, t := range r.tr {
		fmt.Fprintf(w, "%+12.6fs  %-12s %-8s → %-8s  %s\n", r.rel(t.T), t.Monitor, t.From, t.To, t.Cause)
	}
	fmt.Fprintf(w, "%d transitions from %d records over %.3fs\n", len(r.tr), r.records, r.rel(r.end))
}

func writeJSON(w io.Writer, r *result) {
	type tj struct {
		T       float64 `json:"t"`
		TNs     int64   `json:"t_ns"`
		Monitor string  `json:"monitor"`
		From    string  `json:"from"`
		To      string  `json:"to"`
		Cause   string  `json:"cause"`
	}
	out := struct {
		Contract    string `json:"contract"`
		Hash        string `json:"sha256"`
		Records     int    `json:"records"`
		Transitions []tj   `json:"transitions"`
	}{Contract: r.contract.Name, Hash: r.contract.Hash, Records: r.records, Transitions: []tj{}}
	for _, t := range r.tr {
		out.Transitions = append(out.Transitions, tj{r.rel(t.T), t.T, t.Monitor, string(t.From), string(t.To), t.Cause})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(out)
}

// expectation is -expect: none of dev, or dev at the times given.
type expectation struct {
	ok    bool
	times []float64
}

func parseExpect(s string) (*expectation, error) {
	switch {
	case s == "":
		return nil, nil
	case s == "ok":
		return &expectation{ok: true}, nil
	case strings.HasPrefix(s, "dev@"):
		e := &expectation{}
		for _, f := range strings.Split(strings.TrimPrefix(s, "dev@"), ",") {
			v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(f), "s"), 64)
			if err != nil {
				return nil, fmt.Errorf("%q is not a time in seconds", f)
			}
			e.times = append(e.times, v)
		}
		return e, nil
	}
	return nil, fmt.Errorf("%q: write ok, or dev@t1,t2,... with times in seconds from the recording's start", s)
}

// judge says what about the replay contradicts the expectation.
func (e *expectation) judge(r *result, tol time.Duration) []string {
	var devs []monitor.Transition
	for _, t := range r.tr {
		if t.To == monitor.Dev {
			devs = append(devs, t)
		}
	}
	var fails []string
	if e.ok {
		for _, d := range devs {
			fails = append(fails, fmt.Sprintf("%s deviates at %+.6fs (%s), and -expect ok says nothing does", d.Monitor, r.rel(d.T), d.Cause))
		}
		return fails
	}
	matched := make([]bool, len(devs))
	for _, want := range e.times {
		found := false
		for i, d := range devs {
			if math.Abs(r.rel(d.T)-want) <= tol.Seconds() {
				found, matched[i] = true, true
			}
		}
		if !found {
			fails = append(fails, fmt.Sprintf("no deviation begins within %v of %gs", tol, want))
		}
	}
	for i, d := range devs {
		if !matched[i] {
			fails = append(fails, fmt.Sprintf("%s deviates at %+.6fs (%s), which -expect does not list", d.Monitor, r.rel(d.T), d.Cause))
		}
	}
	return fails
}

func (e *expectation) describe(tol time.Duration) string {
	if e.ok {
		return "no monitor deviates, as expected"
	}
	return fmt.Sprintf("every deviation begins within %v of one of the %d times expected, and each time has one", tol, len(e.times))
}
