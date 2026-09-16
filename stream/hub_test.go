package stream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/google/uuid"
	"github.com/rveen/logb"
)

func uid(s string) [16]byte { return uuid.NewSHA1(uuid.NameSpaceOID, []byte(s)) }

func testSchema(name string) *logb.Schema {
	return &logb.Schema{
		UUID:       uid("stream/" + name),
		Name:       name,
		RecordBits: 128,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisExplicit,
		AxisExp:    -9,
		AxisUnit:   "s",
		AxisScale:  logb.TickVal(1),
		AxisField:  0,
		Fields: []logb.Field{
			{Name: "t", BitOffset: 0, BitWidth: 64, Type: logb.TypeUint},
			{Name: "v", BitOffset: 64, BitWidth: 32, Type: logb.TypeFloat, Unit: "V", Hold: true},
			{Name: "n", BitOffset: 96, BitWidth: 32, Type: logb.TypeUint},
		},
	}
}

func rec(t uint64, v float32, n uint32) []byte {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint64(b[0:], t)
	binary.LittleEndian.PutUint32(b[8:], uint32(v)) // value shape is not the point here
	binary.LittleEndian.PutUint32(b[12:], n)
	return b
}

// collect drains a subscription into a buffer, releasing its allowance as a
// real consumer must.
func collect(s *Sub) []byte {
	var out []byte
	for b := range s.Chunks() {
		out = append(out, b...)
		s.Done(len(b))
	}
	return out
}

// decode reads a stream back and reports what it holds, failing the test if it
// is not a valid Logb file.
func decode(t *testing.T, b []byte) (streams map[string]int, meta int, attach int) {
	t.Helper()
	r, err := logb.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("the stream is not a Logb file: %v", err)
	}
	streams = map[string]int{}
	for {
		batch, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decoding the stream: %v", err)
		}
		streams[batch.Schema.Name] += int(batch.Count)
	}
	if r.Truncated {
		t.Fatal("the stream ended at damage rather than at a clean end")
	}
	return streams, len(r.Meta), len(r.Attachments)
}

// A consumer attached before the first byte must receive exactly the file.
func TestSubscriberFromTheStartSeesTheFile(t *testing.T) {
	h := New()
	var file bytes.Buffer
	sub := h.Subscribe(0)
	done := make(chan []byte, 1)
	go func() { done <- collect(sub) }()

	writeRecording(t, io.MultiWriter(&file, h), 3)
	h.Close()

	got := <-done
	if !bytes.Equal(got, file.Bytes()) {
		t.Fatalf("the stream is %d bytes, the file is %d, and they differ",
			len(got), file.Len())
	}
}

