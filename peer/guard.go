package peer

import (
	"fmt"
	"time"
)

// Guard decides whether the two protection rules every master must have
// hold (doc/peer-protocol.md §9.4). It only decides: removing the stress, and
// recording that it did, is the master's.
type Guard struct {
	// WatermarkAge is how old the DUT rack's watermark may be, by this
	// rack's clock, before monitoring is lost. The protocol says at most
	// 100 ms.
	WatermarkAge time.Duration
	// Abort is the monitor whose dev asks for protection; "" for none. The
	// protocol reserves "abort".
	Abort string
}

// Verdict is what the guard found: whether each rule holds, and why.
type Verdict struct {
	MonitoringLost bool
	LostWhy        string
	DUTAbort       bool
	AbortWhy       string
}

// Check judges the view at now, this rack's clock. Call it on every record
// the view takes and at least every 10 ms.
func (g Guard) Check(v *View, now time.Time) Verdict {
	v.mu.Lock()
	connected, wm, wmRecv, off, haveOff := v.connected, v.wm, v.wmRecv, v.offset, v.haveOff
	v.mu.Unlock()
	var r Verdict
	switch {
	case !connected:
		r.MonitoringLost, r.LostWhy = true, "the DUT rack is not connected"
	case wm == 0:
		r.MonitoringLost, r.LostWhy = true, "the DUT rack has sent no watermark yet"
	default:
		// The watermark's age by this clock: its own time less the offset
		// once there is an estimate, and when it arrived before.
		at := wmRecv
		if haveOff {
			at = time.Unix(0, wm-int64(off))
		}
		if age := now.Sub(at); age > g.WatermarkAge {
			r.MonitoringLost, r.LostWhy = true, fmt.Sprintf("the DUT rack's watermark is %v old, beyond %v",
				age.Round(time.Microsecond), g.WatermarkAge)
		}
	}
	if g.Abort != "" && connected && v.Latest(g.Abort) == StateDev {
		r.DUTAbort, r.AbortWhy = true, fmt.Sprintf("the DUT rack's monitor %s is dev", g.Abort)
	}
	return r
}
