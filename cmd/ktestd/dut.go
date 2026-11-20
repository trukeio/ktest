//go:build linux

package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/trukeio/logb"
	"github.com/trukeio/logb/dbc"

	"github.com/trukeio/ktest/channel"
	"github.com/trukeio/ktest/monitor"
	"github.com/trukeio/ktest/peer"
	"github.com/trukeio/ktest/procedure"
	"github.com/trukeio/ktest/record"
	"github.com/trukeio/ktest/secondary"
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
	c, err := monitor.Load(p, ch, procedure.CheckActions)
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

// The DUT rack's streams, as package secondary lays them out for every DUT
// rack, ktestd's included (doc/peer-protocol.md §7.4).
const (
	monSource = "the ktestd monitor evaluator"
	dutSource = "the ktestd DUT rack"
)

// declare lays out the monitor streams and states every monitor unknown from
// the start, so that a reader meets each one held from the first segment.
func (m *dutMon) declare(w *tee, t time.Time) error {
	m.mons = map[string]*teeStream{}
	for _, name := range m.c.Order {
		s, err := w.NewStream(func() *logb.Schema { return secondary.MonSchema(m.c, name, monSource) })
		if err != nil {
			return err
		}
		m.mons[name] = s
	}
	var err error
	if m.wm, err = w.NewStream(func() *logb.Schema { return secondary.WatermarkSchema(monSource) }); err != nil {
		return err
	}
	if m.gap, err = w.NewStream(func() *logb.Schema { return secondary.GapSchema(monSource) }); err != nil {
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
	return w.Write(time.Unix(0, x.T), m.mons[x.Monitor], func(rec []byte) { secondary.PutTransition(rec, x) })
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
		err := w.Write(time.Unix(0, g.T), m.gap, func(rec []byte) { secondary.PutGap(rec, g) })
		if err != nil {
			fatalf("write: %v", err)
		}
	}
	if wm := o.Watermark; wm != 0 && wm > m.lastWM {
		m.lastWM = wm
		err := w.Write(time.Unix(0, wm), m.wm, func(rec []byte) { secondary.PutWatermark(rec, wm) })
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

func streamSub(hub *stream.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filter, err := secondary.SubFilter(r.URL.Query().Get("ch"))
		if err != nil {
			httpError(w, http.StatusBadRequest, 0, "%v", err)
			return
		}
		hub.ServeStreams(w, r, filter)
	}
}
