// Package stream fans one Logb byte stream out to many consumers.
//
// §6.1 of doc/test-software-outline.md settles the transport by deleting the
// question: Logb frames are self-delimiting, length-prefixed and CRC'd, so the
// transport supplies no framing of its own. A live stream is bytes. That is why
// nothing here knows what HTTP is — a Hub is an io.Writer with subscribers, and
// an HTTP handler, a Unix socket or a file are all the same thing to it.
//
// Two properties are the whole design.
//
// # A consumer joining late gets a valid file
//
// Not a fragment of one. Logb restates every schema and every held value at a
// segment boundary, so the bytes that opened the current segment are exactly
// what a reader needs in front of the frames that follow. The Hub keeps those
// bytes — the real ones, as written, not a reconstruction — and replays them to
// each new subscriber ahead of the live stream. So
//
//	curl http://rack/stream/live > run.logb
//
// is a recording: a valid, playable file that begins with a header and a
// segment, because that is what the subscriber was sent. §6.1's own test.
//
// It is the same frames, not the same bytes. A synthesised preamble groups what
// the recording said once — its metadata and attachments — ahead of the current
// segment's schemas, where the original file had them wherever they were
// written. Nothing is lost, duplicated or reordered in a way a reader can see:
// a schema still precedes every DATA frame that uses its id, which is the only
// ordering the format requires. A consumer attached before the producer's first
// byte receives the file byte for byte, because then there is no history to
// synthesise.
//
// # A slow consumer is disconnected, never fed a stream with a hole in it
//
// The two rules that meet here are §10.5's — bus frames must not be dropped —
// and the requirement that the daemon never stall. A subscriber that cannot
// keep up may not block the recording, because the recorder that stalls is the
// one whose receive queue overflows and loses CAN frames for everybody. But
// frames also cannot be dropped from the middle of a byte stream: what would
// arrive is not a lossy Logb stream, it is a corrupt one, and the reader would
// resynchronise at the next SYNC and never know what it missed.
//
// So the third option is taken. A subscriber that exceeds its buffer is closed
// with ErrTooSlow, and its consumer is told. Reconnecting costs it a fresh
// preamble and it is whole again — which is the reconnect story §6.1 says Logb
// gives for free, used here for the case it was not written for.
package stream

import (
	"encoding/binary"
	"errors"
	"math"
	"sync"

	"github.com/rveen/logb"
)

// The framing, which is all this package needs to know about the format:
// a 16-byte file header, then frames of an 8-byte header, a payload and a
// 4-byte CRC.
const (
	headerLen = 16
	frameHdr  = 8
	frameCRC  = 4
)

// ErrTooSlow closes a subscriber that fell too far behind. It is not a
// failure of the recording, which is unaffected, and reconnecting recovers.
var ErrTooSlow = errors.New("stream: consumer fell behind and was disconnected")

// ErrClosed is returned to subscribers when the producer finishes.
var ErrClosed = errors.New("stream: the recording ended")

// DefaultBuffer is how many bytes a subscriber may fall behind by.
//
// Generous, because the cost of being wrong is a disconnection: DATA frames
// carry thousands of records each, so this is seconds of a busy bus rather
// than milliseconds.
const DefaultBuffer = 8 << 20

// A Hub is the producer side. Give it to a Logb writer as its io.Writer —
// usually through an io.MultiWriter, alongside the file — and subscribe to it.
//
// Write is called from the recording goroutine and Subscribe from others; both
// take the same lock, which is what makes a subscriber's preamble and its first
// live frame meet exactly, with no gap and no repetition.
type Hub struct {
	mu     sync.Mutex
	closed bool

	// The bytes a late subscriber needs in front of the live stream.
	header []byte

	// once holds the frames written once for the whole recording — META and
	// ATTACH — in the order they were written. The embedded database lives
	// here, which is why a late consumer can still decode signals.
	once [][]byte

	// The current segment's preamble. Reset at every SYNC, because that is
	// exactly what a segment boundary means: everything below is restated.
	sync    []byte
	schemas [][]byte
	runs    [][]byte
	holds   [][]byte

	// part is the bytes received but not yet forming a whole frame. A frame
	// reaches Write in three pieces — header, payload, CRC — so reassembly is
	// the normal case, not an edge one.
	part []byte
	// atHeader is true until the file header has been consumed.
	atHeader bool

	subs  map[*Sub]struct{}
	stats Stats
}

