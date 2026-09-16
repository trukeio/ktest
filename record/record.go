//go:build linux

// Package record turns received CAN frames into a Logb stream.
//
// It writes two kinds of stream and one that is neither.
//
// The raw stream is the wire: identifier, length, flags and payload, exactly
// as the bus produced them. It is written whether or not a database is loaded,
// because a decoded signal is an interpretation and the bytes it came from are
// the evidence for it. Throwing them away means re-importing to check a signal,
// or to fix a database that turned out to be wrong about one.
//
// The decoded streams are one per database message, created the first time
// that message is seen. A vehicle database describes every message on every bus
// of a model range; a recording holds the handful that were on this wire, and
// declaring the rest would bury the four signals someone came to look at under
// two hundred empty ones. Their layout comes from dbc.Schema, so the record is
// the axis followed by the frame's own bytes and nothing is shifted or scaled
// on the way in.
//
// The third is the drop stream, and it exists because of §10.5: a gap that does
// not say it is a gap is the worst failure this system can produce. The kernel
// reports interior losses through SO_RXQ_OVFL and reports a lost tail not at
// all, so what this package can honestly write is the losses the kernel
// admitted to. See Writer.Frame.
//
// A recorder that also transmits declares two more, and they are §4's split
// applied to a bus. <bus>.tx.set is what the daemon was asked to send, written
// when it acted on the request and whether or not the kernel took the frame.
// <bus>.tx.applied is each of those frames handed back by the kernel as sent.
// The raw stream is the third account and the independent one: it sees the
// daemon's frames the way it sees any other sender on the host, flagged local
// and not attributed. Each transmit stream is written from one clock — the
// requests from the daemon's, the echoes from the kernel's stamps on one
// socket — so neither is ever out of order.
//
// Cyclic transmit adds one stream per task, <bus>.cyclic.<id>, whose fields are
// held so that every segment restates what each task is doing; see
// Writer.Cyclic.
package record

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"math/bits"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/rveen/logb"
	"github.com/rveen/logb/dbc"
	"github.com/rveen/logb/viewer/index"

	"github.com/rveen/ktest/can"
)

// writer is the part of logb.Writer this package uses. It exists so that an
// index.Recorder — which is a logb.Writer that also indexes what it writes —
// can stand in for one without this package caring which it has.
type writer interface {
	AddStream(*logb.Schema) error
	AddRun(*logb.Run) error
	EndRun(id uint32)
	BeginSegment(wallTimeNs int64) error
	WriteData(s *logb.Schema, base logb.AxisVal, runID uint32, recordCount uint32, records []byte) error
	WriteMeta(key, value string) error
	WriteAttach(name string, data []byte) error
	SetHold(s *logb.Schema, base logb.AxisVal, runID uint32, present []bool, record []byte) error
	Close() error
}

// axisBits is the space every stream here leaves in front of its payload for
// the axis, matching dbc.AxisBits so that a raw record and a decoded one are
// laid out the same way.
const axisBits = dbc.AxisBits

// axisExp is the tick size of every time axis written here: nanoseconds. The
// kernel timestamps in nanoseconds and there is no reason to round them.
const axisExp = -9

// Config settles what a recording contains before the first frame arrives.
type Config struct {
	// Bus names the interface, and through it the streams: a bus of "can0"
	// gives a raw stream named can0.raw. §4 fixes this convention before the
	// first driver is written, because renaming a channel later invalidates
	// every panel and every recording that referenced it.
	Bus string

	// DB, if set, is decoded alongside the raw frames and embedded in the
	// recording as an attachment. A converted recording that says what every
	// signal means but not which database said so is a document with its
	// citation removed.
	DB *dbc.File

	// Attach is embedded in the recording as it starts, name to contents: the
	// files that governed it, such as the limits a daemon enforced, so that a
	// recording carries its own conditions rather than naming them.
	Attach map[string][]byte

	// Stamp names the timestamp source, for §11's per-stream quality record.
	Stamp can.Stamp

	Codec  logb.Codec
	Filter logb.Filter

	// PerFrame is how many records accumulate in a stream before a DATA frame
	// is written. Zero takes the default.
	PerFrame int

	// Segment is how often a new segment begins. Every schema and every held
	// value is restated at a segment boundary, which is what lets a reader
	// join a live stream, and what lets a file cut short still decode; the
	// cost of restating them is what §13 says to measure rather than assume.
	Segment time.Duration

	// Meta is written into the recording at run start: §11's provenance, which
	// the caller assembles because it knows the kernel and the adapter and
	// this package does not.
	Meta map[string]string

	// Path, if set, is where the recording will land on disk, and asks for its
	// index to be built as it is written rather than by scanning it afterwards.
	// Writer.SaveIndex then writes the sidecar, and a viewer opens the file
	// without reading it back.
	//
	// It is only a name here: this package writes to an io.Writer and never
	// opens the path. A recording that is not going to a file leaves it empty.
	Path string

	// TX declares the transmit streams, <bus>.tx.set and <bus>.tx.applied, for
	// a recorder that also sends. One that cannot transmit leaves it false, and
	// its recording does not claim streams it could never fill.
	TX bool

	// Now is the clock, injectable so that a test can write a deterministic
	// file. It supplies the recording's epoch and the timestamp of any frame
	// the kernel did not stamp.
	Now func() time.Time

	// Warn, if set, is told about anything that could not be carried across.
	Warn func(format string, a ...any)
}

const (
	defaultPerFrame = 4096
	defaultSegment  = time.Second
)

// A Writer accumulates frames and emits Logb.
//
// It is not safe for concurrent use: one bus, one goroutine, one Writer. The
// daemon's fan-out to several consumers happens below this, on the io.Writer.
type Writer struct {
	w   writer
	cfg Config

	// rec is w again when an index is being built alongside the recording,
	// and nil otherwise. Keeping both spares every call site a type switch.
	rec *index.Recorder

	// epoch is the recording's start, and every axis value here is relative to
	// it. segBase is the current segment's offset from it in nanoseconds.
	epoch    time.Time
	segStart time.Time
	segBase  int64

	raw   *stream
	drops *stream
	byKey map[uint64]*stream
	known map[uint64]*dbc.Message

	// txSet, txApplied and control are nil unless Config.TX asked for them,
	// and so is cyclic, which holds one stream per cyclic task, by dbc.Key.
	txSet     *stream
	txApplied *stream
	control   *stream
	cyclic    map[uint64]*stream
	controls  map[string]*stream // other instruments' control streams, by name

	// custom holds the streams callers lay out themselves; see NewStream.
	custom []*stream

	// lastDrops tracks the kernel's cumulative overflow counter so that a rise
	// in it can be written as the number of frames lost. haveDrops records
	// whether the kernel has ever spoken: its silence is not a zero.
	lastDrops uint32
	haveDrops bool

	stats Stats
}

