//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/trukeio/logb"
	"github.com/trukeio/logb/dbc"

	"github.com/trukeio/ktest/channel"
	"github.com/trukeio/ktest/monitor"
	"github.com/trukeio/ktest/peer"
	"github.com/trukeio/ktest/record"
	"github.com/trukeio/ktest/stream"
)

// # The DUT rack
//
// With -contract, the daemon is a DUT rack (handoff §4.3, §4.4): it runs the
// contract's monitors live, over the records it writes, and serves them with
// everything else it records on /stream/sub, record by record.
//
// The monitors are evaluated on a goroutine of their own, fed from the
// recorder through a tap that never blocks the loop (package monitor, Online).
// Their transitions come back to the loop and are recorded like any other
// source:
//
//	mon.<name>      a held state, ok, dev, unknown, inactive or settling, at the
//	                time of the sample or deadline that caused it, with the
//	                state before it and the cause
//	mon.watermark   a held time, upto: every transition at or before it has been
//	                recorded. The record's own time is the same instant.
//	mon.gap         records the evaluator lost or got too late to use, after
//	                which every monitor is unknown until its inputs are known again
//
// A contract is loaded before anything is written, against the streams this
// daemon will record, decoded DBC messages included, so that a contract that
// names a channel the rack will never carry refuses the start.

// contractChannels is every stream this daemon's configuration says it will
// record, for a contract to be checked against before the recording starts.
// The bus's own streams are left out: a contract reads decoded messages, never
// raw frames (C4).
func contractChannels(bus string, db *dbc.File, supplies supplyFlags, scopes scopeFlags, procs bool) (*channel.Table, error) {
	t := channel.NewTable()
	if err := declareDecoded(t, bus, db); err != nil {
		return nil, err
	}
	for _, s := range supplies {
		for ch := 1; ch <= s.channels; ch++ {
			t.Stream(chanSchema(s.name, ch))
			t.Stream(measSchema(s.name, ch))
		}
	}
	for _, s := range scopes {
		t.Stream(timebaseSchema(s.name))
		t.Stream(acqSchema(s.name))
		for ch := 1; ch <= s.channels; ch++ {
			t.Stream(vertSchema(s.name, ch))
		}
	}
	t.Stream(panelSchema())
	if procs {
		t.Stream(procRunSchema())
		t.Stream(procStepSchema())
		t.Stream(procResultSchema())
	}
	return t, nil
}

// declareDecoded declares, in t, the stream of every message of db, as the
// recorder will declare it on first sight.
func declareDecoded(t *channel.Table, bus string, db *dbc.File) error {
	if db == nil {
		return nil
	}
	for _, m := range db.Messages {
		s, err := record.DecodedSchema(bus, db, m)
		if err != nil {
			// The recorder will not decode this message either, and says so
			// when it meets one; a contract naming it fails to resolve.
			continue
		}
		t.Stream(s)
	}
	return nil
}

// loadContract loads -contract and states it in the recording: name, hash and
// source, the resource's hash, and both files embedded, as limits.json is.
func loadContract(p string, ch *channel.Table, cfg *record.Config) (*monitor.Contract, error) {
	c, err := monitor.Load(p, ch)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	cfg.Meta["peer.protocol"] = strconv.Itoa(peer.Protocol)
	cfg.Meta["dut.contract"] = c.Name
	cfg.Meta["dut.contract.sha256"] = c.Hash
	cfg.Meta["dut.contract.source"] = p
	cfg.Attach["dut-interface.json"] = raw
	if c.Resource != "" {
		rp := filepath.Join(filepath.Dir(p), c.Resource)
		rb, err := os.ReadFile(rp)
		if err != nil {
			return nil, err
		}
		cfg.Meta["dut.resource"] = c.Resource
		cfg.Meta["dut.resource.sha256"] = c.ResourceHash
		cfg.Attach[filepath.Base(c.Resource)] = rb
	}
	return c, nil
}

// dutMon is the loop's side of the live monitors.
type dutMon struct {
	c      *monitor.Contract
	tap    *monitor.Tap
	out    chan monitor.Output
	tick   time.Duration
	mons   map[string]*teeStream
	wm     *teeStream
	gap    *teeStream
	lastWM int64
}

// The layouts. mon.<name>: t 0–7, state 8, from 9, cause 16–79.
// mon.watermark: t 0–7, upto 8–15. mon.gap: t 0–7, from 8–15, to 16–23,
// lost 24–27, reason 32–127.
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
)

// stateText is the states by the number the recording gives them.
func stateText() logb.ValueToText {
	c := logb.ValueToText{Default: "?"}
	for i, s := range monitor.States {
		c.Keys = append(c.Keys, float64(i))
		c.Texts = append(c.Texts, string(s))
	}
	return c
}

func stateCode(s monitor.State) byte {
	for i, x := range monitor.States {
		if x == s {
			return byte(i)
		}
	}
	return byte(len(monitor.States))
}

func monSchema(c *monitor.Contract, name string) *logb.Schema {
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
		"source":   "the ktestd monitor evaluator, contract " + c.Name,
		"contract": c.Hash,
		"clock": "the time of the sample or the deadline that caused the transition, " +
			"never the time it was evaluated (handoff §4.3)",
	})
}

