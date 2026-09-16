//go:build linux

// Package can is the SocketCAN receive and transmit path.
//
// It exists so that the socket rules §8 of doc/test-software-outline.md settled
// by measurement are implemented once. Three of them are not obvious, and each
// one loses frames silently when it is missed:
//
//   - A blocking recvmsg must retry EINTR. Go's async preemption sends SIGURG
//     to a running goroutine, which interrupts the syscall; a reader without
//     the retry drops those frames and is never told.
//   - A zero timestamp is not a timestamp. SO_TIMESTAMPING was measured
//     returning zeroed stamps on frames it had accepted, so Frame.Stamped is a
//     separate flag rather than a Time.IsZero() test, and the caller is
//     required to look at it.
//   - The absence of an overflow report is not evidence of no loss. SO_RXQ_OVFL
//     rides on a delivered packet, so a reader that stalls outright is told
//     nothing: a measured overrun that lost 19 982 of 20 000 frames reported
//     zero everywhere the kernel was willing to look. HaveDrops says whether
//     the kernel spoke, which is not the same as it saying zero.
//
// Nothing here configures an interface. Per §8 interfaces are opened, not
// configured: the operator brings one up with ip(8) first.
package can

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Wire sizes and flags that x/sys/unix does not export.
//
// A classic can_frame is 16 bytes:
//
//	can_id u32 | len u8 | pad u8 | res0 u8 | len8_dlc u8 | data[8]
//
// A canfd_frame is 72:
//
//	can_id u32 | len u8 | flags u8 | res0 u8 | res1 u8 | data[64]
//
// The first eight bytes are laid out compatibly, which is why one receive
// buffer serves both and the returned byte count is what tells them apart.
const (
	MTU   = 16 // sizeof(struct can_frame)
	FDMTU = 72 // sizeof(struct canfd_frame)

	MaxLen = 64 // the largest CAN FD payload

	FlagBRS = 0x01 // canfd_frame.flags: bit rate switch
	FlagESI = 0x02 // canfd_frame.flags: error state indicator
)

// Stamp names a receive-timestamp socket option.
//
// They are not interchangeable, which is why this is a choice rather than a
// bool: on vcan, SO_TIMESTAMPNS stamped 200 frames of 200 on every run while
// SO_TIMESTAMPING returned zeroed stamps in a process that had already opened
// and closed other timestamping sockets. cmd/vcanprobe measures this.
type Stamp int

const (
	StampNone Stamp = iota
	StampTimestamp
	StampTimestampNS
	StampTimestamping
	StampTimestampingHW
)

func (s Stamp) String() string {
	switch s {
	case StampTimestamp:
		return "SO_TIMESTAMP"
	case StampTimestampNS:
		return "SO_TIMESTAMPNS"
	case StampTimestamping:
		return "SO_TIMESTAMPING(sw)"
	case StampTimestampingHW:
		return "SO_TIMESTAMPING(sw+hw)"
	}
	return "none"
}

// Options are the socket options to set before bind. They are applied in order
// and any failure is returned rather than ignored, because a silently unset
// option is the failure mode this package exists to avoid.
type Options struct {
	FD      bool   // CAN_RAW_FD_FRAMES: also accept 64-byte frames
	ErrMask uint32 // CAN_RAW_ERR_FILTER: which error classes to receive
	RcvBuf  int    // SO_RCVBUF, 0 for the system default
	RxqOvfl bool   // SO_RXQ_OVFL: report drops as a control message
	OwnMsgs bool   // CAN_RAW_RECV_OWN_MSGS
	Stamp   Stamp
}

// Monitor is the configuration §8 settles on for recording a bus: every frame
// including FD, kernel timestamps from the option that was measured to deliver
// them, drops reported, and no echo of the daemon's own transmissions.
//
// Error frames are off. A recording wants them, but which classes is a policy
// the caller states; ErrMask = unix.CAN_ERR_MASK takes all of them.
func Monitor() Options {
	return Options{FD: true, RxqOvfl: true, Stamp: StampTimestampNS}
}