// Stats is what the recording cannot say about itself.
type Stats struct {
	Frames    int64 // frames written to the raw stream
	Decoded   int64 // frames that also went to a decoded stream
	Unknown   int64 // frames whose identifier is in no loaded database
	Unstamped int64 // frames the kernel did not timestamp
	Dropped   int64 // frames the kernel admitted losing

	Requested int64 // transmit requests written to the set stream
	Refused   int64 // of those, requests whose write the kernel refused
	Applied   int64 // echoes written to the applied stream
	Unmatched int64 // of those, echoes that answered no outstanding request
	Cyclic    int64 // changes written to cyclic task streams
	Control   int64 // decisions written to the control stream
	Denied    int64 // of those, requests the control plane refused

	Samples int64 // waveform samples written, across every acquisition
}

// stream is one Logb stream and the records not yet written out.
type stream struct {
	s   *logb.Schema
	buf []byte
	n   uint32
}

func (st *stream) record() []byte {
	n := len(st.buf)
	st.buf = append(st.buf, make([]byte, st.s.RecordBytes())...)
	st.n++
	return st.buf[n:]
}

// New starts a recording on w.
func New(w io.Writer, cfg Config) (*Writer, error) {
	if cfg.Bus == "" {
		return nil, fmt.Errorf("record: a recording needs a bus name")
	}
	if cfg.PerFrame <= 0 {
		cfg.PerFrame = defaultPerFrame
	}
	if cfg.Segment <= 0 {
		cfg.Segment = defaultSegment
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Warn == nil {
		cfg.Warn = func(string, ...any) {}
	}

	var lw writer
	var rec *index.Recorder
	if cfg.Path != "" {
		r, err := index.NewRecorder(w, cfg.Path)
		if err != nil {
			return nil, err
		}
		r.Codec, r.Filter = cfg.Codec, cfg.Filter
		lw, rec = r, r
	} else {
		p, err := logb.NewWriter(w)
		if err != nil {
			return nil, err
		}
		p.Codec, p.Filter = cfg.Codec, cfg.Filter
		lw = p
	}

	now := cfg.Now()
	rw := &Writer{
		w:        lw,
		cfg:      cfg,
		rec:      rec,
		epoch:    now,
		segStart: now,
		byKey:    map[uint64]*stream{},
		known:    map[uint64]*dbc.Message{},
	}

	if err := lw.BeginSegment(now.UnixNano()); err != nil {
		return nil, err
	}
	for k, v := range cfg.Meta {
		if err := lw.WriteMeta(k, v); err != nil {
			return nil, err
		}
	}
	if err := lw.WriteMeta("timestamp.source", cfg.Stamp.String()); err != nil {
		return nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Attach)) {
		if err := lw.WriteAttach(name, cfg.Attach[name]); err != nil {
			return nil, err
		}
	}

	rw.raw = &stream{s: rawSchema(cfg)}
	if err := lw.AddStream(rw.raw.s); err != nil {
		return nil, err
	}
	rw.drops = &stream{s: dropSchema(cfg)}
	if err := lw.AddStream(rw.drops.s); err != nil {
		return nil, err
	}
	if cfg.TX {
		rw.txSet = &stream{s: txSetSchema(cfg)}
		if err := lw.AddStream(rw.txSet.s); err != nil {
			return nil, err
		}
		rw.txApplied = &stream{s: txAppliedSchema(cfg)}
		if err := lw.AddStream(rw.txApplied.s); err != nil {
			return nil, err
		}
		rw.control = &stream{s: controlSchema(cfg.Bus, "bus")}
		if err := lw.AddStream(rw.control.s); err != nil {
			return nil, err
		}
		rw.cyclic = map[uint64]*stream{}
		rw.controls = map[string]*stream{}
	}

	// The database goes in as an artefact, never as something a reader must
	// find a parser for: the bit layout is in the schemas.
	if cfg.DB != nil {
		for _, m := range cfg.DB.Messages {
			rw.known[m.Key()] = m
		}
		name := cfg.DB.Name
		if name == "" {
			name = "database.dbc"
		}
		if err := lw.WriteAttach(name, cfg.DB.Raw); err != nil {
			return nil, err
		}
		if err := lw.WriteMeta("dbc.database", name); err != nil {
			return nil, err
		}
		if err := lw.WriteMeta("dbc.sha256", cfg.DB.SHA256()); err != nil {
			return nil, err
		}
	}
	return rw, nil
}

// Frame writes one received frame.
//
// The timestamp rule of §8 is applied here and nowhere else: a frame the kernel
// did not stamp is written with this machine's clock and its stamped bit clear,
// so the record says the axis value is a userspace reading. It is never written
// as t=0, and the count is in Stats.
//
// A rise in the kernel's overflow counter is written to the drop stream before
// the frame that reported it, because that is where the loss happened. A
// counter that never speaks reports nothing, which is exactly §10.5's warning:
// the absence of an overflow signal is not evidence of no loss.
func (w *Writer) Frame(f *can.Frame) error {
	t := f.Time
	if !f.Stamped {
		t = w.cfg.Now()
		w.stats.Unstamped++
	}
	if err := w.rotate(t); err != nil {
		return err
	}
	off := w.offset(t)

	if f.HaveDrops {
		if w.haveDrops && f.Drops > w.lastDrops {
			lost := f.Drops - w.lastDrops
			w.stats.Dropped += int64(lost)
			rec := w.drops.record()
			binary.LittleEndian.PutUint64(rec[0:], uint64(off))
			binary.LittleEndian.PutUint32(rec[8:], f.Drops)
			binary.LittleEndian.PutUint32(rec[12:], lost)
			if err := w.maybeFlush(w.drops); err != nil {
				return err
			}
		}
		w.lastDrops, w.haveDrops = f.Drops, true
	}

	rec := w.raw.record()
	binary.LittleEndian.PutUint64(rec[0:], uint64(off))
	// The identifier goes in with its flag bits removed, because they are
	// already declared as fields of their own: keeping them here as well would
	// say the same thing twice and leave an extended frame reading as a
	// nine-digit number nothing on the bus ever used.
	binary.LittleEndian.PutUint32(rec[8:], f.Arbitration())
	rec[12] = byte(f.Len)
	rec[13] = frameFlags(f)
	copy(rec[14:], f.Payload())
	w.stats.Frames++
	if err := w.maybeFlush(w.raw); err != nil {
		return err
	}

	// An error frame carries error classes in its identifier, not a payload a
	// database describes, so it is raw-only by construction.
	if len(w.known) == 0 || f.Err() {
		return nil
	}
	key := dbc.Key(f.Arbitration(), f.Extended())
	st, ok := w.byKey[key]
	if !ok {
		m := w.known[key]
		if m == nil {
			w.stats.Unknown++
			return nil
		}
		var err error
		if st, err = w.addDecoded(m); err != nil {
			return err
		}
	}
	dec := st.record()
	binary.LittleEndian.PutUint64(dec[0:], uint64(off))
	// The payload goes in as it arrived. A frame shorter than the database
	// says leaves the rest of the record zero, which is what the length in the
	// raw stream is there to disambiguate.
	n := copy(dec[axisBits/8:], f.Payload())
	if n < len(f.Payload()) {
		w.cfg.Warn("message %s: a frame carried %d bytes, the database says %d",
			st.s.Name, f.Len, st.s.RecordBytes()-axisBits/8)
	}
	w.stats.Decoded++
	return w.maybeFlush(st)
}

