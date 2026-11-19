package monitor

import (
	"container/heap"
	"fmt"
	"slices"
	"time"
)

// # Online
//
// The DUT rack runs the evaluator live (handoff §4.3). Two things stand
// between the recorder and the evaluator that a replay does not need.
//
// Order. The evaluator takes records in time order, and refuses an earlier
// one. Live, records reach the recorder in the order their sources delivered
// them, and the sources keep different clocks: the kernel stamps a CAN frame
// as it arrives, a supply's driver stamps a reading when the answer comes
// back. So the online evaluator holds every record for a short window, and
// releases them in time order once the daemon's clock has passed their time
// by the window. A record that arrives after its time has been released past
// is late, and is a gap.
//
// Isolation. The evaluator may never hold up the event loop. The loop hands
// records to a Tap, which never blocks: when the queue behind it is full the
// record is lost, counted, and reported as a gap the moment there is room.
// A gap makes every monitor unknown, from the watermark at which it was
// noticed, and the evaluator starts again from nothing: a monitor is known
// again once its inputs are, from records after the gap. Nothing is dropped
// in silence, and nothing is evaluated over a history with a hole in it.
//
// Time. The evaluator reads no clock. The daemon's clock reaches it as the
// time at which the loop handed each item over, and as a tick the loop sends
// every millisecond or so; that is what lets a loop that has stalled stop the
// watermark, rather than a watermark that runs on while records wait.

// ItemKind is what an Item carries.
type ItemKind uint8

const (
	ItemRecord ItemKind = iota // a record of an input stream
	ItemMode                   // the DUT entered a mode
	ItemTick                   // only the daemon's clock
	itemGap                    // records were lost on the way in
)

// Item is one thing the loop hands the online evaluator, in the order the
// loop saw it. At is the loop's clock as it handed the item over; T is a
// record's or a mode's own time. Both are Unix nanoseconds.
type Item struct {
	Kind   ItemKind
	At     int64
	T      int64
	Stream string
	Rec    []byte
	Mode   string
	gap    *Gap
}

// Gap is records the evaluator did not see, or saw too late to use.
type Gap struct {
	T      int64  // from when every monitor is unknown: the watermark when the gap was noticed
	From   int64  // the first record not used, by its own time
	To     int64  // the last
	Lost   int    // how many
	Reason string // "overflow": the queue was full; "late": a record behind the watermark

	// The mode the DUT was in when the gap was noticed, which a lost mode
	// item would otherwise take with it.
	mode     string
	haveMode bool
}

// Output is what one step of the online evaluator produced, for the loop to
// record: transitions in time order, then the gap that caused some of them,
// then the watermark they bring the monitors up to.
type Output struct {
	Transitions []Transition
	Gap         *Gap
	// Watermark, when not 0, is the time up to which every transition has
	// now been emitted, Unix nanoseconds.
	Watermark int64
}

// Online is the evaluator as the daemon runs it. It is not safe for
// concurrent use: Run gives it a goroutine.
type Online struct {
	c *Contract
	// Window is how long a record is held for earlier ones to arrive.
	Window time.Duration
	// Every is how often the watermark is reported, in the daemon's time.
	Every time.Duration

	e        *Evaluator
	pending  pendingHeap
	seq      uint64
	edge     int64 // the time released up to; 0 before the first
	reported int64 // the last watermark reported
	mode     string
	haveMode bool
}

// NewOnline makes the online evaluator for c.
func NewOnline(c *Contract, window, every time.Duration) (*Online, error) {
	e, err := NewEvaluator(c)
	if err != nil {
		return nil, err
	}
	return &Online{c: c, Window: window, Every: every, e: e}, nil
}

// Streams are the streams the evaluator reads, which the tap passes on.
func (o *Online) Streams() []string { return o.e.Streams() }

// State is a monitor's current state.
func (o *Online) State(name string) State { return o.e.State(name) }

