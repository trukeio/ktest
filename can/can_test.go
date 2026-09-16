//go:build linux

package can

import (
	"testing"

	"golang.org/x/sys/unix"
)

// The wire layouts are the one thing here that can be checked without an
// interface, and they are what the extraction from cmd/vcanprobe rearranged.

func TestClassicWire(t *testing.T) {
	f := New(0x123, []byte{1, 2, 3, 4, 5, 6, 7, 8})
	if f.Wire != MTU {
		t.Fatalf("an 8-byte frame is classic, got Wire=%d", f.Wire)
	}
	b := f.wire(MTU)
	if len(b) != 16 {
		t.Fatalf("struct can_frame is 16 bytes, got %d", len(b))
	}
	want := []byte{0x23, 0x01, 0, 0, 8, 0, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8}
	for i := range want {
		if b[i] != want[i] {
			t.Fatalf("byte %d: got %#x, want %#x\n got %x\nwant %x", i, b[i], want[i], b, want)
		}
	}
}

func TestFDWire(t *testing.T) {
	pay := make([]byte, 64)
	for i := range pay {
		pay[i] = byte(i)
	}
	f := New(0x456, pay)
	if f.Wire != FDMTU {
		t.Fatalf("a 64-byte payload forces CAN FD, got Wire=%d", f.Wire)
	}
	f.Flags = FlagBRS
	b := f.wire(FDMTU)
	if len(b) != 72 {
		t.Fatalf("struct canfd_frame is 72 bytes, got %d", len(b))
	}
	if b[4] != 64 {
		t.Errorf("len byte: got %d, want 64", b[4])
	}
	if b[5] != FlagBRS {
		t.Errorf("flags byte: got %#x, want %#x", b[5], FlagBRS)
	}
	for i := range pay {
		if b[8+i] != pay[i] {
			t.Fatalf("payload byte %d: got %#x, want %#x", i, b[8+i], pay[i])
		}
	}
}

// A classic frame must not pick up the flags byte, which sits in a reserved
// field of struct can_frame and means nothing there.
func TestClassicHasNoFlagsByte(t *testing.T) {
	f := New(0x100, []byte{1})
	f.Flags = FlagBRS
	if b := f.wire(MTU); b[5] != 0 {
		t.Errorf("byte 5 of a classic frame is reserved, got %#x", b[5])
	}
}

func TestIdentifierFlags(t *testing.T) {
	for _, tc := range []struct {
		name        string
		id          uint32
		ext, rtr, e bool
		arb         uint32
	}{
		{"standard", 0x123, false, false, false, 0x123},
		{"extended", unix.CAN_EFF_FLAG | 0x1ABCDEF, true, false, false, 0x1ABCDEF},
		{"remote", unix.CAN_RTR_FLAG | 0x200, false, true, false, 0x200},
		{"error", unix.CAN_ERR_FLAG | unix.CAN_ERR_BUSOFF, false, false, true, unix.CAN_ERR_BUSOFF},
		// An error class above the 11-bit standard mask: masking an error
		// frame as if it were a standard one would silently truncate it.
		{"error wide", unix.CAN_ERR_FLAG | 0x00010080, false, false, true, 0x00010080},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := Frame{ID: tc.id}
			if f.Extended() != tc.ext || f.RTR() != tc.rtr || f.Err() != tc.e {
				t.Errorf("flags on %#x: ext=%v rtr=%v err=%v",
					tc.id, f.Extended(), f.RTR(), f.Err())
			}
			if got := f.Arbitration(); got != tc.arb {
				t.Errorf("Arbitration(%#x) = %#x, want %#x", tc.id, got, tc.arb)
			}
		})
	}
}

// §8's rule, as a type property: an unstamped frame carries a zero Time, and
// nothing may read that zero as a timestamp. The pairing is what makes the
// rule checkable, so it is asserted rather than assumed.
func TestUnstampedIsNotZeroTime(t *testing.T) {
	var f Frame
	if f.Stamped {
		t.Error("a zero Frame must not claim to be stamped")
	}
	if !f.Time.IsZero() {
		t.Error("a zero Frame's Time must be the zero time")
	}
	if f.HaveDrops {
		t.Error("a zero Frame must not claim the kernel reported drops")
	}
}

func TestSendRejectsOversizedPayload(t *testing.T) {
	c := &Conn{fd: -1, tx: make([]byte, FDMTU)}
	f := Frame{ID: 1, Len: MaxLen + 1}
	if err := c.Send(&f); err == nil {
		t.Error("a payload longer than 64 bytes must be refused, not truncated")
	}
	// Eight bytes is the classic limit; more than that must go out as FD
	// rather than be silently cut down.
	g := New(1, make([]byte, 9))
	if g.Wire != FDMTU {
		t.Errorf("a 9-byte payload needs an FD frame, got Wire=%d", g.Wire)
	}
}
