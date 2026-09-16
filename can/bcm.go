//go:build linux

package can

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// CAN_BCM is the kernel's broadcast manager: cyclic transmit paced by a kernel
// timer rather than by the daemon's event loop. §8 rests on the difference —
// measured on vcan, a userspace 1 ms cycle jitters at p99 983 µs where BCM
// holds p99 near zero — which is why §3 puts timing below the daemon.
//
// Measured on vcan, and relied on:
//
//   - A task lives exactly as long as the socket that set it up. Closing the
//     socket stopped a 10 ms task at once, and so did SIGKILL on the process
//     that owned it: nine frames in the 100 ms before, none after. A daemon
//     that dies takes its cyclic traffic with it, which is §7's disconnect
//     policy for an active transmit, provided by the kernel.
//   - TX_SETUP on a running task without SETTIMER replaces its data and keeps
//     its timer: the interval across the change stayed at 10.0 ms.
//   - TX_READ is answered with TX_STATUS, the kernel's own account of the
//     task, and deleting or reading a task that does not exist fails EINVAL.
//   - A counted task with TX_COUNTEVT reports TX_EXPIRED once its last frame
//     has gone out: three frames at 10 ms, and the report after 20 ms.

// struct bcm_msg_head on a 64-bit kernel:
//
//	 0 opcode   u32
//	 4 flags    u32
//	 8 count    u32
//	12 (pad, because bcm_timeval needs 8-byte alignment)
//	16 ival1    {sec i64, usec i64}
//	32 ival2    {sec i64, usec i64}
//	48 can_id   u32
//	52 nframes  u32
//	56 frames[]
const (
	bcmHeadLen = 56

	bcmTXSetup  = 1 // create or update a transmit task
	bcmTXDelete = 2 // remove it
	bcmTXRead   = 3 // ask for its TX_STATUS

	bcmSetTimer   = 0x0001
	bcmStartTimer = 0x0002
	bcmCountEvt   = 0x0004
	bcmFDFrame    = 0x0800
)

// A BCMOp is the opcode of a message the kernel sent to a BCM socket.
type BCMOp uint32

const (
	BCMStatus  BCMOp = 8 // TX_STATUS: the answer to a TX_READ
	BCMExpired BCMOp = 9 // TX_EXPIRED: a counted task sent its last frame
)

// A Task is one cyclic transmit task.
//
// Count frames go out Ival1 apart, and then the frame repeats every Ival2
// until the task is deleted. A task that runs until stopped has Count 0 and
// Ival2 set; a counted one that then stops has Ival2 0.
type Task struct {
	Frame Frame // ID carries CAN_EFF_FLAG; Wire says classic or FD
	Count uint32
	Ival1 time.Duration
	Ival2 time.Duration
}

// A BCMMsg is one message the kernel sent to a BCM socket.
type BCMMsg struct {
	Op   BCMOp
	ID   uint32 // the task's can_id, flag bits included
	FD   bool   // the task is a CAN FD one
	Task Task   // what the kernel holds, for BCMStatus
}

// A BCM is a CAN_BCM socket connected to one interface.
//
// Writes and Recv may happen on different goroutines: they share the
// descriptor and nothing else.
type BCM struct {
	fd  int
	buf []byte
}

func OpenBCM(iface string) (*BCM, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Socket(unix.AF_CAN, unix.SOCK_DGRAM, unix.CAN_BCM)
	if err != nil {
		return nil, fmt.Errorf("socket(CAN_BCM): %w", err)
	}
	if err := unix.Connect(fd, &unix.SockaddrCAN{Ifindex: ifi.Index}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("connect(CAN_BCM): %w", err)
	}
	return &BCM{fd: fd, buf: make([]byte, 4096)}, nil
}

// Close ends the socket, and with it every task it set up.
func (b *BCM) Close() error { return unix.Close(b.fd) }