// A Frame is one CAN frame and whatever the kernel attached to it.
//
// Data is an array rather than a slice so that a receive loop at full bus load
// allocates nothing. Payload returns the part of it that the frame carried.
type Frame struct {
	// ID is the raw can_id as the kernel presents it, flag bits included.
	// Arbitration masks them off; for an error frame the same bits are the
	// error class.
	ID    uint32
	Len   int
	Flags byte // canfd_frame.flags, valid only when FD reports true
	Data  [MaxLen]byte

	// Wire is what recvmsg returned: MTU for a classic frame, FDMTU for an
	// FD one. It is how the two are told apart, so it is kept rather than
	// reduced to a bool.
	Wire int

	// Time is the kernel's receive timestamp, valid only if Stamped. See the
	// package comment: this pair is deliberately not a Time.IsZero() test.
	Time    time.Time
	Stamped bool

	// Local is MSG_DONTROUTE: the frame was sent from this host. Measured on
	// vcan, a monitoring socket sees cansend's frames, CAN_BCM's and a
	// daemon's own all flagged alike, so it says where a frame came from and
	// never who sent it.
	Local bool

	// Confirmed is MSG_CONFIRM: this socket sent the frame and the kernel has
	// handed it back. Only a socket opened with OwnMsgs receives these. On an
	// interface that echoes on transmit completion it is the controller's word
	// that the frame left; vcan echoes at once, so there it proves less.
	Confirmed bool

	// Drops is the cumulative SO_RXQ_OVFL counter as of this frame, valid
	// only if HaveDrops. HaveDrops false means the kernel said nothing, which
	// is not the same as it saying zero.
	Drops     uint32
	HaveDrops bool
}

func (f *Frame) Extended() bool { return f.ID&unix.CAN_EFF_FLAG != 0 }
func (f *Frame) RTR() bool      { return f.ID&unix.CAN_RTR_FLAG != 0 }
func (f *Frame) Err() bool      { return f.ID&unix.CAN_ERR_FLAG != 0 }
func (f *Frame) FD() bool       { return f.Wire == FDMTU }
func (f *Frame) BRS() bool      { return f.FD() && f.Flags&FlagBRS != 0 }
func (f *Frame) ESI() bool      { return f.FD() && f.Flags&FlagESI != 0 }

// Arbitration is the identifier with the flag bits removed. For an error frame
// these bits are the error class instead, so check Err first — and the wider
// mask applies there too, because an error frame carries its classes in the
// same 29 bits an extended identifier uses, without setting CAN_EFF_FLAG.
func (f *Frame) Arbitration() uint32 {
	if f.Extended() || f.Err() {
		return f.ID & unix.CAN_EFF_MASK
	}
	return f.ID & unix.CAN_SFF_MASK
}

// Payload is the bytes the frame carried. It aliases the Frame, so a receive
// loop that keeps it must copy it.
func (f *Frame) Payload() []byte { return f.Data[:f.Len] }

// A Conn is a bound CAN_RAW socket.
//
// Send and Recv may be called from different goroutines: they share the
// descriptor and nothing else. Two concurrent calls to either are not safe.
type Conn struct {
	fd  int
	buf []byte
	oob []byte
	tx  []byte

	// eintrs counts how often a blocking recvmsg was interrupted and retried.
	// It is reported rather than hidden: a nonzero count on a reader without
	// the retry loop is exactly that many frames lost in silence.
	eintrs uint64
}

