//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/rveen/ktest/can"
)

// eintrs accumulates the interrupted-and-retried receives of every socket this
// command closed. A nonzero count is that many frames a reader without the
// retry loop would have lost in silence, which is why it is reported rather
// than hidden. See can.Conn.Interrupts.
var eintrs uint64

// done closes a socket and keeps its interrupt count.
func done(c *can.Conn) {
	eintrs += c.Interrupts()
	c.Close()
}

// A probe measures one property of the interface. It returns a one-line
// summary for a human and a detail map for -json, which is the shape §8 wants
// for writing capabilities into a recording.
type probe struct {
	name string
	desc string
	run  func(iface string) (string, map[string]any, error)
}

func probes() []probe {
	return []probe{
		{"loopback", "classic frame round-trip and its latency", probeLoopback},
		{"fd", "CAN FD 64-byte payload and the BRS flag", probeFD},
		{"errframe", "delivery of a synthetic CAN_ERR_FLAG frame", probeErrFrame},
		{"timestamp", "which timestamp option actually stamps", probeTimestamp},
		{"pacing", "cyclic transmit jitter, userspace against CAN_BCM", probePacing},
		{"throughput", "frames per second the interface will accept", probeThroughput},
		{"overflow", "whether a lost frame is reported as lost", probeOverflow},
	}
}

// probeLoopback confirms the basics and measures how long a frame takes to
// come back, which bounds every other timing figure here.
func probeLoopback(iface string) (string, map[string]any, error) {
	rx, err := can.Open(iface, can.Options{Stamp: can.StampTimestampNS})
	if err != nil {
		return "", nil, err
	}
	defer done(rx)
	tx, err := can.Open(iface, can.Options{})
	if err != nil {
		return "", nil, err
	}
	defer done(tx)
	rx.SetReadTimeout(time.Second)

	const n = 200
	var lat []float64
	out := can.New(0x123, []byte{1, 2, 3, 4, 5, 6, 7, 8})
	var in can.Frame
	for i := 0; i < n; i++ {
		sent := time.Now()
		if err := tx.Send(out); err != nil {
			return "", nil, fmt.Errorf("send: %w", err)
		}
		if err := rx.Recv(&in); err != nil {
			return "", nil, fmt.Errorf("recv: %w", err)
		}
		if in.ID != 0x123 || in.Len != 8 {
			return "", nil, fmt.Errorf("frame came back wrong: id=%#x len=%d", in.ID, in.Len)
		}
		if in.Stamped {
			lat = append(lat, float64(in.Time.Sub(sent).Nanoseconds())/1000)
		}
	}
	sort.Float64s(lat)
	d := map[string]any{"frames": n, "stamped": len(lat)}
	if len(lat) > 0 {
		d["latency_p50_us"] = round(pct(lat, 50))
		d["latency_p99_us"] = round(pct(lat, 99))
	}
	return fmt.Sprintf("%d/%d round-tripped, send to kernel timestamp p50 %.1f µs / p99 %.1f µs",
		n, n, pct(lat, 50), pct(lat, 99)), d, nil
}

// probeFD checks that a 64-byte payload survives with its flags, which is what
// makes CAN FD testable without an FD-capable adapter.
func probeFD(iface string) (string, map[string]any, error) {
	rx, err := can.Open(iface, can.Options{FD: true})
	if err != nil {
		return "", nil, err
	}
	defer done(rx)
	tx, err := can.Open(iface, can.Options{FD: true})
	if err != nil {
		return "", nil, err
	}
	defer done(tx)
	rx.SetReadTimeout(time.Second)

	pay := make([]byte, 64)
	for i := range pay {
		pay[i] = byte(i)
	}
	out := can.New(0x456, pay)
	out.Flags = can.FlagBRS
	if err := tx.Send(out); err != nil {
		return "", nil, fmt.Errorf("send: %w", err)
	}
	var in can.Frame
	if err := rx.Recv(&in); err != nil {
		return "", nil, fmt.Errorf("recv: %w", err)
	}
	ok := in.FD() && in.Len == 64
	for i := range pay {
		if in.Data[i] != pay[i] {
			ok = false
			break
		}
	}
	d := map[string]any{
		"mtu": in.Wire, "len": in.Len, "brs": in.BRS(),
		"esi": in.ESI(), "payload_intact": ok,
	}
	if !ok {
		return "", d, fmt.Errorf("FD frame did not survive: n=%d len=%d", in.Wire, in.Len)
	}
	return fmt.Sprintf("64-byte payload intact, %d-byte MTU, BRS %v", in.Wire, in.BRS()), d, nil
}