// The claim §6.1 rests on: a consumer joining late gets a valid, playable file,
// because the segment preamble it needs is replayed in front of the live bytes.
func TestLateSubscriberGetsAValidFile(t *testing.T) {
	h := New()
	var file bytes.Buffer
	w := newWriter(t, io.MultiWriter(&file, h))
	s := testSchema("can0.raw")

	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	if err := w.BeginSegment(1_700_000_000_000_000_000); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteMeta("bus", "vcan0"); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteAttach("source.dbc", []byte("VERSION \"x\"\n")); err != nil {
		t.Fatal(err)
	}
	// Two segments before anyone is listening.
	for seg := 0; seg < 2; seg++ {
		if seg > 0 {
			if err := w.BeginSegment(1_700_000_000_000_000_000 + int64(seg)*1e9); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.WriteData(s, logb.TickVal(int64(seg)*1e9), 0, 1, rec(0, 12, uint32(seg))); err != nil {
			t.Fatal(err)
		}
		if err := w.SetHold(s, logb.TickVal(int64(seg)*1e9), 0,
			[]bool{false, true, false}, rec(0, 12, uint32(seg))); err != nil {
			t.Fatal(err)
		}
	}

	// Join here, mid-recording.
	sub := h.Subscribe(0)
	done := make(chan []byte, 1)
	go func() { done <- collect(sub) }()

	for seg := 2; seg < 5; seg++ {
		if err := w.BeginSegment(1_700_000_000_000_000_000 + int64(seg)*1e9); err != nil {
			t.Fatal(err)
		}
		if err := w.WriteData(s, logb.TickVal(int64(seg)*1e9), 0, 1, rec(0, 12, uint32(seg))); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	h.Close()

	got := <-done
	if err := sub.Err(); !errors.Is(err, ErrClosed) {
		t.Fatalf("subscription ended with %v, want ErrClosed", err)
	}

	// It must be a file, not a fragment: header, schemas, decodable records.
	streams, meta, attach := decode(t, got)
	if n := streams["can0.raw"]; n != 3 {
		t.Errorf("the late subscriber received %d records, want the 3 written after it joined", n)
	}
	if meta == 0 {
		t.Error("the metadata written before the subscriber joined did not reach it")
	}
	if attach == 0 {
		t.Error("the attached database written before the subscriber joined did not reach it")
	}
	if len(got) >= file.Len() {
		t.Errorf("the late stream is %d bytes and the whole file is %d; it should be shorter",
			len(got), file.Len())
	}
}

// The preamble must meet the live stream exactly: nothing missing, nothing
// twice. A duplicated SCHEMA would be harmless, but a duplicated DATA frame
// would double every value in a plot.
func TestJoinIsSeamlessUnderConcurrentWrites(t *testing.T) {
	h := New()
	w := newWriter(t, h)
	s := testSchema("can0.raw")
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	if err := w.BeginSegment(1_700_000_000_000_000_000); err != nil {
		t.Fatal(err)
	}

	// Subscribers join while the producer is writing, at many different
	// points, and every one of them must decode cleanly.
	const total = 200
	subs := make([]*Sub, 0, 20)
	results := make([]chan []byte, 0, 20)
	for i := 0; i < total; i++ {
		if i%10 == 0 && len(subs) < 20 {
			sub := h.Subscribe(0)
			ch := make(chan []byte, 1)
			go func(s *Sub) { ch <- collect(s) }(sub)
			subs = append(subs, sub)
			results = append(results, ch)
		}
		if err := w.WriteData(s, logb.TickVal(int64(i)), 0, 1, rec(uint64(i), 12, uint32(i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	h.Close()

	for i, ch := range results {
		got := <-ch
		streams, _, _ := decode(t, got)
		n := streams["can0.raw"]
		// Subscriber i joined after 10*i records, so it must see the rest.
		want := total - 10*i
		if n != want {
			t.Errorf("subscriber %d received %d records, want %d", i, n, want)
		}
	}
}

// A consumer that cannot keep up is disconnected, and the recording is
// untouched. The alternative — dropping frames from the middle of its stream —
// would hand it a corrupt file that resynchronises at the next SYNC and never
// says what it lost.
func TestSlowSubscriberIsDroppedNotCorrupted(t *testing.T) {
	h := New()
	var file bytes.Buffer

	// A tiny allowance, and a consumer that never reads.
	slow := h.Subscribe(64)
	fast := h.Subscribe(0)
	fastDone := make(chan []byte, 1)
	go func() { fastDone <- collect(fast) }()

	writeRecording(t, io.MultiWriter(&file, h), 40)

	if err := slow.Err(); !errors.Is(err, ErrTooSlow) {
		t.Fatalf("the slow subscriber ended with %v, want ErrTooSlow", err)
	}
	if st := h.Stats(); st.Dropped != 1 {
		t.Errorf("stats report %d dropped subscribers, want 1", st.Dropped)
	}

	h.Close()
	got := <-fastDone

	// The recording is unaffected and the healthy subscriber is complete.
	if !bytes.Equal(got, file.Bytes()) {
		t.Errorf("a slow consumer disturbed a healthy one: %d bytes against the file's %d",
			len(got), file.Len())
	}
	decode(t, file.Bytes())
}

// Subscribing after the producer has finished must fail cleanly rather than
// hand out a stream that never ends.
func TestSubscribeAfterClose(t *testing.T) {
	h := New()
	writeRecording(t, h, 2)
	h.Close()

	s := h.Subscribe(0)
	if b := collect(s); len(b) != 0 {
		t.Errorf("a subscription taken after the end yielded %d bytes", len(b))
	}
	if err := s.Err(); !errors.Is(err, ErrClosed) {
		t.Errorf("Err = %v, want ErrClosed", err)
	}
}

func newWriter(t *testing.T, w io.Writer) *logb.Writer {
	t.Helper()
	lw, err := logb.NewWriter(w)
	if err != nil {
		t.Fatal(err)
	}
	return lw
}

// writeRecording writes a small complete recording of n segments.
func writeRecording(t *testing.T, w io.Writer, segments int) {
	t.Helper()
	lw := newWriter(t, w)
	s := testSchema("can0.raw")
	if err := lw.AddStream(s); err != nil {
		t.Fatal(err)
	}
	for seg := 0; seg < segments; seg++ {
		if err := lw.BeginSegment(1_700_000_000_000_000_000 + int64(seg)*1e9); err != nil {
			t.Fatal(err)
		}
		if seg == 0 {
			if err := lw.WriteMeta("bus", "vcan0"); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 20; i++ {
			if err := lw.WriteData(s, logb.TickVal(int64(seg)*1e9), 0, 1,
				rec(uint64(i), 12, uint32(i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := lw.Close(); err != nil {
		t.Fatal(err)
	}
}

// Room is the question a waveform producer asks before it writes (§10.5): bus
// frames must not be dropped, so a consumer that cannot keep up with them is
// disconnected, and a hole in a byte stream is corruption rather than loss. An
// acquisition may be dropped instead, and dropping one knowingly — and
// recording that it was dropped — is worth far more to a browser than being
// disconnected and having to fetch a whole preamble again.
func TestRoom(t *testing.T) {
	h := New()
	// With nobody connected there is nothing to fall behind, and a producer
	// must not start throwing acquisitions away because no one is watching.
	if h.Room() != math.MaxInt {
		t.Fatalf("with no subscribers, Room is %d", h.Room())
	}

	const limit = 1 << 16
	s := h.Subscribe(limit)
	if got := h.Room(); got > limit {
		t.Fatalf("a fresh subscriber has %d bytes of room, past its limit of %d", got, limit)
	}
	before := h.Room()
	writeRecording(t, h, 1)
	after := h.Room()
	if after >= before {
		t.Fatalf("Room did not fall as the producer wrote: %d then %d", before, after)
	}

	// Consuming gives the room back, which is what makes this a question worth
	// asking again at the next trigger rather than a one-way ratchet.
	for h.Room() < before {
		select {
		case b, ok := <-s.Chunks():
			if !ok {
				t.Fatal("the subscriber was closed")
			}
			s.Done(len(b))
		default:
			t.Fatalf("%d bytes of room and nothing left to consume; there were %d", h.Room(), before)
		}
	}
	if h.Room() != before {
		t.Errorf("after consuming everything, %d bytes of room; there were %d", h.Room(), before)
	}

	// The least room among the connected, not the most: a producer must ask
	// about the consumer that is worst off.
	tight := h.Subscribe(4096)
	if got := h.Room(); got > 4096 {
		t.Errorf("with a subscriber limited to 4096, Room is %d", got)
	}
	_ = tight

	// A subscriber that has been disconnected no longer bounds anything.
	h.Close()
	if h.Room() != math.MaxInt {
		t.Errorf("after the recording ended, Room is %d", h.Room())
	}
}
