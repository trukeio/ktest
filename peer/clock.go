package peer

import "time"

// Offsets estimates the DUT rack's clock less this one, NTP-style: of the
// last N round trips, the one with the shortest round trip, whose answer is
// pinned closest to its midpoint. Its uncertainty is half that round trip:
// however unevenly the two halves went, the answer was read within it.
type Offsets struct {
	N       int // how many samples are kept; 0 is 8
	samples []ClockSample
}

// Estimate is an offset with its uncertainty.
type Estimate struct {
	Offset      time.Duration // the DUT rack's clock less this one
	Uncertainty time.Duration // half the round trip of the sample it came from
	RTT         time.Duration
	At          time.Time // when that sample was taken, by this clock
}

// Add takes a sample and returns the estimate it leaves.
func (o *Offsets) Add(s ClockSample) Estimate {
	n := o.N
	if n <= 0 {
		n = 8
	}
	o.samples = append(o.samples, s)
	if len(o.samples) > n {
		o.samples = o.samples[len(o.samples)-n:]
	}
	best := o.samples[0]
	for _, x := range o.samples[1:] {
		if x.RTT() < best.RTT() {
			best = x
		}
	}
	return Estimate{Offset: best.Offset(), Uncertainty: best.RTT() / 2, RTT: best.RTT(), At: best.Sent}
}