// probeErrFrame is the useful surprise: vcan loops back a frame with
// CAN_ERR_FLAG set, so the daemon's error handling can be exercised with no
// bus and no fault. It does not exercise the conditions that produce one.
func probeErrFrame(iface string) (string, map[string]any, error) {
	rx, err := can.Open(iface, can.Options{ErrMask: unix.CAN_ERR_MASK})
	if err != nil {
		return "", nil, err
	}
	defer done(rx)
	tx, err := can.Open(iface, can.Options{})
	if err != nil {
		return "", nil, err
	}
	defer done(tx)
	rx.SetReadTimeout(time.Second)

	if err := tx.Send(can.New(unix.CAN_ERR_FLAG|unix.CAN_ERR_BUSOFF, make([]byte, 8))); err != nil {
		return "", nil, fmt.Errorf("send: %w", err)
	}
	var in can.Frame
	if err := rx.Recv(&in); err != nil {
		return "not delivered", map[string]any{"delivered": false}, nil
	}
	d := map[string]any{
		"delivered": true,
		"can_id":    fmt.Sprintf("%#08x", in.ID),
		"err_flag":  in.Err(),
		"busoff":    in.ID&unix.CAN_ERR_BUSOFF != 0,
	}
	return fmt.Sprintf("synthetic bus-off delivered as %#08x", in.ID), d, nil
}

// probeTimestamp runs each option in a fresh child process on purpose.
//
// Enabling a timestamp option in a process that has already opened and closed
// other timestamping sockets gives intermittent results: repeated runs have
// stamped 31, 0, 200 and 54 frames out of 200 with SO_TIMESTAMPING where a
// fresh process stamped 200. SO_TIMESTAMPNS has been reliable throughout. The
// kernel mechanism behind that is not pinned down, so the probe isolates each
// option rather than explaining it, and the rule in §8 stands either way: an
// unstamped frame must never be recorded as t=0.
func probeTimestamp(iface string) (string, map[string]any, error) {
	order := []string{"none", "ts", "tsns", "tsing", "tsing-hw"}
	d := map[string]any{}

	// The naive pass runs FIRST and from cold, because that is the only way
	// to see the problem. Whatever enables receive timestamping appears to be
	// global and refcounted with a deferred disable, so any recent user of it
	// anywhere on the machine — including this command's own child processes
	// below — keeps it warm and hides the effect entirely. Hence the settle.
	time.Sleep(1500 * time.Millisecond)
	inProc := map[string]int{}
	for _, name := range order {
		got, err := stampCount(iface, stampNames[name])
		if err != nil {
			return "", nil, err
		}
		inProc[name] = got
	}

	// Now each option alone in a fresh process, which is the number to trust.
	self, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	var degraded, incomplete []string
	var reliable []string
	for _, name := range order {
		out, err := exec.Command(self, "-i", iface, "-stamp-one", name).CombinedOutput()
		if err != nil {
			return "", nil, fmt.Errorf("child for %s: %w: %s", name, err, out)
		}
		var got, total int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d/%d", &got, &total); err != nil {
			return "", nil, fmt.Errorf("child for %s said %q", name, out)
		}
		e := map[string]any{"isolated": got, "in_process": inProc[name], "frames": total}
		if inProc[name] < got {
			e["degraded"] = true
			degraded = append(degraded, fmt.Sprintf("%s %d→%d", stampNames[name], got, inProc[name]))
		} else if name != "none" {
			if got == total {
				reliable = append(reliable, stampNames[name].String())
			} else {
				// Short even on its own: the option is not dependable here.
				incomplete = append(incomplete,
					fmt.Sprintf("%s %d/%d", stampNames[name], got, total))
			}
		}
		d[name] = e
	}

	msg := "reliable: " + strings.Join(reliable, ", ")
	if len(incomplete) > 0 {
		msg += "; incomplete even in isolation: " + strings.Join(incomplete, ", ")
	}
	if len(degraded) > 0 {
		msg += "; lost stamps when opened after other timestamping sockets: " + strings.Join(degraded, ", ")
	}
	if len(incomplete) == 0 && len(degraded) == 0 {
		msg += "; every option complete this run"
	}
	return msg, d, nil
}