// Stats is what the fan-out cannot say about itself.
type Stats struct {
	Frames      int64 // frames dispatched
	Bytes       int64 // bytes dispatched to the stream as a whole
	Subscribers int   // currently connected
	Joined      int64 // subscribers ever connected
	Dropped     int64 // subscribers disconnected for falling behind
}

// New returns an empty Hub.
func New() *Hub {
	return &Hub{
		subs:     map[*Sub]struct{}{},
		atHeader: true,
	}
}

// Write accepts producer bytes. It never blocks on a subscriber.
func (h *Hub) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, ErrClosed
	}

	h.part = append(h.part, p...)
	h.stats.Bytes += int64(len(p))

	if h.atHeader {
		if len(h.part) < headerLen {
			return len(p), nil
		}
		h.header = append([]byte(nil), h.part[:headerLen]...)
		h.part = h.part[headerLen:]
		h.atHeader = false
		// Dispatched like anything else. A subscriber attached before the
		// producer wrote its first byte got an empty preamble, so this is the
		// only time it will see the header; one joining later finds it in a
		// preamble instead. The rule is the same either way — live bytes go to
		// whoever is connected, history is synthesised for whoever is not.
		for sub := range h.subs {
			h.send(sub, h.header)
		}
	}

	for {
		if len(h.part) < frameHdr {
			break
		}
		n := binary.LittleEndian.Uint32(h.part[0:4])
		total := frameHdr + int(n) + frameCRC
		if len(h.part) < total {
			break
		}
		frame := append([]byte(nil), h.part[:total]...)
		h.part = h.part[total:]
		h.onFrame(logb.FrameType(frame[4]), frame)
	}
	// Reclaim: part is normally empty here, but a partially received frame
	// would otherwise keep the whole backing array alive.
	if len(h.part) == 0 && cap(h.part) > 1<<16 {
		h.part = nil
	}
	return len(p), nil
}

// onFrame files a complete frame into the preamble it belongs to and sends it
// to everyone currently connected. Called with the lock held.
func (h *Hub) onFrame(t logb.FrameType, frame []byte) {
	switch t {
	case logb.FrameSync:
		// A new segment restates everything below it, so the old restatements
		// are not merely stale, they are wrong: they name stream ids this
		// segment has rebound (§6.6).
		h.sync = frame
		h.schemas = h.schemas[:0]
		h.runs = h.runs[:0]
		h.holds = h.holds[:0]
	case logb.FrameSchema:
		h.schemas = append(h.schemas, frame)
	case logb.FrameRun:
		h.runs = append(h.runs, frame)
	case logb.FrameHold:
		h.holds = append(h.holds, frame)
	case logb.FrameMeta, logb.FrameAttach:
		h.once = append(h.once, frame)
	}
	h.stats.Frames++

	for s := range h.subs {
		h.send(s, frame)
	}
}

// send queues one chunk for a subscriber, or disconnects it. Lock held.
func (h *Hub) send(s *Sub, b []byte) {
	if s.closed {
		return
	}
	if s.queued+len(b) > s.limit {
		h.drop(s, ErrTooSlow)
		return
	}
	select {
	case s.ch <- b:
		s.queued += len(b)
	default:
		h.drop(s, ErrTooSlow)
	}
}

// drop disconnects a subscriber with a reason. Lock held.
func (h *Hub) drop(s *Sub, err error) {
	if s.closed {
		return
	}
	s.closed, s.err = true, err
	if errors.Is(err, ErrTooSlow) {
		h.stats.Dropped++
	}
	delete(h.subs, s)
	h.stats.Subscribers = len(h.subs)
	close(s.ch)
}