var errNoTX = errors.New("record: a transmit record in a recording configured without TX")

// errnoUnknown is what the set stream says for a refusal that carried no errno.
const errnoUnknown = 0xFFFF

// errnoOf is what a transmit stream records as the kernel's answer: 0 for
// none, its errno if it carried one, errnoUnknown if it did not.
func errnoOf(err error) uint16 {
	if err == nil {
		return 0
	}
	var e syscall.Errno
	if errors.As(err, &e) && e < errnoUnknown {
		return uint16(e)
	}
	return errnoUnknown
}

// Requested writes one transmit request: what the daemon was asked to put on
// the bus, at t, the moment it acted on the request. sendErr is the kernel's
// answer to the write, nil if it took the frame.
//
// It is written whether or not the kernel refused, because "what did we ask
// for" is the question this stream answers. Its clock is userspace: nothing
// stamps a request but the daemon.
func (w *Writer) Requested(seq uint32, f *can.Frame, t time.Time, sendErr error) error {
	if w.txSet == nil {
		return errNoTX
	}
	if err := w.rotate(t); err != nil {
		return err
	}
	rec := w.txRecord(w.txSet, t, seq, f)
	if sendErr != nil {
		w.stats.Refused++
	}
	binary.LittleEndian.PutUint16(rec[txErrnoAt:], errnoOf(sendErr))
	w.stats.Requested++
	return w.maybeFlush(w.txSet)
}

// Applied writes a frame the transmit socket got back: the kernel's word that
// a frame this daemon sent went out. seq is the request it answers, or 0 for
// an echo no outstanding request accounts for — written all the same, since
// an echo that matches nothing is itself a finding.
//
// The echo's own kernel stamp is its axis value, under the same rule as
// Frame: an unstamped echo is written with this machine's clock and says so.
func (w *Writer) Applied(seq uint32, f *can.Frame) error {
	if w.txApplied == nil {
		return errNoTX
	}
	t := f.Time
	if !f.Stamped {
		t = w.cfg.Now()
		w.stats.Unstamped++
	}
	if err := w.rotate(t); err != nil {
		return err
	}
	w.txRecord(w.txApplied, t, seq, f)
	w.stats.Applied++
	if seq == 0 {
		w.stats.Unmatched++
	}
	return w.maybeFlush(w.txApplied)
}

// txRecord lays out what the two transmit streams share, and returns the
// record so that the set stream can add its errno.
func (w *Writer) txRecord(st *stream, t time.Time, seq uint32, f *can.Frame) []byte {
	rec := st.record()
	binary.LittleEndian.PutUint64(rec[0:], uint64(w.offset(t)))
	binary.LittleEndian.PutUint32(rec[8:], seq)
	binary.LittleEndian.PutUint32(rec[12:], f.Arbitration())
	rec[16] = byte(f.Len)
	rec[17] = frameFlags(f)
	copy(rec[18:], f.Payload())
	return rec
}

// A Stream is one a caller lays out itself — an instrument's channels, which
// this package knows by their schema and nothing more. It shares the
// recording's axis, segments and held-value restatement with every other
// stream here.
type Stream struct {
	st   *stream
	held bool
}

// Schema makes a stream schema on this package's time axis: fields come after
// the 64-bit axis field, and recordBits counts the axis too.
func Schema(name string, recordBits uint32, fields []logb.Field, meta map[string]string) *logb.Schema {
	return &logb.Schema{
		UUID:       uid("ktest/" + name),
		Name:       name,
		RecordBits: recordBits,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisExplicit,
		AxisExp:    axisExp,
		AxisUnit:   "s",
		AxisScale:  logb.TickVal(1),
		AxisField:  0,
		Fields:     append([]logb.Field{axisField()}, fields...),
		Meta:       meta,
	}
}

// NewStream declares a stream made with Schema.
func (w *Writer) NewStream(s *logb.Schema) (*Stream, error) {
	if len(s.Fields) == 0 || s.Fields[0].Name != "t" || s.Fields[0].BitWidth != axisBits || s.AxisField != 0 {
		return nil, fmt.Errorf("record: stream %q does not lead with the time axis; make it with Schema", s.Name)
	}
	if err := w.w.AddStream(s); err != nil {
		return nil, err
	}
	st := &stream{s: s}
	w.custom = append(w.custom, st)
	return &Stream{st: st, held: slices.ContainsFunc(s.Fields, func(f logb.Field) bool { return f.Hold })}, nil
}

// Write writes one record at t. The axis is written here; fill lays out the
// rest. When the stream has held fields the record is also what the stream
// restates at every segment from now on (§5.1), anchored at t.
func (w *Writer) Write(t time.Time, s *Stream, fill func(rec []byte)) error {
	if err := w.rotate(t); err != nil {
		return err
	}
	rec := s.st.record()
	binary.LittleEndian.PutUint64(rec[0:], uint64(w.offset(t)))
	fill(rec)
	if s.held {
		if err := w.hold(s.st, t, rec, nil); err != nil {
			return err
		}
	}
	return w.maybeFlush(s.st)
}

// Meta writes a key the recording learns only once it is under way, such as
// what an instrument said it was when it first answered.
func (w *Writer) Meta(key, value string) error { return w.w.WriteMeta(key, value) }

// Attach embeds a file the recording learns of once it is under way, such as a
// panel saved from the rack. A reader keeps the last attachment of a name, so
// attaching a name again makes the new file the one a recording ends with.
func (w *Writer) Attach(name string, data []byte) error { return w.w.WriteAttach(name, data) }

