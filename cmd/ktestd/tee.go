//go:build linux

package main

import (
	"time"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/can"
	"github.com/trukeio/ktest/record"
)

// tee writes every record three times: to the recording, to the rack's
// stream, and to the subscription stream.
//
// The rack's stream is the same records, uncompressed, for a browser to decode
// as they arrive (§10.3) without carrying a zstd decoder. It is a second writer
// rather than a transcoding of the first one's bytes, so that each is a valid
// Logb stream on its own, and it is where §10.4's decimation will go: the disk
// keeps full rate, the browser gets what it can draw. Only the loop touches
// either writer, as it would one.
//
// The subscription stream, /stream/sub, is the third (§4.4): the same records
// again, uncompressed, each in a DATA frame of its own written the moment the
// record is, for a peer that reacts to them. It carries no waveforms; a
// waveform is the rack's to draw and the file's to keep, and a peer judges
// the DUT by its monitors.
type tee struct {
	file, rack, sub *record.Writer
}

func (t *tee) Frame(f *can.Frame) error {
	return both(t.file.Frame(f), t.rack.Frame(f), t.sub.Frame(f))
}

func (t *tee) Requested(seq uint32, f *can.Frame, at time.Time, sendErr error) error {
	return both(t.file.Requested(seq, f, at, sendErr), t.rack.Requested(seq, f, at, sendErr),
		t.sub.Requested(seq, f, at, sendErr))
}

func (t *tee) Applied(seq uint32, f *can.Frame) error {
	return both(t.file.Applied(seq, f), t.rack.Applied(seq, f), t.sub.Applied(seq, f))
}

func (t *tee) Cyclic(at time.Time, c *record.Cyclic) error {
	return both(t.file.Cyclic(at, c), t.rack.Cyclic(at, c), t.sub.Cyclic(at, c))
}

func (t *tee) Control(at time.Time, c *record.Control) error {
	return both(t.file.Control(at, c), t.rack.Control(at, c), t.sub.Control(at, c))
}

// A teeStream is one stream the daemon lays out itself, in both writers.
type teeStream struct {
	file, rack, sub *record.Stream
}

// NewStream declares the stream mk makes in both writers, each with its own
// schema, since a writer may keep what it is given.
func (t *tee) NewStream(mk func() *logb.Schema) (*teeStream, error) {
	f, err := t.file.NewStream(mk())
	if err != nil {
		return nil, err
	}
	r, err := t.rack.NewStream(mk())
	if err != nil {
		return nil, err
	}
	sb, err := t.sub.NewStream(mk())
	if err != nil {
		return nil, err
	}
	return &teeStream{f, r, sb}, nil
}

func (t *tee) Write(at time.Time, s *teeStream, fill func(rec []byte)) error {
	return both(t.file.Write(at, s.file, fill), t.rack.Write(at, s.rack, fill), t.sub.Write(at, s.sub, fill))
}

// A teeWave is one waveform stream, in whichever writers carry it. Either half
// may be absent, and that is the point: §10.4 says the disk gets full rate and
// the browser gets the envelope, so a channel's samples exist in the file alone
// while its envelope exists in both.
type teeWave struct {
	file, rack *record.Waveform
}

// NewWaveform declares the stream mk makes in the writers asked for, each with
// its own schema, since a writer may keep what it is given.
func (t *tee) NewWaveform(mk func() *logb.Schema, onFile, onRack bool) (*teeWave, error) {
	w := &teeWave{}
	if onFile {
		f, err := t.file.NewWaveform(mk())
		if err != nil {
			return nil, err
		}
		w.file = f
	}
	if onRack {
		r, err := t.rack.NewWaveform(mk())
		if err != nil {
			return nil, err
		}
		w.rack = r
	}
	return w, nil
}

// BeginAcq, WriteSamples and EndAcq carry an acquisition into both writers.
//
// toRack is false when the rack's stream is dropping this acquisition (§10.5).
// The recording is unaffected: a waveform may be dropped on the way to a
// browser that cannot keep up, and the drop is then written into the scope's
// acquisition stream, which both writers carry — so the gap says it is a gap in
// the place a reader of either will find it.
func (t *tee) BeginAcq(at time.Time, a *record.Acq, toRack bool) error {
	err := t.file.BeginAcq(at, a)
	if toRack {
		err = both(err, t.rack.BeginAcq(at, a))
	}
	return err
}

func (t *tee) WriteSamples(at time.Time, w *teeWave, runID uint32,
	step, first time.Duration, count uint32, records []byte, toRack bool) error {

	var err error
	if w.file != nil {
		err = t.file.WriteSamples(at, w.file, runID, step, first, count, records)
	}
	if w.rack != nil && toRack {
		err = both(err, t.rack.WriteSamples(at, w.rack, runID, step, first, count, records))
	}
	return err
}

func (t *tee) EndAcq(runID uint32, toRack bool) {
	t.file.EndAcq(runID)
	if toRack {
		t.rack.EndAcq(runID)
	}
}

func (t *tee) DeclareControl(instrument, kind string) error {
	return both(t.file.DeclareControl(instrument, kind), t.rack.DeclareControl(instrument, kind),
		t.sub.DeclareControl(instrument, kind))
}

func (t *tee) Meta(key, value string) error {
	return both(t.file.Meta(key, value), t.rack.Meta(key, value), t.sub.Meta(key, value))
}

func (t *tee) Attach(name string, data []byte) error {
	return both(t.file.Attach(name, data), t.rack.Attach(name, data), t.sub.Attach(name, data))
}

func (t *tee) Flush() error { return both(t.file.Flush(), t.rack.Flush(), t.sub.Flush()) }
func (t *tee) Close() error { return both(t.file.Close(), t.rack.Close(), t.sub.Close()) }

// Stats and SaveIndex are the recording's: the rack's stream is a view of it,
// and has no file to index.
func (t *tee) Stats() record.Stats      { return t.file.Stats() }
func (t *tee) SaveIndex() (bool, error) { return t.file.SaveIndex() }

// both is the first error of several writes, each of which has been made.
func both(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
