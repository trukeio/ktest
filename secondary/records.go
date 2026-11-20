package secondary

import (
	"encoding/binary"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/monitor"
	"github.com/trukeio/ktest/record"
)

// # The records a DUT rack streams
//
// The layouts of doc/peer-protocol.md §7.4, in one place, for every DUT rack
// built here: this package's Server and ktestd's -contract. Each stream is
// made with record.Schema, so its time axis is first, in nanoseconds. Strings
// are fixed width, UTF-8, NUL-padded, cut at a rune boundary.
//
// source is what a schema's "source" metadata says wrote the stream; it is
// the only thing a rack may vary.

// The layouts, in bytes. mon.<name>: t 0–7, state 8, from 9, cause 16–79.
// mon.watermark: t 0–7, upto 8–15. mon.gap: t 0–7, from 8–15, to 16–23,
// lost 24–27, reason 32–127. mode: t 0–7, seq 8–11, event 12, requested
// 16–47, applied 48–79, by 80–111, message 112–271. dut.action.<name>: t
// 0–7, seq 8–11, run 12–15, event 16, outcome 17, via 18, by 24–55, mode
// 56–87, message 88–247.
const (
	monState = 8
	monFrom  = 9
	monCause = 16
	monBytes = monCause + 64

	wmUpto  = 8
	wmBytes = 16

	gapFrom   = 8
	gapTo     = 16
	gapLost   = 24
	gapReason = 32
	gapBytes  = gapReason + 96

	mdSeq       = 8
	mdEvent     = 12
	mdRequested = 16
	mdApplied   = 48
	mdBy        = 80
	mdMessage   = 112
	mdBytes     = mdMessage + 160

	acSeq     = 8
	acRun     = 12
	acEvent   = 16
	acOutcome = 17
	acVia     = 18
	acBy      = 24
	acMode    = 56
	acMessage = 88
	acBytes   = acMessage + 160
)

// The events of the mode stream.
const (
	ModeStart     = 0
	ModeRequested = 1
	ModeApplied   = 2
	ModeFailed    = 3
)

// The events of a dut.action.<name> stream.
const (
	ActionStarted = 1
	ActionEnded   = 2
)

// OutcomeDesc is how the outcome codes read.
const OutcomeDesc = "1 ok, 2 nok, 3 timeout, 4 refused, 5 link_lost, 6 fail, 7 stopped; 0 for none"

// StateCode is a monitor state's protocol code.
func StateCode(s monitor.State) byte {
	for i, x := range monitor.States {
		if x == s {
			return byte(i)
		}
	}
	return byte(len(monitor.States))
}

func stateText() logb.ValueToText {
	c := logb.ValueToText{Default: "?"}
	for i, s := range monitor.States {
		c.Keys = append(c.Keys, float64(i))
		c.Texts = append(c.Texts, string(s))
	}
	return c
}

func uintF(name string, at, bits uint32, desc string) logb.Field {
	return logb.Field{Name: name, BitOffset: 8 * at, BitWidth: bits, Type: logb.TypeUint, Desc: desc}
}

func strF(name string, at, n uint32, desc string) logb.Field {
	return logb.Field{Name: name, BitOffset: 8 * at, BitWidth: 8 * n, Type: logb.TypeString, Desc: desc}
}

func holdF(f logb.Field) logb.Field { f.Hold = true; return f }

// MonSchema is mon.<name>, for a monitor of c.
func MonSchema(c *monitor.Contract, name, source string) *logb.Schema {
	m := c.Monitors[name]
	desc := "ok: " + m.OK
	if m.Debounce > 0 {
		desc += fmt.Sprintf("; debounce %v", m.Debounce)
	}
	if len(m.Modes) > 0 {
		desc += "; in modes " + strings.Join(m.Modes, ", ")
	}
	return record.Schema("mon."+name, 8*monBytes, []logb.Field{
		{Name: "state", BitOffset: 8 * monState, BitWidth: 8, Type: logb.TypeUint, Hold: true, Conv: stateText(),
			Desc: "the monitor's state from this record on: " + desc},
		{Name: "from", BitOffset: 8 * monFrom, BitWidth: 8, Type: logb.TypeUint, Conv: stateText(),
			Desc: "the state before"},
		strF("cause", monCause, 64, "what was evaluated: a stream's sample, a deadline, a mode, a gap"),
	}, map[string]string{
		"source":   source + ", contract " + c.Name,
		"contract": c.Hash,
		"clock": "the time of the sample or the deadline that caused the transition, " +
			"never the time it was evaluated (handoff §4.3)",
	})
}

// WatermarkSchema is mon.watermark.
func WatermarkSchema(source string) *logb.Schema {
	return record.Schema("mon.watermark", 8*wmBytes, []logb.Field{
		{Name: "upto", BitOffset: 8 * wmUpto, BitWidth: 64, Type: logb.TypeSint, Unit: "ns", Hold: true,
			Desc: "every monitor transition at or before this time, Unix nanoseconds, has been recorded"},
	}, map[string]string{
		"source": source,
		"clock":  "the watermark itself: the daemon's clock less the evaluator's reorder window",
	})
}

// GapSchema is mon.gap.
func GapSchema(source string) *logb.Schema {
	return record.Schema("mon.gap", 8*gapBytes, []logb.Field{
		{Name: "from", BitOffset: 8 * gapFrom, BitWidth: 64, Type: logb.TypeSint, Unit: "ns",
			Desc: "the first record the evaluator did not use, by its own time, Unix nanoseconds"},
		{Name: "to", BitOffset: 8 * gapTo, BitWidth: 64, Type: logb.TypeSint, Unit: "ns", Desc: "the last"},
		uintF("lost", gapLost, 32, "how many records"),
		strF("reason", gapReason, 96, "overflow: the evaluator's queue was full; late: a record behind the watermark"),
	}, map[string]string{
		"source": source,
		"clock": "the watermark when the gap was noticed: every monitor is unknown from here " +
			"until its inputs are known again",
	})
}