// stampNames maps the -stamp-one flag's vocabulary onto the socket options.
var stampNames = map[string]can.Stamp{
	"none":     can.StampNone,
	"ts":       can.StampTimestamp,
	"tsns":     can.StampTimestampNS,
	"tsing":    can.StampTimestamping,
	"tsing-hw": can.StampTimestampingHW,
}

// stampOneMain is the child half of probeTimestamp.
func stampOneMain(iface, name string) error {
	mode, ok := stampNames[name]
	if !ok {
		return fmt.Errorf("unknown timestamp option %q", name)
	}
	n, err := stampCount(iface, mode)
	if err != nil {
		return err
	}
	fmt.Printf("%d/%d\n", n, stampFrames)
	return nil
}

// stampFrames is how many frames each timestamp measurement sends.
const stampFrames = 200

// stampCount reports how many of stampFrames frames came back carrying a
// timestamp under mode, on freshly opened sockets.
func stampCount(iface string, mode can.Stamp) (int, error) {
	rx, err := can.Open(iface, can.Options{Stamp: mode})
	if err != nil {
		return 0, err
	}
	defer done(rx)
	tx, err := can.Open(iface, can.Options{})
	if err != nil {
		return 0, err
	}
	defer done(tx)
	rx.SetReadTimeout(time.Second)

	stamped := 0
	out := can.New(0x321, []byte{1})
	var in can.Frame
	for i := 0; i < stampFrames; i++ {
		if err := tx.Send(out); err != nil {
			return 0, err
		}
		if err := rx.Recv(&in); err != nil {
			return 0, err
		}
		if in.Stamped {
			stamped++
		}
	}
	return stamped, nil
}