// SetReadTimeout bounds a blocking Recv, which then returns unix.EAGAIN.
func (b *BCM) SetReadTimeout(d time.Duration) error {
	tv := unix.Timeval{
		Sec:  int64(d / time.Second),
		Usec: int64((d % time.Second) / time.Microsecond),
	}
	return unix.SetsockoptTimeval(b.fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
}

// Setup creates a task or changes one. restart sets and starts its timer.
// Without it, a running task keeps its timer and only its data changes, which
// is what makes an update seamless; its count and intervals are then ignored.
//
// An interval finer than a microsecond is refused rather than truncated:
// bcm_timeval carries microseconds, and the kernel would run a different
// period from the one the caller asked for.
func (b *BCM) Setup(t *Task, restart bool) error {
	if t.Ival1%time.Microsecond != 0 || t.Ival2%time.Microsecond != 0 {
		return fmt.Errorf("can: CAN_BCM intervals are whole microseconds, not %v and %v", t.Ival1, t.Ival2)
	}
	var flags uint32
	if restart {
		flags |= bcmSetTimer | bcmStartTimer
	}
	// Every setup states the flags afresh, so a counted task that is updated
	// has to ask for its expiry report again or it would lose it.
	if t.Count > 0 {
		flags |= bcmCountEvt
	}
	n := MTU
	if t.Frame.FD() {
		flags |= bcmFDFrame
		n = FDMTU
	}
	if t.Frame.Len > MaxLen || (n == MTU && t.Frame.Len > 8) {
		return fmt.Errorf("can: %d-byte payload does not fit a %d-byte frame", t.Frame.Len, n)
	}
	m := bcmHead(bcmTXSetup, flags, t.Count, t.Ival1, t.Ival2, t.Frame.ID, 1, n)
	t.Frame.marshal(m[bcmHeadLen:])
	_, err := unix.Write(b.fd, m)
	return err
}

// Delete removes a task, stopping it. fd must say what the task was set up
// as, since the kernel is told CAN_FD_FRAME again.
func (b *BCM) Delete(id uint32, fd bool) error {
	_, err := unix.Write(b.fd, bcmHead(bcmTXDelete, fdFlag(fd), 0, 0, 0, id, 0, 0))
	return err
}

// Read asks for a task's TX_STATUS, which arrives through Recv. It fails with
// EINVAL, and nothing arrives, when there is no such task.
func (b *BCM) Read(id uint32, fd bool) error {
	_, err := unix.Write(b.fd, bcmHead(bcmTXRead, fdFlag(fd), 0, 0, 0, id, 0, 0))
	return err
}

// Recv reads the next message the kernel sent this socket. It retries EINTR,
// as every blocking receive here must; see the package comment.
func (b *BCM) Recv(m *BCMMsg) error {
	var n int
	var err error
	for {
		n, err = unix.Read(b.fd, b.buf)
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		return err
	}
	if n < bcmHeadLen {
		return fmt.Errorf("can: short BCM message: %d bytes", n)
	}
	h := b.buf
	le := binary.LittleEndian
	flags := le.Uint32(h[4:])
	*m = BCMMsg{
		Op: BCMOp(le.Uint32(h[0:])),
		ID: le.Uint32(h[48:]),
		FD: flags&bcmFDFrame != 0,
		Task: Task{
			Count: le.Uint32(h[8:]),
			Ival1: timeval(h[16:]),
			Ival2: timeval(h[32:]),
		},
	}
	if le.Uint32(h[52:]) == 0 {
		return nil
	}
	fl := MTU
	if m.FD {
		fl = FDMTU
	}
	if n < bcmHeadLen+fl {
		return fmt.Errorf("can: BCM message of %d bytes is short of its frame", n)
	}
	w := h[bcmHeadLen : bcmHeadLen+fl]
	f := &m.Task.Frame
	*f = Frame{ID: le.Uint32(w[0:]), Len: int(w[4]), Wire: fl}
	if fl == FDMTU {
		f.Flags = w[5]
	}
	if f.Len > MaxLen || (fl == MTU && f.Len > 8) {
		return fmt.Errorf("can: BCM frame claims %d bytes in a %d-byte frame", f.Len, fl)
	}
	copy(f.Data[:], w[8:8+f.Len])
	return nil
}

// Cyclic transmits a classic frame every period, indefinitely. It is the
// simple case, and what cmd/vcanprobe's pacing probe uses.
func (b *BCM) Cyclic(id uint32, period time.Duration, data []byte) error {
	return b.Setup(&Task{Frame: *New(id, data), Ival2: period}, true)
}

// Stop removes a classic task set up by Cyclic.
func (b *BCM) Stop(id uint32) error { return b.Delete(id, false) }

func bcmHead(op, flags, count uint32, ival1, ival2 time.Duration, id, nframes uint32, frameLen int) []byte {
	m := make([]byte, bcmHeadLen+int(nframes)*frameLen)
	le := binary.LittleEndian
	le.PutUint32(m[0:], op)
	le.PutUint32(m[4:], flags)
	le.PutUint32(m[8:], count)
	le.PutUint64(m[16:], uint64(ival1/time.Second))
	le.PutUint64(m[24:], uint64((ival1%time.Second)/time.Microsecond))
	le.PutUint64(m[32:], uint64(ival2/time.Second))
	le.PutUint64(m[40:], uint64((ival2%time.Second)/time.Microsecond))
	le.PutUint32(m[48:], id)
	le.PutUint32(m[52:], nframes)
	return m
}

func timeval(b []byte) time.Duration {
	le := binary.LittleEndian
	return time.Duration(le.Uint64(b[0:]))*time.Second + time.Duration(le.Uint64(b[8:]))*time.Microsecond
}

func fdFlag(fd bool) uint32 {
	if fd {
		return bcmFDFrame
	}
	return 0
}