// Open binds a CAN_RAW socket to an interface.
func Open(iface string, o Options) (*Conn, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("interface %s: %w", iface, err)
	}
	fd, err := unix.Socket(unix.AF_CAN, unix.SOCK_RAW, unix.CAN_RAW)
	if err != nil {
		return nil, fmt.Errorf("socket(AF_CAN): %w", err)
	}
	fail := func(what string, err error) (*Conn, error) {
		unix.Close(fd)
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if o.FD {
		if err := unix.SetsockoptInt(fd, unix.SOL_CAN_RAW, unix.CAN_RAW_FD_FRAMES, 1); err != nil {
			return fail("CAN_RAW_FD_FRAMES", err)
		}
	}
	if o.OwnMsgs {
		if err := unix.SetsockoptInt(fd, unix.SOL_CAN_RAW, unix.CAN_RAW_RECV_OWN_MSGS, 1); err != nil {
			return fail("CAN_RAW_RECV_OWN_MSGS", err)
		}
	}
	if o.ErrMask != 0 {
		if err := unix.SetsockoptInt(fd, unix.SOL_CAN_RAW, unix.CAN_RAW_ERR_FILTER, int(o.ErrMask)); err != nil {
			return fail("CAN_RAW_ERR_FILTER", err)
		}
	}
	if o.RcvBuf > 0 {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, o.RcvBuf); err != nil {
			return fail("SO_RCVBUF", err)
		}
	}
	if o.RxqOvfl {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RXQ_OVFL, 1); err != nil {
			return fail("SO_RXQ_OVFL", err)
		}
	}
	switch o.Stamp {
	case StampTimestamp:
		err = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TIMESTAMP, 1)
	case StampTimestampNS:
		err = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TIMESTAMPNS, 1)
	case StampTimestamping:
		err = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TIMESTAMPING,
			unix.SOF_TIMESTAMPING_RX_SOFTWARE|unix.SOF_TIMESTAMPING_SOFTWARE)
	case StampTimestampingHW:
		err = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TIMESTAMPING,
			unix.SOF_TIMESTAMPING_RX_SOFTWARE|unix.SOF_TIMESTAMPING_SOFTWARE|
				unix.SOF_TIMESTAMPING_RX_HARDWARE|unix.SOF_TIMESTAMPING_RAW_HARDWARE)
	}
	if err != nil {
		return fail(o.Stamp.String(), err)
	}
	if err := unix.Bind(fd, &unix.SockaddrCAN{Ifindex: ifi.Index}); err != nil {
		return fail("bind", err)
	}
	return &Conn{
		fd:  fd,
		buf: make([]byte, FDMTU),
		oob: make([]byte, 512),
		tx:  make([]byte, FDMTU),
	}, nil
}

func (c *Conn) Close() error { return unix.Close(c.fd) }

