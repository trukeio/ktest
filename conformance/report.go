// Package conformance checks an implementation of the Ktest peer protocol
// (doc/peer-protocol.md) against the specification, from the outside, over
// the network: whatever language it is written in.
//
// CheckSecondary acts as a master towards a DUT rack and checks what it
// answers and streams (§2, §6, §7, §11). It is intrusive by nature: it
// takes the lease, arms, changes the mode, runs an action if asked to, and
// disarms. Run it against a rack on a bench, never during a test.
//
// Master runs a reference DUT rack, on package secondary, for a master to
// connect to, injects the faults a master must protect against, and checks
// what the master does that a DUT rack can see (§9.1–§9.3). What a master
// does about a fault — removing the stress — happens in its own world; the
// report's timeline says when each fault began and ended, for the master's
// own recording to be held to (§9.4).
//
// The monitors' semantics (§5) are checked offline on a rack's recording, by
// cmd/monitor-check -live: the same evaluator, replayed, must give every
// transition the rack recorded live, to the nanosecond.
package conformance

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Status is how a check came out.
type Status int

const (
	Pass Status = iota
	Fail
	Skip // could not be checked: what it needs was not given, or an earlier check failed
	Info // a measurement the specification states as a target, reported and not judged
)

func (s Status) String() string { return [...]string{"pass", "FAIL", "skip", "info"}[s] }

// Result is one check.
type Result struct {
	Section string // the specification's section, §6.3
	Title   string // what is required, in a phrase
	Status  Status
	Detail  string // what was seen, for a failure or a skip
}

// Event is one fault the reference rack injected, or something it saw.
type Event struct {
	At   time.Time
	What string
}

// Report is a run's results, in the order they were checked, and for a
// master's run, its timeline and its faults.
type Report struct {
	Subject  string
	Results  []Result
	Timeline []Event
	Faults   []Fault
}

// The faults a master is put through (§9.4), and the rule each must make
// it take: dut_abort for an abort, monitoring_lost for the rest.
const (
	FaultAbort    = "abort"    // mon.abort is dev
	FaultFrozen   = "frozen"   // the rack stands still; its watermark stops
	FaultLink     = "link"     // every subscription ended, every request refused
	FaultContract = "contract" // the rack comes back stating another contract
	FaultProtocol = "protocol" // the rack comes back stating another protocol version
)

// Fault is one fault, from when it began to when it ended, by the
// reference rack's clock.
type Fault struct {
	Kind  string    `json:"kind"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// FaultFile is faults.json, which Master writes beside the master's
// settings: what a master's own recording must answer to. For each fault
// the master takes its rule (dut_abort for an abort, monitoring_lost for
// the rest) within its reaction time of the start, and the rule clears after
// the end. The times are the reference rack's clock; the master's recording
// relates them to its own by the clock offset it recorded (§8).
type FaultFile struct {
	Rack   string  `json:"rack"`
	Faults []Fault `json:"faults"`
}

func (r *Report) pass(section, title, format string, a ...any) {
	r.Results = append(r.Results, Result{section, title, Pass, fmt.Sprintf(format, a...)})
}

func (r *Report) fail(section, title, format string, a ...any) {
	r.Results = append(r.Results, Result{section, title, Fail, fmt.Sprintf(format, a...)})
}

func (r *Report) info(section, title, format string, a ...any) {
	r.Results = append(r.Results, Result{section, title, Info, fmt.Sprintf(format, a...)})
}

func (r *Report) skip(section, title, format string, a ...any) {
	r.Results = append(r.Results, Result{section, title, Skip, fmt.Sprintf(format, a...)})
}

// check records a pass when err is nil and a failure with it otherwise.
func (r *Report) check(section, title string, err error) bool {
	if err != nil {
		r.fail(section, title, "%v", err)
		return false
	}
	r.pass(section, title, "")
	return true
}

// Failed is how many checks failed.
func (r *Report) Failed() int {
	n := 0
	for _, x := range r.Results {
		if x.Status == Fail {
			n++
		}
	}
	return n
}

// Write prints the report as text.
func (r *Report) Write(w io.Writer) {
	fmt.Fprintf(w, "%s\n\n", r.Subject)
	counts := [4]int{}
	for _, x := range r.Results {
		counts[x.Status]++
		line := fmt.Sprintf("  %-4s  %-6s %s", x.Status, x.Section, x.Title)
		if x.Detail != "" {
			line += ": " + strings.ReplaceAll(x.Detail, "\n", "\n          ")
		}
		fmt.Fprintln(w, line)
	}
	if len(r.Timeline) > 0 {
		fmt.Fprintf(w, "\ntimeline (UTC):\n")
		for _, e := range r.Timeline {
			fmt.Fprintf(w, "  %s  %s\n", e.At.UTC().Format("15:04:05.000000"), e.What)
		}
	}
	fmt.Fprintf(w, "\n%d passed, %d failed, %d skipped, %d measured\n", counts[Pass], counts[Fail], counts[Skip], counts[Info])
}