// ModeSchema is mode.
func ModeSchema(source string) *logb.Schema {
	return record.Schema("mode", 8*mdBytes, []logb.Field{
		uintF("seq", mdSeq, 32, "the request; 0 for the start"),
		uintF("event", mdEvent, 8, "1 a mode was asked for, 2 it was applied, 3 its entry action failed; 0 the start"),
		holdF(strF("requested", mdRequested, 32, "the mode last asked for; empty for none")),
		holdF(strF("applied", mdApplied, 32, "the mode the DUT is in, from the end of its entry action on; empty for none")),
		strF("by", mdBy, record.HolderMax, "the name that asked"),
		strF("message", mdMessage, 160, "why an entry action failed"),
	}, map[string]string{
		"source": source,
		"clock":  "userspace: when it was asked, or when the entry action ended; the monitors settle from the latter",
	})
}

// ActionSchema is dut.action.<name>, for an action that runs what.
func ActionSchema(name, what, source string) *logb.Schema {
	return record.Schema("dut.action."+name, 8*acBytes, []logb.Field{
		uintF("seq", acSeq, 32, "the request that started it"),
		uintF("run", acRun, 32, "the action run, numbered from 1 since the daemon started, across actions"),
		uintF("event", acEvent, 8, "1 it started, 2 it ended"),
		uintF("outcome", acOutcome, 8, "at its end: "+OutcomeDesc),
		uintF("via", acVia, 8, "the listener the request came through: 1 the local socket, 2 TLS"),
		strF("by", acBy, record.HolderMax, "the name that asked"),
		strF("mode", acMode, 32, "the mode it is the entry action of; empty for an action asked for"),
		strF("message", acMessage, 160, "at its end: why it failed"),
	}, map[string]string{
		"source":  source,
		"keyword": what,
		"clock":   "userspace: when it started and ended",
	})
}

// PutTransition fills a mon.<name> record.
func PutTransition(rec []byte, x monitor.Transition) {
	rec[monState] = StateCode(x.To)
	rec[monFrom] = StateCode(x.From)
	PutStr(rec, monCause, 64, x.Cause)
}

// PutWatermark fills a mon.watermark record.
func PutWatermark(rec []byte, upto int64) {
	binary.LittleEndian.PutUint64(rec[wmUpto:], uint64(upto))
}

// PutGap fills a mon.gap record.
func PutGap(rec []byte, g *monitor.Gap) {
	binary.LittleEndian.PutUint64(rec[gapFrom:], uint64(g.From))
	binary.LittleEndian.PutUint64(rec[gapTo:], uint64(g.To))
	binary.LittleEndian.PutUint32(rec[gapLost:], uint32(min(g.Lost, 1<<32-1)))
	PutStr(rec, gapReason, 96, g.Reason)
}

// ModeRecord is one record of the mode stream.
type ModeRecord struct {
	Seq                uint32
	Event              byte
	Requested, Applied string
	By                 string
	Message            string
}

// PutMode fills a mode record.
func PutMode(rec []byte, m ModeRecord) {
	binary.LittleEndian.PutUint32(rec[mdSeq:], m.Seq)
	rec[mdEvent] = m.Event
	PutStr(rec, mdRequested, 32, m.Requested)
	PutStr(rec, mdApplied, 32, m.Applied)
	PutStr(rec, mdBy, record.HolderMax, m.By)
	PutStr(rec, mdMessage, 160, m.Message)
}

// ActionRecord is one record of a dut.action.<name> stream.
type ActionRecord struct {
	Seq, Run uint32
	Event    byte
	Outcome  byte // the protocol's code; 0 at the start
	Via      byte
	By       string
	Mode     string // the mode it enters; "" for an action asked for
	Message  string
}

// PutAction fills a dut.action.<name> record.
func PutAction(rec []byte, a ActionRecord) {
	binary.LittleEndian.PutUint32(rec[acSeq:], a.Seq)
	binary.LittleEndian.PutUint32(rec[acRun:], a.Run)
	rec[acEvent] = a.Event
	rec[acOutcome] = a.Outcome
	rec[acVia] = a.Via
	PutStr(rec, acBy, record.HolderMax, a.By)
	PutStr(rec, acMode, 32, a.Mode)
	PutStr(rec, acMessage, 160, a.Message)
}

// PutStr writes s into rec[at:at+n], cut at a rune boundary to fit, and says
// whether it was cut.
func PutStr(rec []byte, at, n int, s string) bool {
	cut := false
	for len(s) > n {
		cut = true
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	copy(rec[at:at+n], s)
	return cut
}

// AlwaysSent are the streams every /stream/sub subscriber is sent (§7.1).
var AlwaysSent = []string{"mon.watermark", "mon.gap", "mode", "mode.*"}

// SubFilter reads /stream/sub's ch, a comma-separated list of stream names
// or path.Match globs, into the filter a hub serves the subscription with:
// those streams, and AlwaysSent.
func SubFilter(ch string) (func(name string) bool, error) {
	globs := append([]string(nil), AlwaysSent...)
	for _, g := range strings.Split(ch, ",") {
		if g = strings.TrimSpace(g); g == "" {
			continue
		}
		if _, err := path.Match(g, ""); err != nil {
			return nil, fmt.Errorf("ch: %q is not a stream name or a glob: %v", g, err)
		}
		globs = append(globs, g)
	}
	return func(name string) bool {
		for _, g := range globs {
			if ok, _ := path.Match(g, name); ok {
				return true
			}
		}
		return false
	}, nil
}