// CyclicEvent is what happened to a cyclic transmit task.
type CyclicEvent uint8

const (
	CyclicStart    CyclicEvent = 1 // a task was created
	CyclicUpdate   CyclicEvent = 2 // a task that existed was changed
	CyclicStop     CyclicEvent = 3 // a task was deleted on request
	CyclicExpired  CyclicEvent = 4 // a counted task sent its last frame
	CyclicEnded    CyclicEvent = 5 // the daemon stopped, and deleted the task
	CyclicDisarmed CyclicEvent = 6 // the bus was disarmed, and the task deleted with it
)

// A CyclicTask is a task as one party describes it: what was requested, or
// what the kernel reported holding.
type CyclicTask struct {
	Frame  *can.Frame
	Period time.Duration
	Count  uint32 // frames still to send before the task ends; 0 runs until deleted
}

// A Cyclic is one change to a cyclic transmit task, with both accounts of it.
type Cyclic struct {
	Seq     uint32 // the request, or 0 for an event the daemon or the kernel originated
	Event   CyclicEvent
	Err     error       // the kernel's answer, nil if it did what was asked
	Active  bool        // whether the task transmits after this event
	Set     CyclicTask  // the task as last requested
	Applied *CyclicTask // the kernel's TX_READ after the event; nil if it holds no such task
}

// Cyclic writes one change to a cyclic transmit task into the task's own
// stream, <bus>.cyclic.<id>, which is declared the first time the task is
// seen, and makes it what that stream restates at every segment from now on.
//
// The restatement is why a task has a stream to itself (§5.1): a HOLD frame
// restates one record per stream, and a consumer joining mid-run should learn
// what is transmitting now from the segment's preamble rather than by
// replaying the recording from its start.
//
// The set fields are the last request even when the kernel refused it, as a
// setpoint channel's are (§4). What is in force is the applied fields, and
// they are absent — guarded, never zero — when the kernel held no such task.
func (w *Writer) Cyclic(t time.Time, c *Cyclic) error {
	if w.cyclic == nil {
		return errNoTX
	}
	if err := w.rotate(t); err != nil {
		return err
	}
	f := c.Set.Frame
	key := dbc.Key(f.Arbitration(), f.Extended())
	st, ok := w.cyclic[key]
	if !ok {
		st = &stream{s: cyclicSchema(w.cfg, f)}
		if err := w.w.AddStream(st.s); err != nil {
			return err
		}
		w.cyclic[key] = st
	}

	rec := st.record()
	le := binary.LittleEndian
	le.PutUint64(rec[0:], uint64(w.offset(t)))
	le.PutUint32(rec[8:], c.Seq)
	le.PutUint32(rec[cycCanID:], f.Arbitration())
	rec[cycEvent] = byte(c.Event)
	rec[cycFlags] = frameFlags(f)
	le.PutUint16(rec[cycErrno:], errnoOf(c.Err))
	if c.Active {
		rec[cycState] |= 1
	}
	rec[cycLen] = byte(f.Len)
	le.PutUint64(rec[cycPeriod:], uint64(c.Set.Period))
	le.PutUint32(rec[cycCount:], c.Set.Count)
	copy(rec[cycPayload:], f.Payload())
	if a := c.Applied; a != nil {
		rec[cycState] |= 2
		rec[cycALen] = byte(a.Frame.Len)
		le.PutUint64(rec[cycAPeriod:], uint64(a.Period))
		le.PutUint32(rec[cycACount:], a.Count)
		copy(rec[cycAPayload:], a.Frame.Payload())
	}

	if err := w.hold(st, t, rec, func(f *logb.Field) bool { return !f.Guarded || c.Applied != nil }); err != nil {
		return err
	}
	w.stats.Cyclic++
	return w.maybeFlush(st)
}

// ControlRequest is what the control plane was asked, or what the daemon did
// unasked.
type ControlRequest uint8

const (
	ControlSend         ControlRequest = 1
	ControlCyclicSet    ControlRequest = 2
	ControlCyclicDelete ControlRequest = 3
	ControlArm          ControlRequest = 4
	ControlDisarm       ControlRequest = 5
	ControlLeaseTake    ControlRequest = 6
	ControlLeaseRenew   ControlRequest = 7
	ControlLeaseRelease ControlRequest = 8
	ControlLeaseExpired ControlRequest = 9  // the daemon, when a lease's term ran out
	ControlShutdown     ControlRequest = 10 // the daemon, as it stopped
	ControlChannelSet   ControlRequest = 11 // a write to an instrument's channel
	ControlStop         ControlRequest = 12 // every instrument disarmed at once: the rack's stop
)

// ControlVerdict is the control plane's answer, given before anything reaches
// the kernel.
type ControlVerdict uint8

const (
	VerdictAccepted   ControlVerdict = 0
	VerdictNotArmed   ControlVerdict = 1 // a send or cyclic task on a bus that is not armed
	VerdictNoLease    ControlVerdict = 2 // a request that needs the lease, made without it
	VerdictLeaseHeld  ControlVerdict = 3 // a lease asked for while another client held it
	VerdictBelowFloor ControlVerdict = 4 // a cyclic period below the limits' floor
	VerdictForbidden  ControlVerdict = 5 // the caller's role does not permit the request
	VerdictAboveLimit ControlVerdict = 6 // a channel write beyond the limit set for the channel
)

// Via is the listener a request arrived through.
type Via uint8

const (
	ViaDaemon Via = 0 // nobody asked: the daemon acted on its own
	ViaUnix   Via = 1 // the local socket, trusted as its file permissions are
	ViaTLS    Via = 2 // the network listener, TLS with a named token
)

// A Control is one decision of the control plane.
type Control struct {
	Seq     uint32 // the request, or 0 for what the daemon did unasked
	Request ControlRequest
	Verdict ControlVerdict
	By      string        // the authenticated name that asked; "" on the local socket and for the daemon
	Via     Via           // the listener it asked through
	Frame   *can.Frame    // the frame a send or cyclic request named, nil for others
	Period  time.Duration // the period a cyclic request asked for

	// Instrument is whose decision this is, and so which control stream it
	// goes in: "" for the bus, whose stream is <bus>.control, or an
	// instrument's name, whose stream is <name>.control. Each has its own
	// lease (§7), and so its own account of who held it.
	Instrument string

	// Kind is what sort of instrument it is — "bus", "psu", "scope" — and goes
	// into the stream's schema rather than into a record, because it never
	// changes. A reader that has to drive the instrument needs it: each kind is
	// leased and armed on a path of its own, and a rack that inferred the kind
	// from the shape of a name would be wrong the first time a third one
	// arrived. It is only read when the stream is created.
	Kind string

	Channel string  // the channel a write named, such as psu1.ch1.v.set
	Value   float64 // the value the write asked for

	// The state the decision leaves, which the stream holds.
	Armed  bool
	Holder string        // the lease holder's name, "" when nobody holds the lease
	TTL    time.Duration // the lease's term as last granted
}

