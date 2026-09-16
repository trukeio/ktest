package scope

import (
	"math"
	"math/rand"
	"testing"

	"github.com/rveen/logb/viewer/decimate"
	"github.com/rveen/logb/viewer/index"
)

// series is the acquisition as viewer/decimate wants it: every sample as a
// float64 with a presence flag, on an axis of one tick a sample. The tick is
// what makes the comparison exact rather than approximate — decimate's fraction
// is (x - from) / (to - from), which on a unit axis is i / (n-1), the same
// expression Envelope bucketing uses.
func series(a *Acquisition) *index.Series {
	s := &index.Series{}
	s.Axis.Time = true
	for i := 0; i < a.Len(); i++ {
		s.Axis.Ticks = append(s.Axis.Ticks, int64(i))
		s.Vals = append(s.Vals, float64(float32(a.Volt(i))))
		s.Present = append(s.Present, true)
	}
	return s
}

// The envelope written into the recording must be the reduction viewer/decimate
// does, not something like it: the whole claim of §10.4 is that the browser is
// shown a decimation with a known containment property, and a second
// implementation that quietly bucketed differently would give that up while
// looking right.
func TestEnvelopeAgreesWithDecimate(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for _, n := range []int{1, 2, 999, 1000, 1001, 7000, 14000, 40001} {
		for _, buckets := range []int{1, 3, 300, 1000, 3000} {
			a := &Acquisition{Codes: make([]byte, n),
				Chan: ChanSettings{VDiv: 0.5, Ofst: 0.25},
				Set:  Settings{TDiv: 1e-3, Sara: 1e6}, Sign: SignSub256}
			for i := range a.Codes {
				a.Codes[i] = byte(rng.Intn(256))
			}
			var e Envelope
			a.Envelope(&e, buckets)
			want := decimate.Numeric(series(a), 0, float64(n-1), buckets, decimate.Linear)

			if e.Exact != want.Exact {
				t.Fatalf("n=%d buckets=%d: exact %v, decimate says %v", n, buckets, e.Exact, want.Exact)
			}
			if e.Len() != len(want.N) {
				t.Fatalf("n=%d buckets=%d: %d buckets, decimate gives %d", n, buckets, e.Len(), len(want.N))
			}
			for b := 0; b < e.Len(); b++ {
				if int32(e.N[b]) != want.N[b] {
					t.Fatalf("n=%d buckets=%d bucket %d: n %d, decimate says %d", n, buckets, b, e.N[b], want.N[b])
				}
				if e.N[b] == 0 {
					if !math.IsNaN(float64(e.Min[b])) || !math.IsNaN(float64(e.Max[b])) {
						t.Fatalf("n=%d buckets=%d bucket %d: empty, and not absent", n, buckets, b)
					}
					continue
				}
				if float64(e.Min[b]) != want.Min[b] || float64(e.Max[b]) != want.Max[b] {
					t.Fatalf("n=%d buckets=%d bucket %d: [%g %g], decimate gives [%g %g]",
						n, buckets, b, e.Min[b], e.Max[b], want.Min[b], want.Max[b])
				}
			}
		}
	}
}

// The reduction exists to keep the one-sample spike that a stride would throw
// away, which is usually the thing the engineer opened the file to find.
func TestEnvelopeKeepsALoneSpike(t *testing.T) {
	a := &Acquisition{Codes: make([]byte, 1_000_000),
		Chan: ChanSettings{VDiv: 1}, Set: Settings{TDiv: 1e-3, Sara: 1e9}, Sign: SignSub256}
	for i := range a.Codes {
		a.Codes[i] = 0
	}
	a.Codes[499_999] = 100 // one sample, four divisions up

	var e Envelope
	a.Envelope(&e, 3000)
	found := false
	for b := 0; b < e.Len(); b++ {
		if e.Max[b] > 3.9 {
			found = true
		}
	}
	if !found {
		t.Error("a one-in-a-million spike did not survive the envelope")
	}
	// The scope's own sparsing would have lost it: every 333rd point.
	kept := false
	for i := 0; i < a.Len(); i += 333 {
		if a.Codes[i] != 0 {
			kept = true
		}
	}
	if kept {
		t.Skip("the stride happened to land on the spike; the comparison says nothing this time")
	}
}

// An envelope is computed per trigger, so it must reuse the buffer it is given
// rather than allocate a new one each time.
func TestEnvelopeReusesItsBuffers(t *testing.T) {
	a := &Acquisition{Codes: make([]byte, 20000), Chan: ChanSettings{VDiv: 1},
		Set: Settings{TDiv: 1e-3, Sara: 1e7}, Sign: SignSub256}
	var e Envelope
	a.Envelope(&e, 3000)
	first := &e.Min[0]
	allocs := testing.AllocsPerRun(20, func() { a.Envelope(&e, 3000) })
	if allocs != 0 {
		t.Errorf("%g allocations an acquisition, want none", allocs)
	}
	if &e.Min[0] != first {
		t.Error("the envelope moved to a new buffer")
	}
}

// The axis the reduced stream is written on has to cover the same span as the
// samples: a widget draws the band against the trace's own time, and a bucket
// centre half a bucket out is a trace drawn in the wrong place.
func TestEnvelopeAxis(t *testing.T) {
	a := &Acquisition{Codes: make([]byte, 14000), Chan: ChanSettings{VDiv: 1},
		Set: Settings{TDiv: 1e-3, Sara: 1e6}, Sign: SignSub256}
	var e Envelope
	a.Envelope(&e, 1000)
	span := a.Duration().Seconds() // 14 ms
	if got, want := e.Step*float64(e.Len()), span; math.Abs(got-want) > 1e-15 {
		t.Errorf("the envelope covers %g s, the acquisition %g s", got, want)
	}
	if got, want := e.First, a.First().Seconds()+e.Step/2; math.Abs(got-want) > 1e-15 {
		t.Errorf("first centre at %g s, want %g", got, want)
	}
	last := e.First + e.Step*float64(e.Len()-1)
	if last > a.First().Seconds()+span {
		t.Errorf("the last centre at %g s is past the acquisition's end", last)
	}
}
