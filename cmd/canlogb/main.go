//go:build linux

// Command canlogb records a CAN bus to a Logb file.
//
//	canlogb [flags] interface
//
// It is milestone 1's vertical slice: SocketCAN in, Logb out. There is no
// control plane and no streaming transport yet — the file on disk and the bytes
// that will later go on the wire are the same format (§6.1), so the transport
// is an io.Writer away rather than a rewrite.
//
// Nothing here configures an interface. Per §8 interfaces are opened, not
// configured:
//
//	ip link set can0 up type can bitrate 500000
//
// or, for a virtual bus to test against:
//
//	modprobe vcan
//	ip link add dev vcan0 type vcan
//	ip link set up vcan0
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rveen/logb"
	"github.com/rveen/logb/dbc"
	"golang.org/x/sys/unix"

	"github.com/rveen/ktest/can"
	"github.com/rveen/ktest/record"
	"github.com/rveen/ktest/stream"
)

// version is what the recording says wrote it. §11: a measurement from 2027
// should be reconstructable in 2030.
const version = "canlogb 0.1"

func main() {
	var (
		out      = flag.String("o", "", "output file; default is <interface>.logb, - for stdout")
		database = flag.String("dbc", "", "CAN database; decodes frames into signals beside the raw ones")
		codec    = flag.String("codec", "zstd", "DATA frame codec: none, deflate or zstd")
		errs     = flag.Bool("err", true, "receive error frames")
		dur      = flag.Duration("d", 0, "stop after this long; 0 runs until interrupted")
		rcvbuf   = flag.Int("rcvbuf", 1<<20, "SO_RCVBUF, in bytes")
		perFrame = flag.Int("frame", 0, "records per DATA frame (default 4096)")
		segment  = flag.Duration("segment", time.Second, "how often to begin a new segment")
		quiet    = flag.Bool("q", false, "do not report what could not be carried across")
		httpAddr = flag.String("http", "", "also serve the recording live as Logb, e.g. :8080 (§6.1)")
	)
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
	}
	iface := flag.Arg(0)

	// Where the recording lands, decided before the writer is built because
	// the index is keyed to it.
	name := *out
	if name == "" {
		name = iface + ".logb"
	}

	cfg := record.Config{
		Bus:      iface,
		Stamp:    can.StampTimestampNS,
		PerFrame: *perFrame,
		Segment:  *segment,
		Meta:     record.Provenance(version, iface),
	}
	// An index is built as the file is written only when there is a file: a
	// recording going to stdout has no path to key a sidecar to.
	if name != "-" {
		cfg.Path = name
	}
	switch *codec {
	case "zstd":
		cfg.Codec = logb.CodecZstd
	case "deflate":
		cfg.Codec = logb.CodecDeflate
	case "none":
		cfg.Codec = logb.CodecNone
	default:
		fatalf("unknown codec %q", *codec)
	}
	if !*quiet {
		cfg.Warn = func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, "canlogb: %s\n", fmt.Sprintf(format, a...))
		}
	}

	// A bus recording holds frames, not signals. The database is what turns
	// the second into the first, and nothing else in the file can.
	if *database != "" {
		db, err := dbc.ParseFile(*database)
		if err != nil {
			fatalf("%v", err)
		}
		cfg.DB = db
		if !*quiet {
			fmt.Fprintf(os.Stderr, "canlogb: %s: %d messages\n", *database, len(db.Messages))
		}
	}

	// §8's configuration for recording a bus, plus the error classes, which
	// are a policy this command states rather than the package assuming.
	opts := can.Monitor()
	opts.RcvBuf = *rcvbuf
	if *errs {
		opts.ErrMask = unix.CAN_ERR_MASK
	}
	c, err := can.Open(iface, opts)
	if err != nil {
		fatalf("%v", err)
	}
	defer c.Close()

	f := os.Stdout
	if name != "-" {
		if f, err = os.Create(name); err != nil {
			fatalf("%v", err)
		}
	}
	bw := bufio.NewWriterSize(f, 1<<20)

	// The file and the wire carry the same bytes, which is §6.1's premise: the
	// transport is an io.Writer away rather than a second encoding.
	var sink io.Writer = bw
	var hub *stream.Hub
	if *httpAddr != "" {
		hub = stream.New()
		sink = io.MultiWriter(bw, hub)
	}

	w, err := record.New(sink, cfg)
	if err != nil {
		fatalf("%v", err)
	}

	if hub != nil {
		srv := &http.Server{Addr: *httpAddr, Handler: hub.Mux()}
		ln, err := net.Listen("tcp", *httpAddr)
		if err != nil {
			fatalf("%v", err)
		}
		go func() {
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				fmt.Fprintf(os.Stderr, "canlogb: http: %v\n", err)
			}
		}()
		defer srv.Close()
		fmt.Fprintf(os.Stderr, "canlogb: streaming live on http://%s/stream/live\n", ln.Addr())
	}

	// A run has to be closed by something that knows it ended, never by the
	// receive path merely going quiet (§10.5). Here that is a signal or the
	// -d deadline, both of which are external to the socket.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	deadline := make(<-chan time.Time)
	if *dur > 0 {
		deadline = time.After(*dur)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-stop:
		case <-deadline:
		}
		close(done)
	}()

	// A short read timeout is what lets the loop notice `done` and flush on a
	// timer without a second goroutine touching the Writer, which is not safe
	// for concurrent use.
	c.SetReadTimeout(100 * time.Millisecond)
	var fr can.Frame
	lastFlush := time.Now()