// HolderMax is the longest lease holder name a recording carries, in bytes.
const HolderMax = 32

// Control writes one decision of the control plane into <bus>.control and
// makes the state it leaves — armed or not, and whose lease — what the stream
// restates at every segment from now on.
//
// It is the control plane's own account, taken before anything reaches the
// kernel: a request it refused is recorded here and nowhere else, and one it
// accepted is recorded here and then in the stream of whatever it asked for.
func (w *Writer) Control(t time.Time, c *Control) error {
	if w.control == nil {
		return errNoTX
	}
	if len(c.Holder) > HolderMax || len(c.By) > HolderMax || len(c.Channel) > HolderMax {
		return fmt.Errorf("record: a name longer than %d bytes: holder %q, by %q, channel %q",
			HolderMax, c.Holder, c.By, c.Channel)
	}
	if err := w.rotate(t); err != nil {
		return err
	}
	st := w.control
	if c.Instrument != "" && c.Instrument != w.cfg.Bus {
		if st = w.controls[c.Instrument]; st == nil {
			st = &stream{s: controlSchema(c.Instrument, c.Kind)}
			if err := w.w.AddStream(st.s); err != nil {
				return err
			}
			w.controls[c.Instrument] = st
		}
	}
	rec := st.record()
	le := binary.LittleEndian
	le.PutUint64(rec[0:], uint64(w.offset(t)))
	le.PutUint32(rec[8:], c.Seq)
	rec[ctlRequest] = byte(c.Request)
	rec[ctlVerdict] = byte(c.Verdict)
	if f := c.Frame; f != nil {
		le.PutUint32(rec[ctlCanID:], f.Arbitration())
		rec[ctlFlags] = frameFlags(f)
	}
	if c.Armed {
		rec[ctlState] |= 1
	}
	if c.Holder != "" {
		rec[ctlState] |= 2
	}
	le.PutUint64(rec[ctlPeriod:], uint64(c.Period))
	le.PutUint64(rec[ctlTTL:], uint64(c.TTL))
	copy(rec[ctlHolder:], c.Holder)
	copy(rec[ctlBy:], c.By)
	rec[ctlVia] = byte(c.Via)
	copy(rec[ctlChannel:], c.Channel)
	le.PutUint64(rec[ctlValue:], math.Float64bits(c.Value))
	if err := w.hold(st, t, rec, nil); err != nil {
		return err
	}
	w.stats.Control++
	if c.Verdict != VerdictAccepted {
		w.stats.Denied++
	}
	return w.maybeFlush(st)
}

// DeclareControl declares an instrument's control stream before its first
// decision, so that a reader learns the instrument exists — and what kind it
// is — from the start of the recording rather than only once somebody has
// asked it for something. A rack needs the kind to know which path to take a
// lease on, and a stream that appeared only with its first decision would
// leave that first request with nowhere to go.
func (w *Writer) DeclareControl(instrument, kind string) error {
	if w.controls == nil {
		return errNoTX
	}
	if instrument == "" || instrument == w.cfg.Bus || w.controls[instrument] != nil {
		return nil
	}
	st := &stream{s: controlSchema(instrument, kind)}
	if err := w.w.AddStream(st.s); err != nil {
		return err
	}
	w.controls[instrument] = st
	return nil
}

// hold makes rec what st restates at every segment from now on. Only held
// fields are present, and of those only the ones present, if given, accepts.
//
// The restatement is anchored at the change itself: its base is the change's
// own time and its axis field zero, so a reader that dates a HOLD by axis_base
// alone and one that adds the field to it agree.
func (w *Writer) hold(st *stream, t time.Time, rec []byte, present func(*logb.Field) bool) error {
	p := make([]bool, len(st.s.Fields))
	for i := range st.s.Fields {
		f := &st.s.Fields[i]
		p[i] = f.Hold && (present == nil || present(f))
	}
	h := slices.Clone(rec)
	binary.LittleEndian.PutUint64(h[0:], 0)
	return w.w.SetHold(st.s, logb.TickVal(t.Sub(w.epoch).Nanoseconds()), 0, p, h)
}

// offset is t on the current segment's axis. It is negative for a record
// stamped before the segment began, which happens whenever records from
// several clocks meet at a boundary; see axisField.
func (w *Writer) offset(t time.Time) int64 {
	return t.Sub(w.epoch).Nanoseconds() - w.segBase
}

// addDecoded declares a message's stream the first time it is seen.
func (w *Writer) addDecoded(m *dbc.Message) (*stream, error) {
	s, err := dbc.Schema(m, dbc.SchemaOptions{
		Namespace: fmt.Sprintf("ktest/%s/%d", w.cfg.Bus, w.epoch.UnixNano()),
		Database:  w.cfg.DB.Name,
		AxisExp:   axisExp,
		Warn:      w.cfg.Warn,
	})
	if err != nil {
		return nil, err
	}
	if err := w.w.AddStream(s); err != nil {
		return nil, err
	}
	st := &stream{s: s}
	w.byKey[m.Key()] = st
	return st, nil
}

// rotate begins a new segment once cfg.Segment has elapsed. Everything
// buffered is written first, because a DATA frame's records are relative to
// the segment it was written in.
func (w *Writer) rotate(t time.Time) error {
	if t.Sub(w.segStart) < w.cfg.Segment {
		return nil
	}
	if err := w.Flush(); err != nil {
		return err
	}
	w.segStart = t
	w.segBase = t.Sub(w.epoch).Nanoseconds()
	return w.w.BeginSegment(t.UnixNano())
}

func (w *Writer) maybeFlush(st *stream) error {
	if int(st.n) < w.cfg.PerFrame {
		return nil
	}
	return w.write(st)
}

func (w *Writer) write(st *stream) error {
	if st.n == 0 {
		return nil
	}
	err := w.w.WriteData(st.s, logb.TickVal(w.segBase), 0, st.n, st.buf)
	st.buf, st.n = st.buf[:0], 0
	return err
}