// Run feeds the evaluator from in and hands what it produces to out, until
// in is closed. A slow reader of out stalls the evaluator, which fills the
// tap's queue, which is then a gap: never a block on the loop.
func (o *Online) Run(in <-chan Item, out chan<- Output) {
	for it := range in {
		if res := o.Step(it); len(res.Transitions) > 0 || res.Gap != nil || res.Watermark != 0 {
			out <- res
		}
	}
}

// Step takes one item. It is the whole of the online evaluator's logic, and
// reads no clock, so a test can drive it item by item.
func (o *Online) Step(it Item) Output {
	var res Output
	switch it.Kind {
	case ItemRecord, ItemMode:
		if o.started() && it.T < o.e.Now() {
			o.reset(&res, &Gap{From: it.T, To: it.T, Lost: 1,
				Reason: fmt.Sprintf("late: %s at %v behind the watermark", it.Stream, time.Duration(o.e.Now()-it.T))})
			if it.Kind == ItemMode {
				// A mode is never lost: it is the state the DUT is in, and
				// the fresh evaluator takes it from now.
				o.mode, o.haveMode = it.Mode, true
				o.feed(&res, Item{Kind: ItemMode, T: o.e.Now(), Mode: it.Mode})
			}
			break
		}
		o.seq++
		heap.Push(&o.pending, pendingItem{it, o.seq})
	case itemGap:
		if it.gap.haveMode {
			o.mode, o.haveMode = it.gap.mode, true
		}
		o.reset(&res, it.gap)
	}
	o.release(&res, it.At-int64(o.Window))
	return res
}

func (o *Online) started() bool { return o.edge != 0 }

// release feeds every held item up to edge, in time order, then advances the
// evaluator to edge: every deadline up to it is evaluated, and the watermark
// stands there.
func (o *Online) release(res *Output, edge int64) {
	if edge <= o.edge {
		return
	}
	for o.pending.Len() > 0 && o.pending[0].T <= edge {
		p := heap.Pop(&o.pending).(pendingItem)
		o.feed(res, p.Item)
	}
	tr, err := o.e.Advance(edge)
	res.Transitions = append(res.Transitions, tr...)
	if err != nil {
		// Advance refuses only a time before the evaluator's own, which a
		// clock stepped back could give. The watermark stays where it was.
		return
	}
	o.edge = edge
	if o.reported == 0 || edge-o.reported >= int64(o.Every) {
		res.Watermark, o.reported = edge, edge
	}
}

func (o *Online) feed(res *Output, it Item) {
	var tr []Transition
	var err error
	if it.Kind == ItemMode {
		o.mode, o.haveMode = it.Mode, true
		tr, err = o.e.Mode(it.T, it.Mode)
	} else {
		tr, err = o.e.Record(it.Stream, it.T, it.Rec)
	}
	res.Transitions = append(res.Transitions, tr...)
	if err != nil {
		o.reset(res, &Gap{From: it.T, To: it.T, Lost: 1, Reason: fmt.Sprintf("%s: %v", it.Stream, err)})
	}
}

// reset makes every monitor unknown from the watermark and starts the
// evaluator again from nothing, in the mode the DUT is in.
func (o *Online) reset(res *Output, g *Gap) {
	t := max(o.e.Now(), o.edge)
	g.T = t
	for _, name := range o.c.Order {
		if s := o.e.State(name); s != Unknown {
			res.Transitions = append(res.Transitions,
				Transition{T: t, Monitor: name, From: s, To: Unknown, Cause: "gap: " + g.Reason})
		}
	}
	res.Gap = g
	e, err := NewEvaluator(o.c)
	if err != nil {
		panic(err) // the contract made one already
	}
	o.e = e
	if t != 0 {
		if _, err := o.e.Advance(t); err != nil {
			panic(err) // a fresh evaluator takes any time
		}
	}
	if o.haveMode {
		tr, _ := o.e.Mode(t, o.mode)
		res.Transitions = append(res.Transitions, tr...)
	}
}