// SetReadTimeout bounds a blocking Recv. A timed-out Recv returns
// unix.EAGAIN, which the caller distinguishes from a real error.
func (c *Conn) SetReadTimeout(d time.Duration) error {
	tv := unix.Timeval{
		Sec:  int64(d / time.Second),
		Usec: int64((d % time.Second) / time.Microsecond),
	}
	return unix.SetsockoptTimeval(c.fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
}

// RcvBuf is the receive buffer size the kernel actually granted, which is not
// necessarily what SO_RCVBUF asked for.
func (c *Conn) RcvBuf() int {
	n, _ := unix.GetsockoptInt(c.fd, unix.SOL_SOCKET, unix.SO_RCVBUF)
	return n
}

// Interrupts is how many blocking receives were interrupted and retried.
func (c *Conn) Interrupts() uint64 { return c.eintrs }

// Recv fills f with the next frame. It retries EINTR; see the package comment
// for why that is not optional.
//
// f is fully overwritten on success, including Stamped and HaveDrops, so a
// caller may reuse one Frame across a whole receive loop.
func (c *Conn) Recv(f *Frame) error {
	var n, oobn, flags int
	var err error
	for {
		n, oobn, flags, _, err = unix.Recvmsg(c.fd, c.buf, c.oob, 0)
		if err != unix.EINTR {
			break
		}
		c.eintrs++
	}
	if err != nil {
		return err
	}
	if n < MTU {
		return fmt.Errorf("can: short read: %d bytes, want at least %d", n, MTU)
	}

	*f = Frame{
		ID:        binary.LittleEndian.Uint32(c.buf[0:4]),
		Len:       int(c.buf[4]),
		Flags:     c.buf[5],
		Wire:      n,
		Local:     flags&unix.MSG_DONTROUTE != 0,
		Confirmed: flags&unix.MSG_CONFIRM != 0,
	}
	if f.Len > MaxLen || (n == MTU && f.Len > 8) {
		return fmt.Errorf("can: frame claims %d bytes in a %d-byte read", f.Len, n)
	}
	copy(f.Data[:], c.buf[8:8+f.Len])

	msgs, err := unix.ParseSocketControlMessage(c.oob[:oobn])
	if err != nil {
		// The frame is good; only its ancillary data is not. Reporting no
		// timestamp is honest and lets the caller apply its own policy.
		return nil
	}
	for _, m := range msgs {
		if m.Header.Level != unix.SOL_SOCKET {
			continue
		}
		switch m.Header.Type {
		case unix.SO_TIMESTAMPING, unix.SCM_TIMESTAMPNS:
			// scm_timestamping is three timespecs: software, deprecated,
			// raw hardware. Only the first is read here.
			if len(m.Data) >= int(unsafe.Sizeof(unix.Timespec{})) {
				t := (*unix.Timespec)(unsafe.Pointer(&m.Data[0]))
				if t.Sec != 0 || t.Nsec != 0 {
					f.Time, f.Stamped = time.Unix(t.Sec, t.Nsec), true
				}
			}
		case unix.SCM_TIMESTAMP:
			if len(m.Data) >= int(unsafe.Sizeof(unix.Timeval{})) {
				t := (*unix.Timeval)(unsafe.Pointer(&m.Data[0]))
				if t.Sec != 0 || t.Usec != 0 {
					f.Time, f.Stamped = time.Unix(t.Sec, t.Usec*1000), true
				}
			}
		case unix.SO_RXQ_OVFL:
			if len(m.Data) >= 4 {
				f.Drops = *(*uint32)(unsafe.Pointer(&m.Data[0]))
				f.HaveDrops = true
			}
		}
	}
	return nil
}

// Send writes one frame. A frame is sent as CAN FD if it says so through Wire
// or carries more than eight bytes; otherwise it goes out classic.
func (c *Conn) Send(f *Frame) error {
	n := MTU
	if f.Wire == FDMTU || f.Len > 8 || f.Flags != 0 {
		n = FDMTU
	}
	if f.Len > MaxLen || (n == MTU && f.Len > 8) {
		return fmt.Errorf("can: %d-byte payload does not fit a %d-byte frame", f.Len, n)
	}
	b := c.tx[:n]
	clear(b)
	f.marshal(b)
	_, err := unix.Write(c.fd, b)
	return err
}

// marshal lays the frame out in the kernel's struct, classic or FD according
// to len(b). It assumes b is zeroed and long enough.
func (f *Frame) marshal(b []byte) {
	binary.LittleEndian.PutUint32(b[0:4], f.ID)
	b[4] = byte(f.Len)
	if len(b) == FDMTU {
		b[5] = f.Flags
	}
	copy(b[8:], f.Data[:f.Len])
}

// wire returns the frame as an n-byte kernel struct. Send marshals in place
// instead; this is for the callers that hand a frame to something other than
// a socket write, such as CAN_BCM.
func (f *Frame) wire(n int) []byte {
	b := make([]byte, n)
	f.marshal(b)
	return b
}

// New builds a frame for transmission. id may carry CAN_EFF_FLAG or
// CAN_ERR_FLAG; a payload longer than eight bytes makes it a CAN FD frame.
func New(id uint32, data []byte) *Frame {
	f := &Frame{ID: id, Len: len(data), Wire: MTU}
	if len(data) > 8 {
		f.Wire = FDMTU
	}
	copy(f.Data[:], data)
	return f
}