// Flush writes out every stream's buffered records. A live consumer sees
// nothing until this happens, so the daemon calls it on a timer as well as
// when a buffer fills.
func (w *Writer) Flush() error {
	for _, st := range []*stream{w.raw, w.drops, w.txSet, w.txApplied, w.control} {
		if st == nil {
			continue
		}
		if err := w.write(st); err != nil {
			return err
		}
	}
	for _, m := range []map[uint64]*stream{w.byKey, w.cyclic} {
		for _, st := range m {
			if err := w.write(st); err != nil {
				return err
			}
		}
	}
	for _, st := range w.controls {
		if err := w.write(st); err != nil {
			return err
		}
	}
	for _, st := range w.custom {
		if err := w.write(st); err != nil {
			return err
		}
	}
	return nil
}

// Close flushes and ends the recording.
func (w *Writer) Close() error {
	if err := w.Flush(); err != nil {
		return err
	}
	return w.w.Close()
}

// Stats reports what the recording cannot say about itself.
func (w *Writer) Stats() Stats { return w.stats }

// SaveIndex writes the index built alongside the recording, so that opening it
// costs no scan. It reports false when no index was built, which is the case
// when Config.Path was empty.
//
// It must be called after the recording has been closed and flushed to
// Config.Path: the sidecar is validated against the file's size, modification
// time and a hash of its first bytes, and one written against a half-flushed
// file is rejected at open and silently costs the scan it was meant to save.
func (w *Writer) SaveIndex() (bool, error) {
	if w.rec == nil {
		return false, nil
	}
	return true, w.rec.Save()
}

// Frame flag bits. These sit in one byte of the raw record and are also
// declared as individual boolean fields, so a viewer shows them as channels
// rather than as a number to decode by hand.
const (
	flagExtended = 1 << iota
	flagRTR
	flagErr
	flagFD
	flagBRS
	flagESI
	flagStamped
	flagLocal
)

func frameFlags(f *can.Frame) byte {
	var b byte
	for _, c := range []struct {
		on  bool
		bit byte
	}{
		{f.Extended(), flagExtended},
		{f.RTR(), flagRTR},
		{f.Err(), flagErr},
		{f.FD(), flagFD},
		{f.BRS(), flagBRS},
		{f.ESI(), flagESI},
		{f.Stamped, flagStamped},
		{f.Local, flagLocal},
	} {
		if c.on {
			b |= c.bit
		}
	}
	return b
}

func uid(name string) [16]byte { return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)) }

// rawSchema is the wire: what a logger with no database writes.
//
// One schema carries classic CAN and CAN FD alike, with a 64-byte payload and
// a length that says how much of it the frame used. Two schemas would save the
// padding on a classic frame, at the cost of making every consumer merge two
// streams to see one bus; the padding is a constant byte run that the codec
// removes, and the merge would be permanent.
//
// The payload is a fixed bytes field, not a variable one — SPEC §6.4 is
// explicit that bus payloads belong in the fixed portion, because a tail costs
// the batch its seekability.
func rawSchema(cfg Config) *logb.Schema {
	name := cfg.Bus + ".raw"
	s := &logb.Schema{
		UUID:       uid("ktest/" + name),
		Name:       name,
		RecordBits: axisBits + 32 + 8 + 8 + 8*can.MaxLen,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisExplicit,
		AxisExp:    axisExp,
		AxisUnit:   "s",
		AxisScale:  logb.TickVal(1), // the field counts nanoseconds
		AxisField:  0,
		Fields: slices.Concat(
			[]logb.Field{
				axisField(),
				{Name: "can_id", BitOffset: 64, BitWidth: 32, Type: logb.TypeUint,
					Desc: "arbitration id, 11 or 29 bits; on an error frame, the error classes instead"},
				{Name: "len", BitOffset: 96, BitWidth: 8, Type: logb.TypeUint, Unit: "B",
					Desc: "how many payload bytes the frame carried"},
			},
			flagFields(104, "extended", "rtr", "error", "fd", "brs", "esi", "stamped", "local"),
			[]logb.Field{payloadField(112,
				"the wire bytes, exactly as the bus produced them; only the first len are the frame's")},
		),
		Meta: map[string]string{"bus": cfg.Bus},
	}
	if cfg.DB != nil && cfg.DB.Name != "" {
		s.Fields[len(s.Fields)-1].Meta["payload.schema"] = cfg.DB.Name
	}
	return s
}

// dropSchema records what the kernel admitted losing.
//
// It is an event stream, so its axis is explicit: drops are sporadic and an
// implicit axis would claim they were periodic. A recording with no records in
// this stream is not a recording with no losses — see §10.5, and the lost tail
// that reported zero everywhere the kernel was willing to look.
func dropSchema(cfg Config) *logb.Schema {
	name := cfg.Bus + ".drops"
	return &logb.Schema{
		UUID:       uid("ktest/" + name),
		Name:       name,
		RecordBits: axisBits + 32 + 32,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisExplicit,
		AxisExp:    axisExp,
		AxisUnit:   "s",
		AxisScale:  logb.TickVal(1),
		AxisField:  0,
		Fields: []logb.Field{
			axisField(),
			{Name: "total", BitOffset: 64, BitWidth: 32, Type: logb.TypeUint,
				Desc: "the kernel's cumulative SO_RXQ_OVFL counter"},
			{Name: "lost", BitOffset: 96, BitWidth: 32, Type: logb.TypeUint,
				Desc: "frames lost since the previous record in this stream"},
		},
		Meta: map[string]string{
			"bus":    cfg.Bus,
			"source": "SO_RXQ_OVFL",
		},
	}
}

// Transmit record layout, in bytes: t 0–7, seq 8–11, can_id 12–15, len 16,
// flags 17, payload 18–81, and on the set stream alone, errno 82–83.
const (
	txErrnoAt   = 18 + can.MaxLen
	txBits      = 8 * txErrnoAt
	txSetBits   = txBits + 16
	txFlagsAt   = 8 * 17
	txPayloadAt = 8 * 18
)

// txSetSchema is §4's set for a bus: every frame the daemon was asked to send.
func txSetSchema(cfg Config) *logb.Schema {
	name := cfg.Bus + ".tx.set"
	return &logb.Schema{
		UUID:       uid("ktest/" + name),
		Name:       name,
		RecordBits: txSetBits,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisExplicit,
		AxisExp:    axisExp,
		AxisUnit:   "s",
		AxisScale:  logb.TickVal(1),
		AxisField:  0,
		Fields: append(txFields("extended", "fd", "brs"), logb.Field{
			Name: "errno", BitOffset: 8 * txErrnoAt, BitWidth: 16, Type: logb.TypeUint,
			Desc: "0 if the kernel took the frame; otherwise the errno its write returned " +
				"(65535 for a refusal that carried none), and the frame never reached the bus",
		}),
		Meta: map[string]string{
			"bus":   cfg.Bus,
			"tx":    "set",
			"clock": "userspace: the daemon's clock when it acted on the request",
		},
	}
}