// probePacing is the measurement §8 rests on: a userspace timer cannot
// reproduce a 1 ms cycle, and the kernel's BCM can.
func probePacing(iface string) (string, map[string]any, error) {
	const (
		period = time.Millisecond
		want   = 300
	)
	d := map[string]any{"period_ms": 1}

	measure := func(id uint32, start func() error, stop func()) ([]float64, error) {
		rx, err := can.Open(iface, can.Options{Stamp: can.StampTimestampNS, RcvBuf: 1 << 20})
		if err != nil {
			return nil, err
		}
		defer done(rx)
		rx.SetReadTimeout(2 * time.Second)
		if err := start(); err != nil {
			return nil, err
		}
		defer stop()

		var stamps []time.Time
		var in can.Frame
		for len(stamps) < want {
			if err := rx.Recv(&in); err != nil {
				break
			}
			if in.ID != id || !in.Stamped {
				continue
			}
			stamps = append(stamps, in.Time)
		}
		var jit []float64
		for i := 1; i < len(stamps); i++ {
			g := stamps[i].Sub(stamps[i-1]).Seconds()*1e6 - float64(period/time.Microsecond)
			if g < 0 {
				g = -g
			}
			jit = append(jit, g)
		}
		sort.Float64s(jit)
		return jit, nil
	}

	// Userspace, the naive way and the expensive way.
	for _, mode := range []string{"sleep", "sleep+spin"} {
		tx, err := can.Open(iface, can.Options{})
		if err != nil {
			return "", nil, err
		}
		doneCh := make(chan struct{})
		start := func() error {
			go func() {
				defer close(doneCh)
				f := can.New(0x200, make([]byte, 8))
				t0 := time.Now()
				for i := 0; i < want+50; i++ {
					target := t0.Add(time.Duration(i) * period)
					if mode == "sleep" {
						if w := time.Until(target); w > 0 {
							time.Sleep(w)
						}
					} else {
						if w := time.Until(target) - 250*time.Microsecond; w > 0 {
							time.Sleep(w)
						}
						for time.Now().Before(target) {
						}
					}
					if tx.Send(f) != nil {
						return
					}
				}
			}()
			return nil
		}
		jit, err := measure(0x200, start, func() { <-doneCh; done(tx) })
		if err != nil {
			return "", nil, err
		}
		d[mode] = jitterDetail(jit)
	}

	// The kernel's own timer.
	bcm, err := can.OpenBCM(iface)
	if err != nil {
		d["bcm"] = map[string]any{"available": false, "error": err.Error()}
		return "CAN_BCM unavailable; userspace only", d, nil
	}
	defer bcm.Close()
	jit, err := measure(0x777,
		func() error { return bcm.Cyclic(0x777, period, []byte{1, 2, 3, 4, 5, 6, 7, 8}) },
		func() { bcm.Stop(0x777) })
	if err != nil {
		return "", nil, err
	}
	d["bcm"] = jitterDetail(jit)

	sleep := d["sleep"].(map[string]any)
	return fmt.Sprintf("jitter at 1 ms: CAN_BCM p99 %.0f µs against userspace sleep p99 %.0f µs",
		d["bcm"].(map[string]any)["p99_us"], sleep["p99_us"]), d, nil
}

// probeThroughput establishes that the virtual bus is never the bottleneck,
// and therefore that it applies no back-pressure a real bus would.
func probeThroughput(iface string) (string, map[string]any, error) {
	tx, err := can.Open(iface, can.Options{})
	if err != nil {
		return "", nil, err
	}
	defer done(tx)
	// A reader with a large buffer, so the cost measured is transmit.
	rx, err := can.Open(iface, can.Options{RcvBuf: 1 << 22})
	if err != nil {
		return "", nil, err
	}
	rx.SetReadTimeout(200 * time.Millisecond)
	stop := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		var in can.Frame
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := rx.Recv(&in); err != nil {
				return
			}
		}
	}()

	f := can.New(0x300, []byte{1, 2, 3, 4, 5, 6, 7, 8})
	var sent, enobufs int
	t0 := time.Now()
	for time.Since(t0) < 500*time.Millisecond {
		for i := 0; i < 1000; i++ {
			if err := tx.Send(f); err != nil {
				if err == unix.ENOBUFS {
					enobufs++
					continue
				}
				return "", nil, fmt.Errorf("send: %w", err)
			}
			sent++
		}
	}
	el := time.Since(t0)
	close(stop)
	<-drained
	done(rx)

	rate := float64(sent) / el.Seconds()
	// A 500 kbit/s bus carrying maximum-length classic frames tops out near
	// this, which is the number the measurement is worth comparing against.
	const busFramesPerSec = 7000
	return fmt.Sprintf("%.0f frames/s accepted, about %.0f× a saturated 500 kbit/s bus (ENOBUFS %d)",
			rate, rate/busFramesPerSec, enobufs),
		map[string]any{
			"frames_per_sec": int(rate), "enobufs": enobufs,
			"vs_500kbit_bus": round(rate / busFramesPerSec),
		}, nil
}

