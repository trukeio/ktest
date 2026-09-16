//go:build linux

package record

import (
	"fmt"
	"time"

	"github.com/rveen/logb"
)

// This file is the waveform half of the package: streams whose records are
// samples on a uniform axis rather than events on a wall clock.
//
// A waveform is the one shape here that §5.4 settles entirely. One record a
// sample, axis_mode implicit uniform so the axis costs no bytes, one run_id an
// acquisition, and the settings it was taken under in the RUN frame's
// parameters. What this file adds to that is the two rules a writer has to obey
// for it to stay true:
//
//   - A change of sample interval opens a new segment (§5.2). axis_step lives
//     in the SCHEMA frame and a schema is only restated at a segment boundary,
//     so a writer that changed it mid-segment would leave every record of that
//     frame decoding to a wrong axis position and say nothing. logb refuses
//     such a write; WriteAcquisition opens the segment instead, which is the
//     cheap and complete answer §5.2 chose.
//   - A run ends when its acquisition is written. A run stays in every later
//     segment's preamble until it is ended, which is right for a sweep and
//     ruinous for a scope: an acquisition a second for an hour would restate
//     3600 RUN frames into every segment, for a reader that needs only the ones
//     that segment carries.

// A Waveform is a stream of samples on a uniform time axis: an acquisition at a
// time, each written whole.
//
// It is not a Stream. A Stream accumulates records and hands them over in
// batches on the recording's own axis; a Waveform has one batch per acquisition,
// its own axis base and step, and its own run, so it goes to the file directly
// rather than through the buffer.
type Waveform struct {
	st *stream
}

// UniformSchema makes a stream schema whose axis is time, implicit and uniform:
// the axis is base + i x step and costs no bytes in the record (§5.4). step is
// the interval the stream starts at; WriteAcquisition changes it, opening a
// segment when it does.
//
// recordBits counts only the fields, since there is no axis field to leave room
// for. That is the difference from Schema, and it is the point of this shape.
func UniformSchema(name string, recordBits uint32, step time.Duration, fields []logb.Field, meta map[string]string) *logb.Schema {
	return &logb.Schema{
		UUID:       uid("ktest/" + name),
		Name:       name,
		RecordBits: recordBits,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisImplicit,
		AxisExp:    axisExp,
		AxisUnit:   "s",
		AxisStep:   logb.TickVal(step.Nanoseconds()),
		Fields:     fields,
		Meta:       meta,
	}
}

// NewWaveform declares a stream made with UniformSchema.
func (w *Writer) NewWaveform(s *logb.Schema) (*Waveform, error) {
	if s.AxisMode != logb.AxisImplicit || s.AxisKind != logb.AxisTime {
		return nil, fmt.Errorf("record: stream %q is not a uniform time axis; make it with UniformSchema", s.Name)
	}
	if err := w.w.AddStream(s); err != nil {
		return nil, err
	}
	return &Waveform{st: &stream{s: s}}, nil
}

// An Acq is one acquisition: one trigger, whatever it left on however many
// channels.
//
// It is the unit a run is, which is why it is declared once and not once a
// stream. A two-channel trigger writes four streams — each channel's samples
// and each channel's envelope — and they are one acquisition, under one run,
// with one RUN frame saying what the settings were.
type Acq struct {
	// RunID identifies the acquisition and Index is its ordinal, both as §5.4
	// asks: one run an acquisition, and the axis restarts with it.
	RunID uint32
	Index uint32

	// Params are the settings the acquisition was taken under, for the RUN
	// frame. They are what makes the samples mean something later: the volts a
	// division and the offset the conversion used, the timebase and the rate the
	// axis was computed from, and which of the guide's disputed readings was
	// taken.
	Params map[string]string
}

// BeginAcq opens an acquisition: it declares the run and the settings it was
// taken under.
//
// t is where the acquisition is placed on the recording's timeline. Until the
// scope's clock and the daemon's are tied (§12, slice 3), that is a userspace
// stamp taken at readout and not the instant the scope triggered; the caller
// records the difference between those two things rather than hiding it, and
// the scope's own frame time goes in Params beside it, never in place of it
// (§11).
func (w *Writer) BeginAcq(t time.Time, a *Acq) error {
	if err := w.rotate(t); err != nil {
		return err
	}
	return w.w.AddRun(&logb.Run{ID: a.RunID, Index: a.Index, Params: a.Params})
}

// WriteSamples writes one stream's share of an acquisition: its samples, whole,
// under the acquisition's run.
//
// step is the interval between samples and first where sample 0 lies relative
// to t, normally negative, since half the screen is before the trigger.
func (w *Writer) WriteSamples(t time.Time, wf *Waveform, runID uint32,
	step, first time.Duration, count uint32, records []byte) error {

	// §5.2: axis_step is schema-scoped, so a change of it needs a segment of its
	// own. Everything buffered goes out first, because its records are relative
	// to the segment they were written in; the run is restated by the new
	// segment's preamble, as every run in force is.
	ticks := logb.TickVal(step.Nanoseconds())
	if wf.st.s.AxisStep != ticks {
		if err := w.Flush(); err != nil {
			return err
		}
		wf.st.s.AxisStep = ticks
		w.segStart = t
		w.segBase = t.Sub(w.epoch).Nanoseconds()
		if err := w.w.BeginSegment(t.UnixNano()); err != nil {
			return err
		}
	}
	// The axis base is where sample 0 lies, counted from the recording's epoch,
	// because that is what a DATA frame's axis_base means: the explicit path
	// writes the segment's own base there and the offset from it in each
	// record, and the two sum to the same thing. Writing a segment-relative
	// base here would put every acquisition wherever the last segment happened
	// to begin, which is a wrong answer with nothing to say so.
	base := logb.TickVal(t.Sub(w.epoch).Nanoseconds() + first.Nanoseconds())
	if err := w.w.WriteData(wf.st.s, base, runID, count, records); err != nil {
		return err
	}
	w.stats.Samples += int64(count)
	return nil
}

// EndAcq says the acquisition is complete, so its run need not be restated in
// any later segment. See the note at the head of this file for why that is not
// an optimisation.
func (w *Writer) EndAcq(runID uint32) { w.w.EndRun(runID) }