// txAppliedSchema is §4's applied for a bus: each sent frame, handed back.
func txAppliedSchema(cfg Config) *logb.Schema {
	name := cfg.Bus + ".tx.applied"
	return &logb.Schema{
		UUID:       uid("ktest/" + name),
		Name:       name,
		RecordBits: txBits,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisExplicit,
		AxisExp:    axisExp,
		AxisUnit:   "s",
		AxisScale:  logb.TickVal(1),
		AxisField:  0,
		Fields:     txFields("extended", "rtr", "error", "fd", "brs", "esi", "stamped", "local"),
		Meta: map[string]string{
			"bus":    cfg.Bus,
			"tx":     "applied",
			"source": "CAN_RAW_RECV_OWN_MSGS: frames the kernel handed back to the socket that sent them (MSG_CONFIRM)",
		},
	}
}

// txFields lays out what the two transmit streams share: the axis, the request
// number, and the frame in the raw stream's own terms, so that one frame reads
// the same in all three accounts of it.
func txFields(flags ...string) []logb.Field {
	return slices.Concat(
		[]logb.Field{
			axisField(),
			{Name: "seq", BitOffset: 64, BitWidth: 32, Type: logb.TypeUint,
				Desc: "the request number the control plane answered with; 0 on an echo that answers no request"},
			{Name: "can_id", BitOffset: 96, BitWidth: 32, Type: logb.TypeUint,
				Desc: "arbitration id, 11 or 29 bits"},
			{Name: "len", BitOffset: 128, BitWidth: 8, Type: logb.TypeUint, Unit: "B",
				Desc: "how many payload bytes the frame carried"},
		},
		flagFields(txFlagsAt, flags...),
		[]logb.Field{payloadField(txPayloadAt, "the frame's bytes; only the first len are the frame's")},
	)
}

// axisField is the explicit time axis every stream here leads with.
//
// It is signed because an offset can be negative. A daemon that transmits
// merges three clocks — the kernel's stamps on two sockets and its own — and a
// record stamped just before the segment it lands in sits behind that
// segment's axis_base. dbc.Schema declares its decoded streams' axis the same
// way.
func axisField() logb.Field {
	return logb.Field{Name: "t", BitOffset: 0, BitWidth: axisBits, Type: logb.TypeSint, Unit: "ns",
		Desc: "axis: nanoseconds from the DATA frame's axis_base, negative for a record stamped before it"}
}

func payloadField(at uint32, desc string) logb.Field {
	return logb.Field{Name: "payload", BitOffset: at, BitWidth: 8 * can.MaxLen, Type: logb.TypeBytes,
		Desc: desc, Meta: map[string]string{"payload.encoding": "can.raw"}}
}

// A flagBit names one bit that frameFlags packs.
type flagBit struct {
	name string
	mask byte
	desc string
}

// flagDecl is every flag bit, so that each stream carrying a flag byte
// declares the same bit for the same thing.
var flagDecl = []flagBit{
	{"extended", flagExtended, ""},
	{"rtr", flagRTR, ""},
	{"error", flagErr, "an error frame: can_id holds error classes, not an identifier"},
	{"fd", flagFD, ""},
	{"brs", flagBRS, "bit rate switch"},
	{"esi", flagESI, "error state indicator"},
	{"stamped", flagStamped, "the axis value is a kernel timestamp; clear means it was read in userspace"},
	{"local", flagLocal, "sent from this host (MSG_DONTROUTE), by any process: where the frame came from, not who sent it"},
}

// flagFields declares the named bits of a flag byte that starts at bit at.
func flagFields(at uint32, names ...string) []logb.Field {
	var fs []logb.Field
	for _, n := range names {
		i := slices.IndexFunc(flagDecl, func(d flagBit) bool { return d.name == n })
		if i < 0 {
			panic("record: no flag bit " + n)
		}
		d := flagDecl[i]
		fs = append(fs, logb.Field{Name: n, BitOffset: at + uint32(bits.TrailingZeros8(d.mask)),
			BitWidth: 1, Type: logb.TypeBool, Desc: d.desc})
	}
	return fs
}

// Cyclic record layout, in bytes: t 0–7, seq 8–11, can_id 12–15, event 16,
// flags 17, errno 18–19, state 20 (active bit 0, applied bit 1), len 21,
// applied_len 22, period 24–31, count 32–35, applied_period 40–47,
// applied_count 48–51, payload 56–119, applied_payload 120–183.
const (
	cycCanID    = 12
	cycEvent    = 16
	cycFlags    = 17
	cycErrno    = 18
	cycState    = 20
	cycLen      = 21
	cycALen     = 22
	cycPeriod   = 24
	cycCount    = 32
	cycAPeriod  = 40
	cycACount   = 48
	cycPayload  = 56
	cycAPayload = cycPayload + can.MaxLen
	cycBytes    = cycAPayload + can.MaxLen
)

// TaskName is a cyclic task's name: its identifier as candump writes it, three
// hex digits for a standard frame and eight for an extended one. It ends the
// task's stream name, and ktestd names the task in its URL the same way.
func TaskName(id uint32, extended bool) string {
	if extended {
		return fmt.Sprintf("%08X", id)
	}
	return fmt.Sprintf("%03X", id)
}

// Control record layout, in bytes: t 0–7, seq 8–11, can_id 12–15, request 16,
// verdict 17, flags 18, state 19 (armed bit 0, leased bit 1), period 20–27,
// ttl 28–35, holder 36–67, by 68–99, via 100, channel 101–132, value 133–140.
const (
	ctlCanID   = 12
	ctlRequest = 16
	ctlVerdict = 17
	ctlFlags   = 18
	ctlState   = 19
	ctlPeriod  = 20
	ctlTTL     = 28
	ctlHolder  = 36
	ctlBy      = ctlHolder + HolderMax
	ctlVia     = ctlBy + HolderMax
	ctlChannel = ctlVia + 1
	ctlValue   = ctlChannel + HolderMax
	ctlBytes   = ctlValue + 8
)