type pendingItem struct {
	Item
	seq uint64
}

// pendingHeap orders held items by time, and items of one time in the order
// they arrived.
type pendingHeap []pendingItem

func (h pendingHeap) Len() int { return len(h) }
func (h pendingHeap) Less(i, j int) bool {
	if h[i].T != h[j].T {
		return h[i].T < h[j].T
	}
	return h[i].seq < h[j].seq
}
func (h pendingHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *pendingHeap) Push(x any)   { *h = append(*h, x.(pendingItem)) }
func (h *pendingHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// Tap is the loop's side of the online evaluator: it never blocks. It is
// called from the loop's goroutine only.
type Tap struct {
	want  map[string]bool
	ch    chan Item
	clock time.Time

	lost     int
	from, to int64

	mode     string
	haveMode bool
}

// NewTap makes a tap for the streams o reads, with a queue of n items, and
// the channel o.Run reads.
func NewTap(o *Online, n int) (*Tap, <-chan Item) {
	t := &Tap{want: map[string]bool{}, ch: make(chan Item, n)}
	for _, s := range o.Streams() {
		t.want[s] = true
	}
	return t, t.ch
}

// Wants says whether the evaluator reads a stream.
func (t *Tap) Wants(stream string) bool { return t.want[stream] }

// SetClock sets the loop's clock as the tap hands records over: the latest
// time at which the loop knew it had taken in everything its sources had sent.
// Not simply the time of the call: a loop catching up after a pause writes
// records, stamped before it, out of several queues in no particular order,
// and a clock read while it does so would carry the watermark past the ones
// still waiting.
func (t *Tap) SetClock(at time.Time) { t.clock = at }

// Record hands over one record of a stream the evaluator reads, taken at
// rt, at the clock SetClock gave, and passes over any other. rec is copied.
func (t *Tap) Record(stream string, rt time.Time, rec []byte) {
	t.RecordAt(t.clock, stream, rt, rec)
}

// RecordAt is Record with the loop's clock given.
func (t *Tap) RecordAt(at time.Time, stream string, rt time.Time, rec []byte) {
	if !t.want[stream] {
		return
	}
	t.put(Item{Kind: ItemRecord, At: at.UnixNano(), T: rt.UnixNano(), Stream: stream, Rec: slices.Clone(rec)}, true)
}

// Mode hands over the DUT entering a mode at mt.
func (t *Tap) Mode(at, mt time.Time, mode string) {
	t.mode, t.haveMode = mode, true
	t.put(Item{Kind: ItemMode, At: at.UnixNano(), T: mt.UnixNano(), Mode: mode, Stream: "mode " + mode}, true)
}

// Tick hands over the daemon's clock. A tick that does not fit is not a
// loss: the next one says the same, later.
func (t *Tap) Tick(at time.Time) {
	t.put(Item{Kind: ItemTick, At: at.UnixNano()}, false)
}

// Close ends the evaluator's input.
func (t *Tap) Close() { close(t.ch) }

func (t *Tap) put(it Item, counts bool) {
	if t.lost > 0 {
		g := &Gap{From: t.from, To: t.to, Lost: t.lost, Reason: "overflow", mode: t.mode, haveMode: t.haveMode}
		select {
		case t.ch <- Item{Kind: itemGap, At: it.At, gap: g}:
			t.lost = 0
		default:
		}
	}
	if t.lost == 0 {
		select {
		case t.ch <- it:
			return
		default:
		}
	}
	if !counts {
		return
	}
	if t.lost == 0 {
		t.from, t.to = it.T, it.T
	}
	t.lost++
	t.from, t.to = min(t.from, it.T), max(t.to, it.T)
}

// Clock is the clock SetClock last gave.
func (t *Tap) Clock() time.Time { return t.clock }