loop:
	for {
		select {
		case <-done:
			break loop
		default:
		}
		switch err := c.Recv(&fr); err {
		case nil:
			if err := w.Frame(&fr); err != nil {
				fatalf("write: %v", err)
			}
		case unix.EAGAIN:
			// The read timeout expired with no frame. Not evidence of
			// anything: §10.5 forbids reading a quiet socket as a quiet bus.
		default:
			fatalf("recv: %v", err)
		}
		if time.Since(lastFlush) >= 200*time.Millisecond {
			// The recorder first, which turns buffered records into frames,
			// then the file's own buffer: a consumer of either sees the
			// recording advance rather than arrive in one lump at the end.
			if err := w.Flush(); err != nil {
				fatalf("flush: %v", err)
			}
			if err := bw.Flush(); err != nil {
				fatalf("flush: %v", err)
			}
			lastFlush = time.Now()
		}
	}

	if err := w.Close(); err != nil {
		fatalf("close: %v", err)
	}
	if hub != nil {
		// After the writer, so that subscribers receive the INDEX and END
		// frames and their streams are complete files rather than truncated
		// ones.
		hub.Close()
	}
	if err := bw.Flush(); err != nil {
		fatalf("flush: %v", err)
	}
	if f != os.Stdout {
		if err := f.Close(); err != nil {
			fatalf("close: %v", err)
		}
	}

	// The index is saved after the file is closed, so that it fingerprints
	// what is actually on disk.
	if saved, err := w.SaveIndex(); err != nil {
		// Not fatal: the recording is complete and correct, and a viewer can
		// still scan it. But a silent failure here is indistinguishable from
		// an index that quietly never works.
		fmt.Fprintf(os.Stderr, "canlogb: the index was not saved (%v); opening this file will scan it\n", err)
	} else if saved && !*quiet {
		fmt.Fprintf(os.Stderr, "canlogb: index written; opening %s costs no scan\n", name)
	}

	s := w.Stats()
	fmt.Fprintf(os.Stderr, "canlogb: %s: %d frames", name, s.Frames)
	if s.Decoded > 0 || s.Unknown > 0 {
		fmt.Fprintf(os.Stderr, ", %d decoded, %d not in the database", s.Decoded, s.Unknown)
	}
	fmt.Fprintln(os.Stderr)
	// Both of these are silence made visible, which is the whole point of
	// §10.5 and of §8's timestamp rule. They are reported even when zero is
	// the answer, because "no drops reported" and "no drops" differ.
	fmt.Fprintf(os.Stderr, "canlogb: %d frames unstamped, %d frames the kernel admitted losing",
		s.Unstamped, s.Dropped)
	if n := c.Interrupts(); n > 0 {
		fmt.Fprintf(os.Stderr, ", %d interrupted reads retried", n)
	}
	fmt.Fprintln(os.Stderr)
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "canlogb: %s\n", fmt.Sprintf(format, a...))
	os.Exit(1)
}

func usage() {
	fmt.Fprint(os.Stderr, strings.TrimLeft(`
canlogb records a CAN bus to a Logb file.

It does not configure interfaces. Bring one up first:

    ip link set can0 up type can bitrate 500000

or, for a virtual bus:

    modprobe vcan
    ip link add dev vcan0 type vcan
    ip link set up vcan0

then record:

    canlogb vcan0                       to vcan0.logb, until interrupted
    canlogb -d 10s -o run.logb vcan0    for ten seconds
    canlogb -dbc car.dbc can0           decoding signals beside the raw frames

Flags:
`, "\n"))
	flag.PrintDefaults()
	os.Exit(2)
}