// controlSchema is the control plane's stream for one instrument, the bus
// being one.
func controlSchema(instrument, kind string) *logb.Schema {
	name := instrument + ".control"
	if kind == "" {
		kind = "instrument"
	}
	return &logb.Schema{
		UUID:       uid("ktest/" + name),
		Name:       name,
		RecordBits: 8 * ctlBytes,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisExplicit,
		AxisExp:    axisExp,
		AxisUnit:   "s",
		AxisScale:  logb.TickVal(1),
		AxisField:  0,
		Fields: slices.Concat(
			[]logb.Field{
				axisField(),
				numField("seq", 8, 32, "", "the request number the control plane answered with; 0 for what the daemon did unasked"),
				numField("can_id", ctlCanID, 32, "", "the identifier a send or cyclic request named; 0 for other requests"),
				numField("request", ctlRequest, 8, "", "1 send, 2 cyclic set, 3 cyclic delete, 4 arm, 5 disarm, "+
					"6 lease take, 7 lease renew, 8 lease release, 9 lease expired, 10 shutdown, 11 channel set, "+
					"12 stop: every instrument disarmed at once, under one seq"),
				numField("verdict", ctlVerdict, 8, "", "0 accepted; refused: 1 not armed, 2 without the lease, "+
					"3 the lease is held by another, 4 a cyclic period below the floor, 5 the caller's role does not permit it, "+
					"6 a channel write above the channel's limit"),
			},
			flagFields(8*ctlFlags, "extended", "fd", "brs"),
			held(
				logb.Field{Name: "armed", BitOffset: 8 * ctlState, BitWidth: 1, Type: logb.TypeBool,
					Desc: "the bus is armed: sends and cyclic tasks may reach it"},
				logb.Field{Name: "leased", BitOffset: 8*ctlState + 1, BitWidth: 1, Type: logb.TypeBool,
					Desc: "a client holds the bus's lease"},
			),
			[]logb.Field{
				numField("period", ctlPeriod, 64, "ns", "the period a cyclic request asked for; 0 for other requests"),
			},
			held(
				numField("ttl", ctlTTL, 64, "ns", "the lease's term as last granted"),
				logb.Field{Name: "holder", BitOffset: 8 * ctlHolder, BitWidth: 8 * HolderMax, Type: logb.TypeString,
					Desc: "the lease holder's name: as it gave it on the local socket, as it authenticated over TLS; " +
						"empty when nobody holds the lease"},
			),
			[]logb.Field{
				{Name: "by", BitOffset: 8 * ctlBy, BitWidth: 8 * HolderMax, Type: logb.TypeString,
					Desc: "the authenticated name that made the request; empty on the local socket and for what the daemon did unasked"},
				numField("via", ctlVia, 8, "", "0 the daemon, unasked; 1 the local socket; 2 the network listener, TLS with a named token"),
				{Name: "channel", BitOffset: 8 * ctlChannel, BitWidth: 8 * HolderMax, Type: logb.TypeString,
					Desc: "the channel a write named; empty for other requests"},
				{Name: "value", BitOffset: 8 * ctlValue, BitWidth: 64, Type: logb.TypeFloat,
					Desc: "the value a write asked for, whatever the verdict; 0 for other requests"},
			},
		),
		Meta: map[string]string{
			"instrument": instrument,
			"kind":       kind,
			"source":     "the ktestd control plane, before anything reaches the kernel or the instrument",
			"clock":      "userspace: the daemon's clock when it decided",
		},
	}
}

// held marks fields as held: written on change, the last value standing.
func held(fs ...logb.Field) []logb.Field {
	for i := range fs {
		fs[i].Hold = true
	}
	return fs
}

func numField(name string, byteAt, bits uint32, unit, desc string) logb.Field {
	return logb.Field{Name: name, BitOffset: 8 * byteAt, BitWidth: bits, Type: logb.TypeUint, Unit: unit, Desc: desc}
}

// cyclicSchema is one cyclic task's stream.
func cyclicSchema(cfg Config, f *can.Frame) *logb.Schema {
	task := TaskName(f.Arbitration(), f.Extended())
	name := cfg.Bus + ".cyclic." + task
	payload := payloadField(8*cycPayload, "the frame's bytes as requested; only the first len are the frame's")
	applied := payloadField(8*cycAPayload, "the frame's bytes as the kernel holds them; only the first applied_len are the frame's")
	applied.Name = "applied_payload"

	fields := slices.Concat(
		[]logb.Field{
			axisField(),
			numField("seq", 8, 32, "", "the request number the control plane answered with; 0 for an event the daemon or the kernel originated"),
			numField("can_id", cycCanID, 32, "", "arbitration id, 11 or 29 bits"),
			numField("event", cycEvent, 8, "", "1 start, 2 update, 3 stop, 4 expired (a counted task sent its last frame), 5 ended (the daemon stopped), 6 disarmed (the bus was disarmed: by request, a released lease or a lapsed one)"),
		},
		held(flagFields(8*cycFlags, "extended", "fd", "brs")...),
		[]logb.Field{
			numField("errno", cycErrno, 16, "", "0 if the kernel did what was asked; otherwise the errno it returned (65535 for a refusal that carried none)"),
		},
		held(
			logb.Field{Name: "active", BitOffset: 8 * cycState, BitWidth: 1, Type: logb.TypeBool,
				Desc: "the task transmits after this event"},
			logb.Field{Name: "applied", BitOffset: 8*cycState + 1, BitWidth: 1, Type: logb.TypeBool,
				Desc: "the kernel reported holding the task (TX_READ), and the applied_ fields are its account"},
			numField("len", cycLen, 8, "B", "payload length as requested"),
			numField("applied_len", cycALen, 8, "B", "payload length the kernel holds"),
			numField("period", cycPeriod, 64, "ns", "interval between frames as requested"),
			numField("count", cycCount, 32, "", "frames requested, 0 for until stopped"),
			numField("applied_period", cycAPeriod, 64, "ns", "the interval the kernel holds"),
			numField("applied_count", cycACount, 32, "", "frames the kernel has still to send; 0 for a task that runs until stopped, or one that is spent"),
			payload,
			applied,
		),
	)
	guard := uint16(slices.IndexFunc(fields, func(f logb.Field) bool { return f.Name == "applied" }))
	for i := range fields {
		if strings.HasPrefix(fields[i].Name, "applied_") {
			fields[i].Guarded, fields[i].GuardField, fields[i].GuardValue = true, guard, 1
		}
	}

	return &logb.Schema{
		UUID:       uid("ktest/" + name),
		Name:       name,
		RecordBits: 8 * cycBytes,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisExplicit,
		AxisExp:    axisExp,
		AxisUnit:   "s",
		AxisScale:  logb.TickVal(1),
		AxisField:  0,
		Fields:     fields,
		Meta: map[string]string{
			"bus":    cfg.Bus,
			"tx":     "cyclic",
			"task":   task,
			"source": "CAN_BCM",
			"clock":  "userspace: the daemon's clock when it acted; the frames themselves are in " + cfg.Bus + ".raw",
		},
	}
}