// probeOverflow is the one that matters most, because §10.5 makes a rule of it:
// a gap that does not say it is a gap is the worst failure this system can
// produce. The kernel reports interior gaps exactly and a lost tail not at all.
func probeOverflow(iface string) (string, map[string]any, error) {
	d := map[string]any{}

	// Case 1: a reader that drains, but slower than the writer fills.
	rx, err := can.Open(iface, can.Options{RcvBuf: 8192, RxqOvfl: true})
	if err != nil {
		return "", nil, err
	}
	tx, err := can.Open(iface, can.Options{})
	if err != nil {
		done(rx)
		return "", nil, err
	}
	stop := make(chan struct{})
	go func() {
		f := can.New(0x100, make([]byte, 8))
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			putSeq(f, uint32(i))
			if tx.Send(f) != nil {
				return
			}
			if i%50 == 0 {
				time.Sleep(50 * time.Microsecond)
			}
		}
	}()

	rx.SetReadTimeout(300 * time.Millisecond)
	var got int
	var lastSeq int64 = -1
	var lostBySeq int64
	var ovfl uint32
	var in can.Frame
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := rx.Recv(&in); err != nil {
			break
		}
		got++
		time.Sleep(20 * time.Microsecond) // the slow consumer
		s := int64(seqOf(&in))
		if lastSeq >= 0 && s > lastSeq+1 {
			lostBySeq += s - lastSeq - 1
		}
		lastSeq = s
		if in.HaveDrops && in.Drops > ovfl {
			ovfl = in.Drops
		}
	}
	close(stop)
	done(rx)
	done(tx)
	d["interior"] = map[string]any{
		"received": got, "lost_by_sequence": lostBySeq, "so_rxq_ovfl": ovfl,
		"agree": int64(ovfl) == lostBySeq,
	}

	// Case 2: a reader that stalls completely, so the loss is all tail.
	rx2, err := can.Open(iface, can.Options{RcvBuf: 8192, RxqOvfl: true})
	if err != nil {
		return "", nil, err
	}
	defer done(rx2)
	tx2, err := can.Open(iface, can.Options{})
	if err != nil {
		return "", nil, err
	}
	defer done(tx2)

	const burst = 20000
	f := can.New(0x101, make([]byte, 8))
	for i := 0; i < burst; i++ {
		putSeq(f, uint32(i))
		tx2.Send(f)
	}
	rx2.SetReadTimeout(300 * time.Millisecond)
	var drained int
	var ovfl2 uint32
	var gaps int
	lastSeq = -1
	for {
		if err := rx2.Recv(&in); err != nil {
			break
		}
		drained++
		s := int64(seqOf(&in))
		if lastSeq >= 0 && s > lastSeq+1 {
			gaps++
		}
		lastSeq = s
		if in.HaveDrops && in.Drops > ovfl2 {
			ovfl2 = in.Drops
		}
	}
	d["lost_tail"] = map[string]any{
		"injected": burst, "drained": drained, "missing": burst - drained,
		"so_rxq_ovfl": ovfl2, "interior_gaps_seen": gaps,
		"silent": ovfl2 == 0 && gaps == 0 && drained < burst,
	}

	return fmt.Sprintf("interior gaps counted exactly (%d = %d); a lost tail of %d frames reported %d",
		ovfl, lostBySeq, burst-drained, ovfl2), d, nil
}

// The overflow probe writes a sequence number into the first four payload
// bytes. It is the independent check on the kernel's own drop counter: §10.5
// turns on the two disagreeing.
func putSeq(f *can.Frame, n uint32) { binary.LittleEndian.PutUint32(f.Data[:4], n) }

func seqOf(f *can.Frame) uint32 { return binary.LittleEndian.Uint32(f.Data[:4]) }

func jitterDetail(jit []float64) map[string]any {
	return map[string]any{
		"n":      len(jit),
		"p50_us": round(pct(jit, 50)),
		"p99_us": round(pct(jit, 99)),
		"max_us": round(pct(jit, 100)),
	}
}

func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	return v[int(p/100*float64(len(v)-1))]
}

func round(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
