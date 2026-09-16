package scope

import "math"

// An Envelope is one acquisition reduced to what a screen can draw: one bucket
// a pixel column, carrying the least and greatest sample in it (§10.4).
//
// It is the reduction viewer/decimate does, done on an acquisition as it
// arrives rather than on an indexed file, and without materialising the
// acquisition as float64 values and a presence mask first — which at 14 Mpts
// would be 112 MB a trigger for a 48 kB answer. envelope_test.go asserts the
// two agree exactly on the same samples, so this is a faster path to the same
// reduction and not a second opinion about what the reduction is.
//
// The scope's own sparsing is no substitute. Every nth point is the aliasing
// and the lost glitch an envelope exists to prevent: a one-sample spike between
// two kept points is simply gone, and nothing says so. Min and max keep it.
type Envelope struct {
	// Min and Max are the extremes in each bucket; N is how many samples it
	// held. N is not decoration: where it is zero the bucket has no sample and
	// the values are meaningless, which is a fact about the acquisition and must
	// reach the screen as a gap rather than as a zero.
	Min, Max []float32
	N        []uint32

	// First is where the first bucket's centre lies relative to the
	// acquisition's own time, and Step the distance between centres: the axis
	// the reduced stream is written on.
	First, Step float64

	// Exact is true when the acquisition held no more samples than there were
	// buckets, so Min and Max are the samples themselves. At high zoom that is
	// what an engineer is looking at, and a bucket centre would put a sample at
	// a position it was never recorded at.
	Exact bool
}

// Len is how many buckets the envelope holds.
func (e *Envelope) Len() int { return len(e.N) }

// Envelope reduces the acquisition to at most buckets columns, filling e so
// that a caller keeps one envelope for the life of a run rather than allocating
// per trigger.
//
// The partition is in sample index space, which for a uniform axis is the same
// partition as decimate's: with the axis at first + i x dt, the fraction
// (x - first) / ((n-1) x dt) is i / (n-1), and bucketing on the index says so
// without the subtraction.
func (a *Acquisition) Envelope(e *Envelope, buckets int) {
	if e == nil {
		return
	}
	if buckets < 1 {
		buckets = 1
	}
	n := a.Len()
	dt := a.Step().Seconds()
	first := a.First().Seconds()

	if n <= buckets {
		e.resize(n)
		e.First, e.Step, e.Exact = first, dt, true
		for i := 0; i < n; i++ {
			v := float32(a.Volt(i))
			e.Min[i], e.Max[i], e.N[i] = v, v, 1
		}
		return
	}

	e.resize(buckets)
	// The buckets span the samples, so the first centre is half a bucket in.
	span := float64(n) * dt
	e.Step = span / float64(buckets)
	e.First, e.Exact = first+e.Step/2, false
	for b := range e.N {
		e.N[b] = 0
		e.Min[b], e.Max[b] = float32(math.NaN()), float32(math.NaN())
	}
	scale := a.Chan.VDiv / Codes
	ofst := a.Chan.Ofst
	// The divisor is n-1 rather than n because decimate's range runs from the
	// first sample to the last, not to one past it.
	last := float64(n - 1)
	for i := 0; i < n; i++ {
		b := int(float64(i) / last * float64(buckets))
		if b >= buckets {
			b = buckets - 1
		}
		v := float32(float64(a.Sign.Signed(a.Codes[i]))*scale - ofst)
		if e.N[b] == 0 {
			e.Min[b], e.Max[b] = v, v
		} else {
			if v < e.Min[b] {
				e.Min[b] = v
			}
			if v > e.Max[b] {
				e.Max[b] = v
			}
		}
		e.N[b]++
	}
}

func (e *Envelope) resize(n int) {
	if cap(e.N) < n {
		e.Min = make([]float32, n)
		e.Max = make([]float32, n)
		e.N = make([]uint32, n)
		return
	}
	e.Min, e.Max, e.N = e.Min[:n], e.Max[:n], e.N[:n]
}
