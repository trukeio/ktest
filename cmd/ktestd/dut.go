//go:build linux

package main

import (
	"net/http"

	"github.com/trukeio/logb/dbc"

	"github.com/trukeio/ktest/channel"
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
// The contract, the monitors and the DUT's modes and actions are package
// secondary's Core, the same as every DUT rack built on it. The monitors are
// evaluated on a goroutine of their own, fed from the recorder through a tap
// that never blocks the loop. Their transitions come back to the loop and are
// recorded like any other source:
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

// The DUT rack's streams, as package secondary lays them out for every DUT
// rack, ktestd's included (doc/peer-protocol.md §7.4).
const (
	monSource = "the ktestd monitor evaluator"
	dutSource = "the ktestd DUT rack"
)

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