// Room is how many bytes the consumer with the least space left can still fall
// behind by, or MaxInt when nobody is connected.
//
// It exists for §10.5's split. Bus frames must not be dropped, so a consumer
// that cannot keep up with them is disconnected — a hole in a byte stream is
// corruption, not loss, and there is no third answer. A waveform acquisition
// may be dropped, and a producer that asks first can drop one knowingly and
// record that it did, which is worth far more to a browser than being
// disconnected and having to fetch a whole preamble again.
//
// It is a reading, not a reservation: a consumer may fall further behind
// between the question and the write. That is why the caller leaves a margin.
func (h *Hub) Room() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	room := math.MaxInt
	for s := range h.subs {
		if s.closed {
			continue
		}
		room = min(room, s.limit-s.queued)
	}
	return room
}

// Subscribe attaches a consumer.
//
// The returned Sub begins with the bytes that make what follows a valid file:
// the file header, the frames written once for the whole recording, and the
// current segment's preamble as it was actually written. Everything the
// producer writes from this moment follows it, with nothing missing and
// nothing sent twice — Write holds the same lock, so there is no window
// between the snapshot and the subscription.
//
// limit is how many bytes the consumer may fall behind by before it is
// disconnected; zero takes DefaultBuffer.
func (h *Hub) Subscribe(limit int) *Sub {
	h.mu.Lock()
	defer h.mu.Unlock()

	if limit <= 0 {
		limit = DefaultBuffer
	}
	s := &Sub{
		ch:    make(chan []byte, 1024),
		limit: limit,
		hub:   h,
	}
	if h.closed {
		s.closed, s.err = true, ErrClosed
		close(s.ch)
		return s
	}

	// The preamble, in the order a file carries it: header, segment, what was
	// said once, then this segment's restatements.
	pre := make([]byte, 0, 4096)
	pre = append(pre, h.header...)
	if h.sync != nil {
		pre = append(pre, h.sync...)
	}
	for _, f := range h.once {
		pre = append(pre, f...)
	}
	for _, f := range h.schemas {
		pre = append(pre, f...)
	}
	for _, f := range h.runs {
		pre = append(pre, f...)
	}
	for _, f := range h.holds {
		pre = append(pre, f...)
	}
	if len(pre) > 0 {
		s.ch <- pre
		s.queued = len(pre)
	}

	h.subs[s] = struct{}{}
	h.stats.Subscribers = len(h.subs)
	h.stats.Joined++
	return s
}

// Close ends the stream. Every subscriber sees its channel close after the
// bytes already queued for it, so a consumer writing to a file gets a complete
// one.
func (h *Hub) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	for s := range h.subs {
		h.drop(s, ErrClosed)
	}
	return nil
}

// Stats reports the fan-out's own accounting.
func (h *Hub) Stats() Stats {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stats
}

// A Sub is one consumer's view of the stream.
type Sub struct {
	hub   *Hub
	ch    chan []byte
	limit int

	// Guarded by hub.mu.
	queued int
	closed bool
	err    error
}

// Chunks is the stream. It is closed when the subscription ends, after which
// Err says why.
//
// A consumer must call Done for every chunk it has finished writing, or its
// allowance never frees and it disconnects itself.
func (s *Sub) Chunks() <-chan []byte { return s.ch }

// Done releases the allowance a chunk was holding.
func (s *Sub) Done(n int) {
	s.hub.mu.Lock()
	s.queued -= n
	if s.queued < 0 {
		s.queued = 0
	}
	s.hub.mu.Unlock()
}

// Err reports why the subscription ended: ErrTooSlow, ErrClosed, or nil while
// it is still running.
func (s *Sub) Err() error {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	return s.err
}

// Cancel ends the subscription from the consumer's side.
func (s *Sub) Cancel() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	if !s.closed {
		s.hub.drop(s, nil)
	}
}