func watermarkSchema() *logb.Schema {
	return record.Schema("mon.watermark", 8*wmBytes, []logb.Field{
		{Name: "upto", BitOffset: 8 * wmUpto, BitWidth: 64, Type: logb.TypeSint, Unit: "ns", Hold: true,
			Desc: "every monitor transition at or before this time, Unix nanoseconds, has been recorded"},
	}, map[string]string{
		"source": "the ktestd monitor evaluator",
		"clock":  "the watermark itself: the daemon's clock less the evaluator's reorder window",
	})
}

func gapSchema() *logb.Schema {
	return record.Schema("mon.gap", 8*gapBytes, []logb.Field{
		{Name: "from", BitOffset: 8 * gapFrom, BitWidth: 64, Type: logb.TypeSint, Unit: "ns",
			Desc: "the first record the evaluator did not use, by its own time, Unix nanoseconds"},
		{Name: "to", BitOffset: 8 * gapTo, BitWidth: 64, Type: logb.TypeSint, Unit: "ns", Desc: "the last"},
		uintF("lost", gapLost, 32, "", "how many records"),
		strF("reason", gapReason, 96, "overflow: the evaluator's queue was full; late: a record behind the watermark"),
	}, map[string]string{
		"source": "the ktestd monitor evaluator",
		"clock": "the watermark when the gap was noticed: every monitor is unknown from here " +
			"until its inputs are known again",
	})
}

// declare lays out the monitor streams and states every monitor unknown from
// the start, so that a reader meets each one held from the first segment.
func (m *dutMon) declare(w *tee, t time.Time) error {
	m.mons = map[string]*teeStream{}
	for _, name := range m.c.Order {
		s, err := w.NewStream(func() *logb.Schema { return monSchema(m.c, name) })
		if err != nil {
			return err
		}
		m.mons[name] = s
	}
	var err error
	if m.wm, err = w.NewStream(watermarkSchema); err != nil {
		return err
	}
	if m.gap, err = w.NewStream(gapSchema); err != nil {
		return err
	}
	for _, name := range m.c.Order {
		if err := m.transition(w, monitor.Transition{T: t.UnixNano(), Monitor: name,
			From: monitor.Unknown, To: monitor.Unknown, Cause: "start"}); err != nil {
			return err
		}
	}
	return nil
}

func (m *dutMon) transition(w *tee, x monitor.Transition) error {
	return w.Write(time.Unix(0, x.T), m.mons[x.Monitor], func(rec []byte) {
		rec[monState] = stateCode(x.To)
		rec[monFrom] = stateCode(x.From)
		putStr(rec, monCause, 64, x.Cause)
	})
}

// write records what the evaluator produced: its transitions, the gap that
// caused some of them, then the watermark over all of it.
func (m *dutMon) write(w *tee, o monitor.Output) {
	for _, x := range o.Transitions {
		if err := m.transition(w, x); err != nil {
			fatalf("write: %v", err)
		}
	}
	if g := o.Gap; g != nil {
		err := w.Write(time.Unix(0, g.T), m.gap, func(rec []byte) {
			binary.LittleEndian.PutUint64(rec[gapFrom:], uint64(g.From))
			binary.LittleEndian.PutUint64(rec[gapTo:], uint64(g.To))
			binary.LittleEndian.PutUint32(rec[gapLost:], uint32(min(g.Lost, 1<<32-1)))
			putStr(rec, gapReason, 96, g.Reason)
		})
		if err != nil {
			fatalf("write: %v", err)
		}
	}
	if wm := o.Watermark; wm != 0 && wm > m.lastWM {
		m.lastWM = wm
		err := w.Write(time.Unix(0, wm), m.wm, func(rec []byte) {
			binary.LittleEndian.PutUint64(rec[wmUpto:], uint64(wm))
		})
		if err != nil {
			fatalf("write: %v", err)
		}
	}
}

// # /stream/sub
//
//	GET /stream/sub?ch=mon.*,DutStatus
//
// The recording, uncompressed, one record to a DATA frame, written the moment
// the record is: for a peer that reacts to what it reads (§4.4). ch is a
// comma-separated list of stream names or globs, path.Match's; the stream's
// metadata — the rack's provenance among it — and mon.watermark, mon.gap and
// mode.* are always sent, since a subscriber cannot judge what it reads
// without them. A subscriber that falls behind is disconnected, as on
// /stream/live, and the recording is unaffected.

// alwaysSent are the streams every /stream/sub subscriber is sent.
var alwaysSent = []string{"mon.watermark", "mon.gap", "mode", "mode.*"}

func streamSub(hub *stream.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		globs := append([]string(nil), alwaysSent...)
		for _, g := range strings.Split(r.URL.Query().Get("ch"), ",") {
			if g = strings.TrimSpace(g); g == "" {
				continue
			}
			if _, err := path.Match(g, ""); err != nil {
				httpError(w, http.StatusBadRequest, 0, "ch: %q is not a stream name or a glob: %v", g, err)
				return
			}
			globs = append(globs, g)
		}
		hub.ServeStreams(w, r, func(name string) bool {
			for _, g := range globs {
				if ok, _ := path.Match(g, name); ok {
					return true
				}
			}
			return false
		})
	}
}
