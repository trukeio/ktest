//go:build linux

// Command ktestd is the rack daemon. It owns a bus, records it, and puts
// frames on it when asked.
//
//	ktestd [flags] interface
//
// Milestone 2 starts here because it is where a program first needs a control
// plane (§12). canlogb records and does not actuate, and stays that way; ktestd
// is built on the same packages — can/, record/, stream/ — with the control
// plane above them.
//
// # Control in, results out
//
// Commands go in over REST and results come back only on the stream (§3, §6).
// A request is accepted or refused, and the answer carries a sequence number
// and nothing else: what it did is in the recording. A single send is recorded
// in three streams that answer three different questions (§4):
//
//	<bus>.tx.set      what the daemon was asked to send, and when it acted
//	<bus>.tx.applied  each of those frames, handed back by the kernel as sent
//	<bus>.raw         what was on the bus, this daemon's frames among them
//
// # Leases, arming and limits
//
// The browser is an actuator (§7), so nothing is sent by default. A client
// takes the bus's lease, which gives it a token and a term, and must present
// the token with every request and renew the lease before the term runs out.
// It then arms the bus, and only an armed bus takes sends and cyclic tasks.
// When the lease is released or lapses — the client finished, or crashed and
// stopped renewing — the bus is disarmed and every cyclic task stopped, which
// is §7's rule that an active transmit does not outlive its client. Anyone who
// can reach the socket may disarm, lease or no: stopping is always safe.
//
// What a client may do even with the lease is bounded by limits read once, at
// start, from -limits, or built in when there is no file: a floor on cyclic
// periods, to begin with. The limits are written into the recording, and every
// decision the control plane takes, refusals included, is recorded in
// <bus>.control, whose held fields say at every segment whether the bus is
// armed and who holds it.
//
//	curl --unix-socket /run/ktest.sock -d '{"holder":"bench script"}' http://ktest/bus/vcan0/lease
//	curl --unix-socket /run/ktest.sock -H 'Ktest-Lease: <token>' -X POST http://ktest/bus/vcan0/arm
//	curl --unix-socket /run/ktest.sock -H 'Ktest-Lease: <token>' -d '{"id":"0x123","data":"DEADBEEF"}' \
//	    http://ktest/bus/vcan0/send
//
// # Signals
//
// With -dbc, a send or a cyclic task may name a message and its signals in
// physical units instead of an identifier and bytes. The frame is encoded with
// the database the recording embeds and decodes the bus with:
//
//	curl ... -d '{"message":"EngineData","signals":{"EngineSpeed":3000,"Gear":"Reverse"}}' \
//	    http://ktest/bus/vcan0/send
//
// A signal left out takes the database's start value, and one the database
// gives none must be named. A value is rounded to the nearest step the signal
// represents, and refused outside the range the database declares.
//
// # Cyclic transmit
//
// Cyclic transmit is the kernel's: CAN_BCM paces it on a kernel timer, below
// the daemon (§3, §8). A task is named by its identifier as candump writes it,
// and PUT starts it or changes it:
//
//	curl ... -H 'Ktest-Lease: <token>' -X PUT -d '{"data":"0102","period":"10ms"}' \
//	    http://ktest/bus/vcan0/cyclic/123
//	curl ... -H 'Ktest-Lease: <token>' -X DELETE http://ktest/bus/vcan0/cyclic/123
//
// Each task has its own stream, <bus>.cyclic.<id>, recording each change with
// the kernel's own account of the task beside the request. Its fields are held,
// so every segment restates them and a consumer joining mid-run sees what is
// transmitting now. A task lives exactly as long as the daemon's BCM socket: if
// the daemon dies, the kernel stops every task it had.
//
// # Supplies
//
// -psu drives a bench supply over SCPI, as many as are named. Each is an
// instrument with its own lease and its own arming, taken and given as the
// bus's are, so a client driving a supply does not hold the bus. Its channels
// are written one at a time, each under the supply's lease and only while it
// is armed, and never above the limit the limits file sets for the channel:
//
//	ktestd -psu psu1=192.168.1.20:5025,2 -limits limits.json can0
//	curl ... -d '{}' http://ktest/psu/psu1/lease
//	curl ... -H 'Ktest-Lease: <token>' -X POST http://ktest/psu/psu1/arm
//	curl ... -H 'Ktest-Lease: <token>' -X PUT -d '{"value":5}' http://ktest/channels/psu1.ch1.v.set
//	curl ... -H 'Ktest-Lease: <token>' -X PUT -d '{"value":true}' http://ktest/channels/psu1.ch1.out.set
//
// A supply is recorded in three accounts (§4), kept apart. <supply>.ch<N>
// holds, beside each other, what a client asked for and what the supply
// reports holding after it — the supply clamps a setpoint beyond its rating,
// and says so only in its error queue, which is recorded too. <supply>.ch<N>.meas
// is what it measures at its terminals, polled. <supply>.control is the
// control plane's account, as <bus>.control is the bus's. The connection is
// owned by a goroutine of its own, so a supply that is slow or has gone quiet
// holds up nothing else; a write is answered once the supply has answered.
//
// Disarming a supply switches its outputs off, and so does the stop, POST
// /disarm, which disarms every instrument at once and which, like a disarm,
// anyone may ask for. A lease released or run out, or the daemon stopping,
// only disarms: outputs stay as they were, since cutting what a supply feeds
// is itself an act, and one no client asked for.
//
// # Scopes
//
// -scope drives an oscilloscope over SCPI, as many as are named. Each is an
// instrument with its own lease and its own arming, as a supply is. It is
// driven from a goroutine of its own — arm, wait for the acquisition to
// complete, read every displayed channel, again — so a readout of tens of
// megabytes never holds up the loop that records the bus.
//
//	ktestd -scope scope1=192.168.1.30:5025,2,full=ch1 -limits limits.json can0
//	curl ... -H 'Ktest-Lease: <token>' -X PUT -d '{"value":0.5}' //	    http://ktest/channels/scope1.ch1.vert.vdiv.set
//	curl ... -H 'Ktest-Lease: <token>' -X PUT -d '{"value":0.001}' //	    http://ktest/channels/scope1.timebase.tdiv.set
//
// Acquisition is not gated by arming: a scope is an input, and reading one
// harms nothing. Arming gates its settings, which are writes like any other
// (§4), and the stop leaves the instrument exactly as it is — stopping an
// acquisition is not the kind of thing a stop exists for (§7).
//
// What a scope records, per channel:
//
//	<scope>.ch<N>.env    a min/max envelope of each acquisition, ~3000 columns
//	<scope>.ch<N>        every sample, f32 volts, for a channel full= names
//	<scope>.ch<N>.vert   the volts a division and the offset, set beside applied
//	<scope>.timebase     the seconds a division, the delay and the depth, likewise
//	<scope>.acq          one record per channel per trigger, whatever became of it
//
// A sample is f32 volts (§13). The alternative was the digitiser's 8-bit code
// with the conversion in the schema — compact, and exactly what was measured —
// which makes every volts/div change a schema change and so a new stream
// identity, and §4 rests on a channel's name meaning one thing for a whole
// session. The code is recoverable: the conversion is stated per acquisition,
// in the RUN frame and in <scope>.acq.
//
// The samples are on a uniform axis with no axis bytes in the record, one
// run_id per acquisition, and the settings the acquisition was taken under in
// the RUN frame's parameters (§5.4). A change of the sample interval opens a
// new segment, because axis_step lives in the schema and a schema is restated
// only at a segment boundary (§5.2).
//
// The envelope goes to the recording and to the rack; full rate goes to the
// recording alone, and only for a channel the configuration names, because
// continuous full-rate capture is a decision someone made rather than a
// surprise disk fill overnight (§10.4). The rack may fall behind, and an
// acquisition it cannot take is dropped rather than disconnecting it — and is
// recorded in <scope>.acq as a drop, because a gap that does not say it is a
// gap is the worst failure this system can produce (§10.5).
//
// Two readings of Siglent's programming guide change the numbers and are
// settings rather than constants: -scope-sign, how a code above 127 becomes
// negative, and -scope-trigger-delay, whether TRDL shifts the time axis. Both
// are written into the recording and into every acquisition. See package scope.
//
// # Access
//
// Two listeners carry the control plane. The Unix socket, created 0660, is the
// local one, and its file's owner and group are its access control. -https is
// the network one (§7): TLS, and a named token on every request —
// Authorization: Bearer — checked against a file of token hashes. Each name
// has a role: view may read the streams and disarm, operate may also lease,
// arm and transmit. Over TLS a lease belongs to the name that took it, so a
// lease token copied out of one client does not let another drive the bus
// under it, and every decision is recorded with the name and the listener
// that asked.
//
// Without -tls-cert and -tls-key the daemon makes a self-signed certificate
// once, keeps it in its state directory, and prints its fingerprint for
// clients to pin. -new-token makes a token and the line that admits it.
//
//	ktestd -new-token rolf:operate >> tokens
//	ktestd -https :8443 -tokens tokens vcan0
//	curl --cacert ~/.local/state/ktestd/tls-cert.pem -H 'Authorization: Bearer <token>' \
//	    https://rack:8443/stream/live > run.logb
//
// -http serves the recording read-only over plain TCP, as canlogb does,
// unauthenticated. It carries no control endpoint.
//
// # The rack
//
// The network listener also serves the rack, the browser UI built in web/. It
// asks for a token, then reads /stream/rack — the same records as the
// recording, uncompressed, for the browser to decode as they arrive (§10.3) —
// and lays out the panel given with -panel: widgets on a grid, each bound to
// channels by name. The panel is embedded in the recording beside the limits,
// so a recording brings back the rack that watched it (§10.1). Without a panel
// the rack lists every channel the stream carries.
//
//	ktestd -https :8443 -tokens tokens -dbc car.dbc -panel bench.json can0
//
// The rack edits its panel as well: widgets dragged, resized, added and
// bound, then saved with PUT /panel by a name with the operate role. A save
// is checked as the file is at start, written back to the -panel file, and
// embedded in the recording again, and rack.panel records it — or its refusal
// — with the revision in force, so a recording says which rack was on screen
// when. Every page showing the rack follows the save from the stream.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/rveen/logb"
	"github.com/rveen/logb/dbc"
	"golang.org/x/sys/unix"

	"github.com/rveen/ktest/can"
	"github.com/rveen/ktest/psu"
	"github.com/rveen/ktest/record"
	dso "github.com/rveen/ktest/scope"
	"github.com/rveen/ktest/stream"
	"github.com/rveen/ktest/web"
)

// version is what the recording says wrote it (§11).
const version = "ktestd 0.4"

func main() {
	var (
		out        = flag.String("o", "", "output file; default is <interface>.logb")
		database   = flag.String("dbc", "", "CAN database; decodes frames into signals beside the raw ones")
		codec      = flag.String("codec", "zstd", "DATA frame codec: none, deflate or zstd")
		errs       = flag.Bool("err", true, "receive error frames")
		dur        = flag.Duration("d", 0, "stop after this long; 0 runs until interrupted")
		rcvbuf     = flag.Int("rcvbuf", 1<<20, "SO_RCVBUF of each receiving socket, in bytes")
		segment    = flag.Duration("segment", time.Second, "how often to begin a new segment")
		quiet      = flag.Bool("q", false, "do not report what could not be carried across")
		sockPath   = flag.String("sock", "/run/ktest.sock", "the control socket")
		httpAddr   = flag.String("http", "", "also serve the recording read-only over TCP, e.g. :8080")
		limitsPath = flag.String("limits", "", "JSON file of limits to enforce; built-in defaults without one")
		httpsAddr  = flag.String("https", "", "also serve control and streams over TLS with named tokens, e.g. :8443")
		tokensPath = flag.String("tokens", "", "-https: file of names the daemon admits, one \"name role sha256\" per line")
		tlsCert    = flag.String("tls-cert", "", "-https: certificate file; without one a self-signed certificate is made and kept")
		tlsKey     = flag.String("tls-key", "", "-https: the key for -tls-cert")
		stateDir   = flag.String("state", defaultStateDir(), "where the daemon keeps what it makes, such as its certificate")
		newToken   = flag.String("new-token", "", "make a token for name:role (view or operate): the token to stderr, "+
			"the line that admits it to stdout; then exit")
		panelPath = flag.String("panel", "", "a rack panel, JSON: served to the browser and embedded in the recording")
		psuPoll   = flag.Duration("psu-poll", 100*time.Millisecond, "how often each supply's channels are measured")
		scopePoll = flag.Duration("scope-poll", 5*time.Millisecond, "how often each scope is asked whether an acquisition has completed")
		scopeSign = flag.String("scope-sign", "sub256", "how a scope code above 127 becomes negative: sub256 (Siglent guide "+
			"PG01-E02D) or sub255 (PG01-E02C). The two differ by one code on every negative sample")
		scopeTrdl = flag.Bool("scope-trigger-delay", false, "take TRDL into a scope's time axis, as third-party readers do "+
			"and the programming guides do not")
		supplies supplyFlags
		scopes   scopeFlags
	)
	flag.Var(&supplies, "psu", "a bench supply to drive, SCPI over TCP: name=host:port[,channels]; repeatable")
	flag.Var(&scopes, "scope", "an oscilloscope to drive, SCPI over TCP: name=host:port[,channels][,full=ch1+ch2];\n"+
		"\tfull names the channels whose every sample goes into the recording, and every channel gets the envelope; repeatable")
	flag.Usage = usage
	flag.Parse()
	if *newToken != "" {
		if err := printNewToken(os.Stdout, os.Stderr, *newToken); err != nil {
			fatalf("%v", err)
		}
		return
	}
	if flag.NArg() != 1 {
		flag.Usage()
	}
	iface := flag.Arg(0)
	name := *out
	if name == "" {
		name = iface + ".logb"
	}

	lim, limRaw, err := loadLimits(*limitsPath)
	if err != nil {
		fatalf("limits: %v", err)
	}
	signRule, err := dso.ParseSignRule(*scopeSign)
	if err != nil {
		fatalf("%v", err)
	}

	cfg := record.Config{
		Bus:     iface,
		Stamp:   can.StampTimestampNS,
		Segment: *segment,
		Meta:    record.Provenance(version, iface),
		Path:    name,
		TX:      true,
	}
	// The limits go into the recording twice: as the values in force, which a
	// checker reads, and as the file they came from, which a person reads.
	cfg.Meta["limits.cyclic.min_period"] = time.Duration(lim.MinPeriod).String()
	if err := checkChannelLimits(lim, supplies, scopes, iface); err != nil {
		fatalf("limits: %v", err)
	}
	// Every settable part states its limits, including the ones that have none:
	// a checker reading this cannot tell "no limit" from "the daemon forgot to
	// say" unless the daemon says both.
	states := func(inst, part string) {
		cl := lim.Channels[inst+"."+part]
		for _, q := range quantitiesOf(part) {
			cfg.Meta["limits."+inst+"."+part+"."+q+"_max"] = limitString(cl.max(q))
		}
	}
	for _, s := range supplies {
		cfg.Meta["psu."+s.name+".addr"] = s.addr
		cfg.Meta["psu."+s.name+".channels"] = strconv.Itoa(s.channels)
		cfg.Meta["psu."+s.name+".dialect"] = psu.Keysight.Name
		for ch := 1; ch <= s.channels; ch++ {
			states(s.name, fmt.Sprintf("ch%d", ch))
		}
	}
	for _, s := range scopes {
		cfg.Meta["scope."+s.name+".addr"] = s.addr
		cfg.Meta["scope."+s.name+".channels"] = strconv.Itoa(s.channels)
		cfg.Meta["scope."+s.name+".dialect"] = dso.Siglent.Name
		cfg.Meta["scope."+s.name+".buckets"] = strconv.Itoa(Buckets)
		// The two readings the programming guide leaves open. A recording that
		// did not say which was taken would be one code out on every negative
		// sample with nothing to say so (§12, milestone 4).
		cfg.Meta["scope."+s.name+".sign"] = signRule.String()
		cfg.Meta["scope."+s.name+".trigger_delay"] = strconv.FormatBool(*scopeTrdl)
		var full []string
		for ch := 1; ch <= s.channels; ch++ {
			if s.full[ch] {
				full = append(full, fmt.Sprintf("ch%d", ch))
			}
		}
		// §10.4: full rate is the exception, and a recording says which
		// channels someone decided to keep at it.
		cfg.Meta["scope."+s.name+".full_rate"] = strings.Join(full, " ")
		if len(full) == 0 {
			cfg.Meta["scope."+s.name+".full_rate"] = "none: the envelope only"
		}
		for _, part := range scopeParts(s.channels) {
			states(s.name, part)
		}
	}
	cfg.Attach = map[string][]byte{}
	if limRaw != nil {
		cfg.Meta["limits.source"] = *limitsPath
		cfg.Attach["limits.json"] = limRaw
	} else {
		cfg.Meta["limits.source"] = "built-in defaults"
	}
	// The panel goes in the same way: a recording opened years later brings
	// back the rack that watched it (§10.1), not only its signals.
	panelRaw, err := loadPanel(*panelPath)
	if err != nil {
		fatalf("panel: %v", err)
	}
	if panelRaw != nil {
		cfg.Meta["panel.source"] = *panelPath
		cfg.Attach["panel.json"] = panelRaw
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
			fmt.Fprintf(os.Stderr, "ktestd: %s\n", fmt.Sprintf(format, a...))
		}
	}
	if *database != "" {
		db, err := dbc.ParseFile(*database)
		if err != nil {
			fatalf("%v", err)
		}
		cfg.DB = db
	}

	// The network listener's credentials are settled before anything is opened,
	// and written into the recording: which certificate the daemon presented,
	// and which names could act, with their roles — never their hashes.
	var tlsCfg *tls.Config
	var tokens *tokenSet
	if *httpsAddr != "" {
		if *tokensPath == "" {
			fatalf("-https needs -tokens: a network listener without authentication is what §7 rules out")
		}
		if tokens, err = loadTokens(*tokensPath); err != nil {
			fatalf("tokens: %v", err)
		}
		cert, source, err := loadOrMakeCert(*tlsCert, *tlsKey, *stateDir)
		if err != nil {
			fatalf("tls: %v", err)
		}
		tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert.tls}, MinVersion: tls.VersionTLS12}
		cfg.Meta["tls.cert.source"] = source
		cfg.Meta["tls.cert.sha256"] = cert.fingerprint
		cfg.Meta["auth.identities"] = tokens.String()
		fmt.Fprintf(os.Stderr, "ktestd: TLS certificate %s\nktestd:   sha256 %s\nktestd:   curl --pinnedpubkey %s\n",
			source, cert.fingerprint, cert.pin)
	}

	// The monitor, opened as canlogb opens it. It sees this daemon's frames the
	// way it sees every other sender on the host — flagged local, not
	// attributed — which is what makes <bus>.raw an account of the bus that is
	// independent of the daemon's own claims about what it sent.
	mopts := can.Monitor()
	mopts.RcvBuf = *rcvbuf
	if *errs {
		mopts.ErrMask = unix.CAN_ERR_MASK
	}
	mon, err := can.Open(iface, mopts)
	if err != nil {
		fatalf("%v", err)
	}
	defer mon.Close()

	// The transmit socket asks for its own frames back. CAN_RAW filters select
	// by identifier rather than by sender, so it receives the whole bus and the
	// echo reader keeps only what MSG_CONFIRM marks as this socket's.
	txc, err := can.Open(iface, can.Options{
		FD:      true,
		OwnMsgs: true,
		RxqOvfl: true,
		RcvBuf:  *rcvbuf,
		Stamp:   can.StampTimestampNS,
	})
	if err != nil {
		fatalf("%v", err)
	}
	defer txc.Close()

	// CAN_BCM lives in a module a stripped kernel can lack (§8), so it is
	// probed rather than assumed, and the recording says what was found.
	bcm, err := can.OpenBCM(iface)
	if err != nil {
		cfg.Meta["can.bcm"] = "unavailable: " + err.Error()
		fmt.Fprintf(os.Stderr, "ktestd: CAN_BCM unavailable (%v); cyclic transmit is disabled\n", err)
		bcm = nil
	} else {
		cfg.Meta["can.bcm"] = "available"
		defer bcm.Close()
	}

	// The control socket before the output file, so that a daemon that cannot
	// start fails before it has written anything.
	ln, err := listenUnix(*sockPath)
	if err != nil {
		fatalf("%v", err)
	}

	f, err := os.Create(name)
	if err != nil {
		fatalf("%v", err)
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	hub := stream.New()
	fw, err := record.New(io.MultiWriter(bw, hub), cfg)
	if err != nil {
		fatalf("%v", err)
	}
	// The rack's stream: the same records, uncompressed and unindexed. See tee.
	rackHub := stream.New()
	rcfg := cfg
	rcfg.Codec, rcfg.Filter, rcfg.Path = logb.CodecNone, logb.FilterNone, ""
	rcfg.Warn = nil // said once, by the recording's writer
	rw, err := record.New(rackHub, rcfg)
	if err != nil {
		fatalf("%v", err)
	}
	w := &tee{file: fw, rack: rw}

	d := &daemon{bus: iface, db: cfg.DB, panelPath: *panelPath,
		supplies: map[string]int{}, scopes: map[string]int{},
		reqs: make(chan *request), closing: make(chan struct{}), gone: make(chan struct{})}
	if panelRaw != nil {
		d.panel.Store(newPanelState(panelRaw, 1, ""))
	} else {
		d.panel.Store(newPanelState(nil, 0, ""))
	}
	for _, s := range supplies {
		d.supplies[s.name] = s.channels
	}
	for _, s := range scopes {
		d.scopes[s.name] = s.channels
	}
	// net/http reports what goes wrong on a connection — a client hanging up
	// mid-handshake, say — through a logger; this one says whose it is.
	srvLog := log.New(os.Stderr, "ktestd: ", 0)
	ctl := &http.Server{Handler: local(d.mux(hub, rackHub)), ErrorLog: srvLog}
	go serve(ctl, ln)
	fmt.Fprintf(os.Stderr, "ktestd: recording %s to %s, control on unix://%s, cyclic floor %v (%s)\n",
		iface, name, *sockPath, time.Duration(lim.MinPeriod), cfg.Meta["limits.source"])

	var secure *http.Server
	if tlsCfg != nil {
		sl, err := net.Listen("tcp", *httpsAddr)
		if err != nil {
			fatalf("%v", err)
		}
		secure = &http.Server{Handler: rack(web.Handler(), authenticated(tokens, d.mux(hub, rackHub))),
			TLSConfig: tlsCfg, ErrorLog: srvLog}
		go func() {
			if err := secure.ServeTLS(sl, "", ""); err != nil && err != http.ErrServerClosed {
				fmt.Fprintf(os.Stderr, "ktestd: https: %v\n", err)
			}
		}()
		fmt.Fprintf(os.Stderr, "ktestd: control and streams on https://%s for %s\n", sl.Addr(), tokens)
	}

	var tcp *http.Server
	if *httpAddr != "" {
		tl, err := net.Listen("tcp", *httpAddr)
		if err != nil {
			fatalf("%v", err)
		}
		tcp = &http.Server{Handler: readOnly(hub, rackHub), ErrorLog: srvLog}
		go serve(tcp, tl)
		fmt.Fprintf(os.Stderr, "ktestd: streaming read-only on http://%s/stream/live\n", tl.Addr())
	}

	// A run is closed by something that knows it ended — a signal or the -d
	// deadline — never by the receive path going quiet (§10.5).
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	deadline := make(<-chan time.Time)
	if *dur > 0 {
		deadline = time.After(*dur)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
		case <-deadline:
		}
		close(done)
	}()

	stop := make(chan struct{})
	rx := make(chan can.Frame, 4096)
	echo := make(chan can.Frame, 256)
	bcmc := make(chan can.BCMMsg, 64)
	fail := make(chan error, 3)
	var txLost atomic.Uint32
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); receive(mon, rx, stop, fail, nil) }()
	go func() {
		defer wg.Done()
		receive(txc, echo, stop, fail, func(f *can.Frame) bool {
			if f.HaveDrops {
				txLost.Store(f.Drops)
			}
			return f.Confirmed
		})
	}()
	if bcm != nil {
		wg.Add(1)
		go func() { defer wg.Done(); receiveBCM(bcm, bcmc, stop, fail) }()
	}

	// Each supply gets its goroutine and its streams, declared now so that a
	// reader meets every channel from the start, answering or not.
	events := make(chan supplyEvent, 256)
	// An acquisition is large and may be dropped (§10.5), so this channel is
	// short on purpose: a driver that cannot hand one over throws it away and
	// says so, rather than queueing megabytes the rack will never draw.
	scopeEvents := make(chan scopeEvent, 8)
	l := &loop{d: d, w: w, bw: bw, txc: txc, bcm: bcm, bcmc: bcmc, events: events,
		scopes: scopeEvents, rackHub: rackHub,
		floor: time.Duration(lim.MinPeriod), limits: lim.Channels, tasks: map[uint32]*task{},
		bus: &inst{name: iface}, insts: map[string]*inst{}}
	l.insts[iface] = l.bus
	// The panel's stream opens with the panel the daemon starts with, so that
	// its revision is held from the first segment on.
	if l.panel, err = w.NewStream(panelSchema); err != nil {
		fatalf("%v", err)
	}
	l.panelRecord(time.Now(), 0, panelSaved, nil, d.panel.Load())
	for _, sp := range supplies {
		s := &supply{name: sp.name, addr: sp.addr, channels: sp.channels, poll: *psuPoll,
			cmds: make(chan supplyCmd, 8), off: make(chan uint32, 1), events: events}
		in := &inst{name: s.name, sup: s}
		for ch := 1; ch <= s.channels; ch++ {
			st, err := w.NewStream(func() *logb.Schema { return chanSchema(s.name, ch) })
			if err != nil {
				fatalf("%v", err)
			}
			meas, err := w.NewStream(func() *logb.Schema { return measSchema(s.name, ch) })
			if err != nil {
				fatalf("%v", err)
			}
			in.chans = append(in.chans, &psuChan{st: st, meas: meas})
		}
		l.insts[s.name] = in
		wg.Add(1)
		go func() { defer wg.Done(); s.run(stop) }()
		fmt.Fprintf(os.Stderr, "ktestd: supply %s at %s, %d channels, measured every %v\n",
			s.name, s.addr, s.channels, s.poll)
	}
	for _, sp := range scopes {
		s := &scopeDriver{name: sp.name, addr: sp.addr, channels: sp.channels, poll: *scopePoll,
			cfg:    dso.Config{Dialect: dso.Siglent, Sign: signRule, TrigDelay: *scopeTrdl, Timeout: 30 * time.Second},
			cmds:   make(chan scopeCmd, 4),
			events: scopeEvents}
		si := &scopeInst{drv: s}
		in := &inst{name: s.name, scp: si}
		if si.timebase, err = w.NewStream(func() *logb.Schema { return timebaseSchema(s.name) }); err != nil {
			fatalf("%v", err)
		}
		if si.acq, err = w.NewStream(func() *logb.Schema { return acqSchema(s.name) }); err != nil {
			fatalf("%v", err)
		}
		for ch := 1; ch <= s.channels; ch++ {
			sc := &scopeChan{}
			if sc.vert, err = w.NewStream(func() *logb.Schema { return vertSchema(s.name, ch) }); err != nil {
				fatalf("%v", err)
			}
			// The envelope goes to both writers; full rate to the recording
			// alone, and only where the configuration asked for it (§10.4).
			if sc.env, err = w.NewWaveform(func() *logb.Schema {
				return envSchema(s.name, ch, time.Microsecond)
			}, true, true); err != nil {
				fatalf("%v", err)
			}
			if sp.full[ch] {
				if sc.samples, err = w.NewWaveform(func() *logb.Schema {
					return samplesSchema(s.name, ch, time.Nanosecond)
				}, true, false); err != nil {
					fatalf("%v", err)
				}
			}
			si.chans = append(si.chans, sc)
		}
		l.insts[s.name] = in
		wg.Add(1)
		go func() { defer wg.Done(); s.run(stop) }()
		fmt.Fprintf(os.Stderr, "ktestd: scope %s at %s, %d channels, %d envelope columns, sign rule %s, full rate %s\n",
			s.name, s.addr, s.channels, Buckets, signRule, cfg.Meta["scope."+s.name+".full_rate"])
	}
	// Every instrument's control stream is declared now, with its kind, so a
	// rack that has only just connected knows a scope from a supply before
	// anyone has taken either one's lease.
	for _, name := range slices.Sorted(maps.Keys(l.insts)) {
		if err := w.DeclareControl(name, l.insts[name].kind()); err != nil {
			fatalf("%v", err)
		}
	}
	unconfirmed := l.run(rx, echo, done, fail)
	close(stop)
	wg.Wait()
	close(d.gone)

	if err := w.Close(); err != nil {
		fatalf("close: %v", err)
	}
	// After the writer, so that subscribers receive the INDEX and END frames,
	// and before the servers stop, so that they have time to.
	hub.Close()
	rackHub.Close()
	shutdown(ctl)
	if secure != nil {
		shutdown(secure)
	}
	if tcp != nil {
		shutdown(tcp)
	}
	if err := bw.Flush(); err != nil {
		fatalf("flush: %v", err)
	}
	if err := f.Close(); err != nil {
		fatalf("close: %v", err)
	}
	if saved, err := w.SaveIndex(); err != nil {
		fmt.Fprintf(os.Stderr, "ktestd: the index was not saved (%v); opening this file will scan it\n", err)
	} else if saved && !*quiet {
		fmt.Fprintf(os.Stderr, "ktestd: index written; opening %s costs no scan\n", name)
	}

	s := w.Stats()
	fmt.Fprintf(os.Stderr, "ktestd: %s: %d frames on the bus", name, s.Frames)
	if s.Decoded > 0 || s.Unknown > 0 {
		fmt.Fprintf(os.Stderr, ", %d decoded, %d not in the database", s.Decoded, s.Unknown)
	}
	fmt.Fprintln(os.Stderr)
	// Every one of these is reported even at zero: "none reported" and "none"
	// differ, which is §10.5's point and §8's.
	fmt.Fprintf(os.Stderr, "ktestd: %d control decisions, %d of them refusals; "+
		"%d sends requested, %d refused by the kernel, %d applied",
		s.Control, s.Denied, s.Requested, s.Refused, s.Applied)
	if s.Unmatched > 0 {
		fmt.Fprintf(os.Stderr, ", %d echoes answering no request", s.Unmatched)
	}
	if unconfirmed > 0 {
		fmt.Fprintf(os.Stderr, ", %d sent and never handed back", unconfirmed)
	}
	fmt.Fprintf(os.Stderr, "; %d cyclic task changes\n", s.Cyclic)
	fmt.Fprintf(os.Stderr, "ktestd: %d frames unstamped, %d frames the kernel admitted losing",
		s.Unstamped, s.Dropped)
	if n := txLost.Load(); n > 0 {
		fmt.Fprintf(os.Stderr, ", %d lost on the transmit socket, echoes possibly among them", n)
	}
	if n := mon.Interrupts() + txc.Interrupts(); n > 0 {
		fmt.Fprintf(os.Stderr, ", %d interrupted reads retried", n)
	}
	fmt.Fprintln(os.Stderr)
}

// limits is the policy the daemon enforces whatever any client asks (§7:
// "independent of what any panel permits"). It is read once, at start, and no
// request changes it: the lease lets a client transmit, and never lets it
// loosen what transmitting may do.
type limits struct {
	// MinPeriod is the floor on a cyclic task's period, so that no task can
	// flood the bus.
	MinPeriod duration `json:"min_period"`

	// Channels bounds each supply channel's setpoints, by channel —
	// "psu1.ch1": {"v_max": 12, "i_max": 1}.
	Channels map[string]chanLimit `json:"channels"`
}

// checkChannelLimits refuses a limit for a channel no -psu declares: a limit
// on a misspelt channel would be one nobody set on the real one.
func checkChannelLimits(lim limits, supplies supplyFlags, scopes scopeFlags, bus string) error {
	have := map[string]bool{}
	named := map[string]string{}
	claim := func(kind, name string) error {
		if name == bus {
			return fmt.Errorf("%s %s is named as the bus is; the two would share a control stream", kind, name)
		}
		if was, ok := named[name]; ok {
			return fmt.Errorf("%s %s is named as the %s is; the two would share a control stream", kind, name, was)
		}
		named[name] = kind
		return nil
	}
	for _, s := range supplies {
		if err := claim("supply", s.name); err != nil {
			return err
		}
		for ch := 1; ch <= s.channels; ch++ {
			have[fmt.Sprintf("%s.ch%d", s.name, ch)] = true
		}
	}
	for _, s := range scopes {
		if err := claim("scope", s.name); err != nil {
			return err
		}
		for _, part := range scopeParts(s.channels) {
			have[s.name+"."+part] = true
		}
	}
	for _, key := range slices.Sorted(maps.Keys(lim.Channels)) {
		cl := lim.Channels[key]
		if !have[key] {
			return fmt.Errorf("%q: no -psu or -scope declares it", key)
		}
		// A limit that applies to nothing the part has is a limit nobody set,
		// which is the failure a limits file must not be able to have.
		for _, q := range []string{"v", "i", "msiz"} {
			v := cl.max(q)
			if v == nil {
				continue
			}
			if !slices.Contains(quantitiesOf(strings.TrimPrefix(key, keyInstrument(key)+".")), q) {
				return fmt.Errorf("%q: %s_max, and this part has no %s to limit", key, q, q)
			}
			if !(*v >= 0) {
				return fmt.Errorf("%q: %s_max is %g; a limit is a number, not negative", key, q, *v)
			}
		}
	}
	return nil
}

// keyInstrument is the instrument a limits key names.
func keyInstrument(key string) string {
	inst, _, _ := strings.Cut(key, ".")
	return inst
}

// limitString is a limit as the recording states it.
func limitString(v *float64) string {
	if v == nil {
		return "none"
	}
	return strconv.FormatFloat(*v, 'g', -1, 64)
}

// defaultLimits are in force when the daemon is given no file, and the
// recording says so.
var defaultLimits = limits{MinPeriod: duration(time.Millisecond)}

// loadLimits reads a limits file over the defaults. A field it does not know
// is an error rather than ignored: a misspelt limit that quietly fell back to
// its default would be a limit nobody set.
func loadLimits(path string) (limits, []byte, error) {
	lim := defaultLimits
	if path == "" {
		return lim, nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return lim, nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&lim); err != nil {
		return lim, nil, fmt.Errorf("%s: %w", path, err)
	}
	return lim, raw, nil
}

// duration is a time.Duration written in JSON as Go writes one: "1ms".
type duration time.Duration

func (d *duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf(`a duration is a string such as "1ms", not %s`, b)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if v < 0 {
		return fmt.Errorf("duration %v is negative", v)
	}
	*d = duration(v)
	return nil
}

// A daemon is the control plane's view of the loop that owns the bus.
type daemon struct {
	bus string
	db  *dbc.File // -dbc, for sends by signal; nil without one

	// The panel in force, which only the loop replaces, and the -panel file
	// a save is written back to; "" without one, and then nothing is saved.
	panel     atomic.Pointer[panelState]
	panelPath string

	supplies map[string]int // -psu: each supply's channel count, by name
	scopes   map[string]int // -scope: each scope's channel count, by name

	// reqs is unbuffered, so a request is either taken by the loop — which
	// then always answers it — or never taken at all.
	reqs chan *request

	// closing is closed when the loop stops taking requests, and gone when
	// every supply has stopped too, so that a write handed to a supply that
	// will now never carry it out is not waited on for ever.
	closing chan struct{}
	gone    chan struct{}
}

type reqKind int

const (
	reqSend reqKind = iota
	reqCyclicSet
	reqCyclicDelete
	reqArm
	reqDisarm
	reqLeaseTake
	reqLeaseRenew
	reqLeaseRelease
	reqChannelSet
	reqStop
	reqPanelSave // recorded in rack.panel, not in a control stream: a panel is no instrument
)

// controlOf is how each kind of request is named in <bus>.control.
var controlOf = map[reqKind]record.ControlRequest{
	reqSend:         record.ControlSend,
	reqCyclicSet:    record.ControlCyclicSet,
	reqCyclicDelete: record.ControlCyclicDelete,
	reqArm:          record.ControlArm,
	reqDisarm:       record.ControlDisarm,
	reqLeaseTake:    record.ControlLeaseTake,
	reqLeaseRenew:   record.ControlLeaseRenew,
	reqLeaseRelease: record.ControlLeaseRelease,
	reqChannelSet:   record.ControlChannelSet,
	reqStop:         record.ControlStop,
}

type request struct {
	kind   reqKind
	inst   string // the instrument it is for: the bus or a supply; "" for the stop, which is for all
	who    caller // who asked, and through which listener
	token  string // the Ktest-Lease header, "" if none was sent
	f      can.Frame
	part   string        // channel set: the part of the instrument, "ch1" or "ch1.vert" or "timebase"
	ch     int           // channel set: the channel the part names, from 1; 0 when it names none
	what   string        // channel set: the quantity, "v", "i", "out", "vdiv", "tdiv", ...
	value  float64       // channel set: the value, a switch as 0 or 1
	period time.Duration // cyclic only
	count  uint32        // cyclic only
	holder string        // lease take only
	ttl    time.Duration // lease take and renew; 0 keeps the lease's term
	panel  []byte        // panel save: the panel, checked
	match  string        // panel save: the sha256 of the panel it was edited from
	reply  chan reply
}

type reply struct {
	seq   uint32
	err   error         // why the request was refused, or the kernel's answer
	token string        // a lease take's token
	ttl   time.Duration // a lease's term, after a take or renew

	// deferred says the request went on to a supply, which answers it itself
	// once the supply has answered: the loop never waits on an instrument.
	deferred bool
}

var (
	errNoTask     = errors.New("no such cyclic task")
	errNoBCM      = errors.New("this kernel has no CAN_BCM (is the can-bcm module loaded?), so cyclic transmit is disabled")
	errNoLease    = errors.New("this request needs the instrument's lease: take one, and send its token as the Ktest-Lease header")
	errLeaseHeld  = errors.New("another client holds the instrument's lease")
	errNotArmed   = errors.New("the instrument is not armed")
	errBelowFloor = errors.New("interlock")
	errForbidden  = errors.New("not permitted")
)

// Lease terms. A term is short by design: it is how long a crashed client's
// cyclic traffic can outlive it.
const (
	defaultTTL = 30 * time.Second
	minTTL     = time.Second
	maxTTL     = time.Hour
)

// inst is one instrument as the control plane knows it: the bus, or a supply.
// Each has its own lease and its own arming (§7), so that a client driving a
// supply does not hold the bus, and one instrument released or disarmed leaves
// the others as they were.
type inst struct {
	name  string
	armed bool
	lease *lease // nil when nobody holds it

	// A supply's, nil and empty for anything else.
	sup   *supply
	chans []*psuChan // channel 1 at index 0

	// A scope's, nil for anything else.
	scp *scopeInst

	// connected says whether the instrument is answering. The bus is never
	// anything else, and leaves it false, as it leaves everything above.
	connected bool
}

// kind is what sort of instrument this is, which the recording states in the
// control stream's schema: the rack reads it to know which path an instrument's
// lease is taken on (§7).
func (in *inst) kind() string {
	switch {
	case in.sup != nil:
		return "psu"
	case in.scp != nil:
		return "scope"
	}
	return "bus"
}

// lease is an instrument's one writer.
type lease struct {
	by      caller // who took it, and through which listener
	holder  string
	token   string
	ttl     time.Duration
	expires time.Time
}

// pending is a frame the kernel took and has not yet handed back.
type pending struct {
	seq uint32
	f   can.Frame
}

// task is a cyclic task as the loop knows it: the last request the kernel took.
type task struct {
	frame  can.Frame
	set    record.CyclicTask // set.Frame points at frame
	active bool
}

// loop owns the recording, the transmit socket and the BCM socket, and the
// control plane's state: the lease, the arming, the limits. Everything that
// reads or writes any of them happens on its goroutine, one event at a time,
// which is why none of it needs a lock and why a request's record can never
// be written out of order with the decision or the kernel call it describes.
type loop struct {
	d   *daemon
	w   *tee
	bw  *bufio.Writer
	txc *can.Conn

	bcm  *can.BCM // nil when the kernel has no CAN_BCM
	bcmc <-chan can.BCMMsg
	// held keeps BCM messages that arrived while a TX_READ answer was awaited,
	// for the loop to handle afterwards and in order.
	held []can.BCMMsg

	bus    *inst
	insts  map[string]*inst // every instrument by name, the bus among them
	panel  *teeStream       // rack.panel
	events <-chan supplyEvent
	scopes <-chan scopeEvent

	// rackHub is the browser's fan-out, consulted before a waveform is written
	// to it: §10.5 lets an acquisition be dropped, and dropping one knowingly
	// beats disconnecting the consumer whole.
	rackHub *stream.Hub
	floor   time.Duration
	limits  map[string]chanLimit

	seq   uint32
	queue []pending // sent and not yet handed back, oldest first
	tasks map[uint32]*task
}

// run is the loop. It returns how many sent frames were never handed back.
func (l *loop) run(rx, echo <-chan can.Frame, done <-chan struct{}, fail <-chan error) int {
	reqs := l.d.reqs
	var drain <-chan time.Time
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case f := <-rx:
			if err := l.w.Frame(&f); err != nil {
				fatalf("write: %v", err)
			}

		case f := <-echo:
			var n uint32
			n, l.queue = match(l.queue, &f)
			if err := l.w.Applied(n, &f); err != nil {
				fatalf("write: %v", err)
			}
			if reqs == nil && len(l.queue) == 0 {
				return 0
			}

		case m := <-l.bcmc:
			l.bcmEvent(m)

		case r := <-reqs:
			if rep := l.handle(r); !rep.deferred {
				r.reply <- rep
			}

		case e := <-l.events:
			l.supplyEvent(e)

		case e := <-l.scopes:
			l.scopeEvent(e)

		case <-tick.C:
			l.expire(time.Now())
			// The recorder first, which turns buffered records into frames,
			// then the file's own buffer, so that a consumer of either sees the
			// recording advance.
			if err := l.w.Flush(); err != nil {
				fatalf("flush: %v", err)
			}
			if err := l.bw.Flush(); err != nil {
				fatalf("flush: %v", err)
			}

		case err := <-fail:
			fatalf("recv: %v", err)

		case <-done:
			// Stop taking requests and stop every task, then give the frames
			// already sent a moment to come back. A recording closed on a send
			// in flight would say it was requested and never applied, which
			// should be a finding only when it is true.
			done, reqs = nil, nil
			close(l.d.closing)
			// A supply's outputs are left as they are: the daemon stopping is
			// not a reason to power down whatever the supply feeds.
			t := time.Now()
			l.expire(t)
			for _, name := range slices.Sorted(maps.Keys(l.insts)) {
				in := l.insts[name]
				in.armed, in.lease = false, nil
				l.record(in, t, 0, record.ControlShutdown, record.VerdictAccepted, nil)
			}
			l.endTasks()
			if len(l.queue) == 0 {
				return 0
			}
			drain = time.After(250 * time.Millisecond)

		case <-drain:
			return len(l.queue)
		}

		for len(l.held) > 0 {
			m := l.held[0]
			l.held = l.held[1:]
			l.bcmEvent(m)
		}
	}
}

// handle takes one request through the control plane and, if it is admitted,
// to the kernel.
func (l *loop) handle(r *request) reply {
	t := time.Now()
	// A lease that ran out between ticks is ended before the request is
	// judged, so that a lapsed token is never honoured for want of a tick.
	l.expire(t)

	// What cannot even be named is answered without a number: no kernel
	// facility to do it with, or no task to do it to.
	in := l.insts[r.inst] // nil for the stop, which is for every instrument
	switch {
	case (r.kind == reqCyclicSet || r.kind == reqCyclicDelete) && l.bcm == nil:
		return reply{err: errNoBCM}
	case r.kind == reqCyclicDelete && l.tasks[r.f.ID] == nil:
		return reply{err: errNoTask}
	case r.kind == reqChannelSet && !in.connected && in.sup != nil:
		return reply{err: errNotConnected}
	case r.kind == reqChannelSet && !in.connected:
		return reply{err: errScopeNotConnected}
	case r.kind == reqChannelSet && in.sup != nil && len(in.sup.cmds) == cap(in.sup.cmds):
		return reply{err: errBusy}
	case r.kind == reqChannelSet && in.scp != nil && len(in.scp.drv.cmds) == cap(in.scp.drv.cmds):
		return reply{err: errScopeBusy}
	}

	l.seq++
	if r.kind == reqPanelSave {
		return l.savePanel(r, t)
	}
	if r.kind == reqStop {
		// Anyone the daemon admits may stop, as anyone may disarm: every
		// instrument in turn, each in its own stream, under one number.
		for _, name := range slices.Sorted(maps.Keys(l.insts)) {
			l.disarm(l.insts[name], t, l.seq, record.ControlStop, r, true)
		}
		return reply{seq: l.seq}
	}
	v, err := l.admit(in, r)
	if err != nil {
		l.control(in, t, r, v)
		return reply{seq: l.seq, err: err}
	}
	switch r.kind {
	case reqSend:
		l.control(in, t, r, v)
		return l.send(r, t)
	case reqCyclicSet:
		l.control(in, t, r, v)
		return l.cyclicSet(r, t)
	case reqCyclicDelete:
		l.control(in, t, r, v)
		return l.cyclicDelete(r, t)
	case reqChannelSet:
		l.control(in, t, r, v)
		if in.scp != nil {
			return l.scopeSet(in, r, t)
		}
		return l.channelSet(in, r, t)
	case reqArm:
		in.armed = true
		l.control(in, t, r, v)
	case reqDisarm:
		l.disarm(in, t, l.seq, record.ControlDisarm, r, true)
	case reqLeaseTake:
		// A client that already holds the lease — by its token, or over TLS
		// by its name, as a reloaded page does — gets the same lease back.
		if in.lease == nil {
			in.lease = &lease{token: newToken()}
		}
		in.lease.by, in.lease.holder, in.lease.ttl, in.lease.expires = r.who, r.holder, r.ttl, t.Add(r.ttl)
		l.control(in, t, r, v)
		return reply{seq: l.seq, token: in.lease.token, ttl: r.ttl}
	case reqLeaseRenew:
		if r.ttl > 0 {
			in.lease.ttl = r.ttl
		}
		in.lease.expires = t.Add(in.lease.ttl)
		l.control(in, t, r, v)
		return reply{seq: l.seq, ttl: in.lease.ttl}
	case reqLeaseRelease:
		in.lease = nil
		l.disarm(in, t, l.seq, record.ControlLeaseRelease, r, false)
	}
	return reply{seq: l.seq}
}

// admit is the control plane's decision on a request, taken before anything
// reaches the kernel or the instrument.
func (l *loop) admit(in *inst, r *request) (record.ControlVerdict, error) {
	if r.kind == reqDisarm {
		// Anyone the daemon admits at all may disarm, whatever their role,
		// lease or no. Stopping is always safe, and an emergency stop that
		// needed the lease would be out of reach exactly when the client
		// holding it has hung.
		return record.VerdictAccepted, nil
	}
	if r.who.role != roleOperate {
		return record.VerdictForbidden, fmt.Errorf("%w: %q has the view role, which may read the streams and disarm, and nothing more",
			errForbidden, r.who.name)
	}
	if r.kind == reqLeaseTake {
		if in.lease != nil && !holds(in, r) && !sameName(in, r) {
			return record.VerdictLeaseHeld, fmt.Errorf("%w: %q holds it for another %v",
				errLeaseHeld, in.lease.holder, time.Until(in.lease.expires).Round(time.Second))
		}
		return record.VerdictAccepted, nil
	}
	if !holds(in, r) {
		return record.VerdictNoLease, errNoLease
	}
	if (r.kind == reqSend || r.kind == reqCyclicSet || r.kind == reqChannelSet) && !in.armed {
		return record.VerdictNotArmed, errNotArmed
	}
	if r.kind == reqCyclicSet && r.period < l.floor {
		return record.VerdictBelowFloor, fmt.Errorf("%w: a period of %v is below this daemon's floor of %v",
			errBelowFloor, r.period, l.floor)
	}
	if r.kind == reqChannelSet {
		limit := l.limits[in.name+"."+r.part].max(r.what)
		if limit != nil && r.value > *limit {
			return record.VerdictAboveLimit, fmt.Errorf("%w: %g is above this channel's limit of %g",
				errAboveLimit, r.value, *limit)
		}
	}
	return record.VerdictAccepted, nil
}

// holds says whether r carries the instrument's current lease: its token and —
// over the network — the name that took it, so that a lease token copied out of
// one client does not let another drive the instrument under the first one's
// name. The local socket is trusted as its file permissions are, and needs only
// the token.
//
// The token comparison takes the same time whatever the token, so that the
// answer's timing says nothing about how much of it was right.
func holds(in *inst, r *request) bool {
	if in.lease == nil || r.token == "" ||
		subtle.ConstantTimeCompare([]byte(r.token), []byte(in.lease.token)) != 1 {
		return false
	}
	return r.who.via != record.ViaTLS || sameName(in, r)
}

// sameName says whether r comes over TLS from the name that took the lease.
func sameName(in *inst, r *request) bool {
	return in.lease != nil && r.who.via == record.ViaTLS &&
		in.lease.by.via == record.ViaTLS && r.who.name == in.lease.by.name
}

// expire ends every lease whose term has run out. A client that crashed stops
// renewing, and this is how its cyclic traffic stops (§7). A supply's outputs
// are left as they are: a lapsed client is a reason to stop taking its writes,
// not to cut the power it set up.
func (l *loop) expire(t time.Time) {
	for _, name := range slices.Sorted(maps.Keys(l.insts)) {
		in := l.insts[name]
		if in.lease == nil || t.Before(in.lease.expires) {
			continue
		}
		in.lease = nil
		l.disarm(in, t, 0, record.ControlLeaseExpired, nil, false)
	}
}

// disarm disarms an instrument. On the bus it ends transmission, deleting every
// task; on a supply, off says whether every output is switched off too, which
// a disarm and the stop do, and a released or lapsed lease does not. The
// decision is recorded first and then its consequences, so that a reader meets
// the cause before them.
func (l *loop) disarm(in *inst, t time.Time, seq uint32, req record.ControlRequest, r *request, off bool) {
	in.armed = false
	l.record(in, t, seq, req, record.VerdictAccepted, r)
	if in.sup != nil {
		if off {
			select {
			case in.sup.off <- seq:
			default: // a stop is already waiting, and this one is the same
			}
		}
		return
	}
	if in.scp != nil {
		// A scope is an input. Disarming stops its settings being written, and
		// there is nothing to switch off: the stop is for outputs (§7), and an
		// acquisition in progress harms nothing by finishing.
		return
	}
	for _, key := range slices.Sorted(maps.Keys(l.tasks)) {
		tk := l.tasks[key]
		dt := time.Now()
		err := l.bcm.Delete(tk.frame.ID, tk.frame.FD())
		if tk.active {
			l.write(dt, &record.Cyclic{Seq: seq, Event: record.CyclicDisarmed, Err: err, Active: err != nil,
				Set: tk.set, Applied: l.readback(tk.frame.ID, tk.frame.FD())})
		}
		// A task the kernel would not delete is still transmitting, and is
		// kept, so that the loop does not lose track of it.
		if err == nil || !tk.active {
			delete(l.tasks, key)
		}
	}
}

// control records the decision on a request.
func (l *loop) control(in *inst, t time.Time, r *request, v record.ControlVerdict) {
	l.record(in, t, l.seq, controlOf[r.kind], v, r)
}

// record writes one control decision into the instrument's control stream,
// with the state it leaves. r is the request it answers, nil for what the
// daemon did unasked.
func (l *loop) record(in *inst, t time.Time, seq uint32, req record.ControlRequest, v record.ControlVerdict, r *request) {
	c := &record.Control{Seq: seq, Request: req, Verdict: v, Armed: in.armed,
		Instrument: in.name, Kind: in.kind()}
	if r != nil {
		c.By, c.Via = r.who.name, r.who.via
		switch r.kind {
		case reqSend, reqCyclicSet, reqCyclicDelete:
			c.Frame, c.Period = &r.f, r.period
		case reqChannelSet:
			c.Channel = channelName{in.name, r.part, r.what}.String()
			c.Value = r.value
		}
	}
	if in.lease != nil {
		c.Holder, c.TTL = in.lease.holder, in.lease.ttl
	}
	if err := l.w.Control(t, c); err != nil {
		fatalf("write: %v", err)
	}
}

func (l *loop) send(r *request, t time.Time) reply {
	// The time was taken before the write, so that a request always precedes
	// the kernel's stamp on its own echo.
	err := l.txc.Send(&r.f)
	if werr := l.w.Requested(l.seq, &r.f, t, err); werr != nil {
		fatalf("write: %v", werr)
	}
	if err == nil {
		l.queue = append(l.queue, pending{l.seq, r.f})
	}
	return reply{seq: l.seq, err: err}
}

// cyclicSet starts a task or changes one.
func (l *loop) cyclicSet(r *request, t time.Time) reply {
	want := &task{frame: r.f, active: true}
	want.set = record.CyclicTask{Frame: &want.frame, Period: r.period, Count: r.count}
	kt := can.Task{Frame: r.f}
	if r.count > 0 {
		kt.Count, kt.Ival1 = r.count, r.period
	} else {
		kt.Ival2 = r.period
	}

	key := r.f.ID
	old := l.tasks[key]
	ev, restart := record.CyclicStart, true
	var err error
	if old != nil {
		ev = record.CyclicUpdate
		// Keeping the timer is what makes an update seamless — measured, the
		// interval across one stayed at 10.0 ms — and it is only right when a
		// running task's timing is unchanged and it runs until stopped.
		restart = !(old.active && old.set.Count == 0 && r.count == 0 &&
			old.set.Period == r.period && old.frame.FD() == r.f.FD())
		if old.frame.FD() != r.f.FD() {
			// A classic task and an FD task are different kernel tasks, so
			// turning one into the other is a delete and a create.
			if err = l.bcm.Delete(old.frame.ID, old.frame.FD()); err == nil {
				delete(l.tasks, key)
				old = nil
			}
		}
	}
	if err == nil {
		err = l.bcm.Setup(&kt, restart)
	}
	active := old != nil && old.active
	if err == nil {
		l.tasks[key] = want
		active = true
	}
	l.write(t, &record.Cyclic{Seq: l.seq, Event: ev, Err: err, Active: active,
		Set: want.set, Applied: l.readback(r.f.ID, r.f.FD())})
	return reply{seq: l.seq, err: err}
}

// cyclicDelete stops a task and forgets it.
func (l *loop) cyclicDelete(r *request, t time.Time) reply {
	old := l.tasks[r.f.ID]
	err := l.bcm.Delete(old.frame.ID, old.frame.FD())
	active := old.active
	if err == nil {
		delete(l.tasks, r.f.ID)
		active = false
	}
	l.write(t, &record.Cyclic{Seq: l.seq, Event: record.CyclicStop, Err: err, Active: active,
		Set: old.set, Applied: l.readback(old.frame.ID, old.frame.FD())})
	return reply{seq: l.seq, err: err}
}

// bcmEvent handles a message the kernel sent unasked. The one that matters
// is TX_EXPIRED: a counted task sent its last frame.
//
// It is believed only if the kernel agrees. An expiry queued by a task's
// previous run can be read after a request has restarted it, and marking the
// new run stopped on the strength of it would be the recording lying; asking
// the kernel whether the task has frames left settles which run it was.
func (l *loop) bcmEvent(m can.BCMMsg) {
	if m.Op != can.BCMExpired {
		return
	}
	tk := l.tasks[m.ID]
	if tk == nil || !tk.active || tk.frame.FD() != m.FD {
		return
	}
	st := l.status(tk.frame.ID, tk.frame.FD())
	if st != nil && (st.Count > 0 || st.Ival2 > 0) {
		return
	}
	tk.active = false
	l.write(time.Now(), &record.Cyclic{Event: record.CyclicExpired, Active: false,
		Set: tk.set, Applied: cyclicTask(st)})
}

// endTasks stops every running task as the daemon stops. Closing the socket
// would stop them anyway — measured — but deleting them here puts the moment
// they stopped into the recording, rather than leaving a reader to infer it
// from the frames ceasing.
func (l *loop) endTasks() {
	for _, key := range slices.Sorted(maps.Keys(l.tasks)) {
		tk := l.tasks[key]
		if !tk.active {
			continue
		}
		t := time.Now()
		err := l.bcm.Delete(tk.frame.ID, tk.frame.FD())
		l.write(t, &record.Cyclic{Event: record.CyclicEnded, Err: err, Active: err != nil,
			Set: tk.set, Applied: l.readback(tk.frame.ID, tk.frame.FD())})
		delete(l.tasks, key)
	}
}

func (l *loop) write(t time.Time, c *record.Cyclic) {
	if err := l.w.Cyclic(t, c); err != nil {
		fatalf("write: %v", err)
	}
}

// readback is the kernel's account of a task, nil if it holds none.
func (l *loop) readback(id uint32, fd bool) *record.CyclicTask {
	return cyclicTask(l.status(id, fd))
}

// status asks the kernel for a task's TX_STATUS. The kernel queues the answer
// before the write returns, so the wait is only for the reader goroutine to
// pass it on; anything else that arrives first is kept, in order, for the
// loop to handle afterwards.
func (l *loop) status(id uint32, fd bool) *can.Task {
	if err := l.bcm.Read(id, fd); err != nil {
		return nil // EINVAL: the kernel holds no such task
	}
	deadline := time.After(100 * time.Millisecond)
	for {
		select {
		case m := <-l.bcmc:
			if m.Op == can.BCMStatus && m.ID == id && m.FD == fd {
				return &m.Task
			}
			l.held = append(l.held, m)
		case <-deadline:
			return nil
		}
	}
}

// cyclicTask turns the kernel's intervals back into a period and a count.
func cyclicTask(st *can.Task) *record.CyclicTask {
	if st == nil {
		return nil
	}
	f := st.Frame
	ct := &record.CyclicTask{Frame: &f, Count: st.Count, Period: st.Ival2}
	if st.Count > 0 || st.Ival2 == 0 {
		ct.Period = st.Ival1
	}
	return ct
}

// match finds the request an echo answers and removes it from the queue.
//
// The kernel hands a socket's frames back in the order it sent them, so the
// answer is normally the oldest one outstanding. Searching past it means an
// echo lost to a receive overflow costs one request its confirmation, rather
// than shifting every answer after it onto the wrong request. Identical frames
// sent back to back are indistinguishable, and are matched in order.
func match(q []pending, f *can.Frame) (uint32, []pending) {
	for i := range q {
		if same(&q[i].f, f) {
			seq := q[i].seq
			return seq, append(q[:i], q[i+1:]...)
		}
	}
	return 0, q
}

func same(a, b *can.Frame) bool {
	return a.ID == b.ID && a.Len == b.Len && a.FD() == b.FD() && a.BRS() == b.BRS() &&
		bytes.Equal(a.Payload(), b.Payload())
}

// newToken is a lease token: 128 random bits, in hex.
func newToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// receive reads one socket until stop is closed. keep, if set, sees every
// frame and says which to pass on.
//
// The short read timeout is only what lets it notice stop. A read that times
// out is not evidence of a quiet bus (§10.5), only of nothing having arrived.
func receive(c *can.Conn, out chan<- can.Frame, stop <-chan struct{}, fail chan<- error,
	keep func(*can.Frame) bool) {

	c.SetReadTimeout(100 * time.Millisecond)
	var f can.Frame
	for {
		select {
		case <-stop:
			return
		default:
		}
		switch err := c.Recv(&f); err {
		case nil:
			if keep != nil && !keep(&f) {
				continue
			}
			select {
			case out <- f:
			case <-stop:
				return
			}
		case unix.EAGAIN:
		default:
			select {
			case fail <- err:
			case <-stop:
			}
			return
		}
	}
}

// receiveBCM passes on every message the kernel sends the BCM socket.
func receiveBCM(b *can.BCM, out chan<- can.BCMMsg, stop <-chan struct{}, fail chan<- error) {
	b.SetReadTimeout(100 * time.Millisecond)
	var m can.BCMMsg
	for {
		select {
		case <-stop:
			return
		default:
		}
		switch err := b.Recv(&m); err {
		case nil:
			select {
			case out <- m:
			case <-stop:
				return
			}
		case unix.EAGAIN:
		default:
			select {
			case fail <- err:
			case <-stop:
			}
			return
		}
	}
}

// leaseHeader carries a lease's token on every request that needs it.
const leaseHeader = "Ktest-Lease"

func (d *daemon) mux(hub, rackHub *stream.Hub) *http.ServeMux {
	m := http.NewServeMux()
	m.Handle("GET /stream/rack", rackHub)
	m.HandleFunc("GET /panel", d.servePanel)
	m.HandleFunc("PUT /panel", d.putPanel)
	m.HandleFunc("POST /bus/{bus}/lease", d.leaseTake)
	m.HandleFunc("PUT /bus/{bus}/lease", d.leaseRenew)
	m.HandleFunc("DELETE /bus/{bus}/lease", d.plain(reqLeaseRelease))
	m.HandleFunc("POST /bus/{bus}/arm", d.plain(reqArm))
	m.HandleFunc("POST /bus/{bus}/disarm", d.plain(reqDisarm))
	m.HandleFunc("POST /bus/{bus}/send", d.send)
	m.HandleFunc("PUT /bus/{bus}/cyclic/{task}", d.cyclicPut)
	m.HandleFunc("DELETE /bus/{bus}/cyclic/{task}", d.cyclicDelete)
	m.HandleFunc("POST /psu/{psu}/lease", d.leaseTake)
	m.HandleFunc("PUT /psu/{psu}/lease", d.leaseRenew)
	m.HandleFunc("DELETE /psu/{psu}/lease", d.plain(reqLeaseRelease))
	m.HandleFunc("POST /psu/{psu}/arm", d.plain(reqArm))
	m.HandleFunc("POST /psu/{psu}/disarm", d.plain(reqDisarm))
	m.HandleFunc("POST /scope/{scope}/lease", d.leaseTake)
	m.HandleFunc("PUT /scope/{scope}/lease", d.leaseRenew)
	m.HandleFunc("DELETE /scope/{scope}/lease", d.plain(reqLeaseRelease))
	m.HandleFunc("POST /scope/{scope}/arm", d.plain(reqArm))
	m.HandleFunc("POST /scope/{scope}/disarm", d.plain(reqDisarm))
	m.HandleFunc("PUT /channels/{channel}", d.channelPut)
	m.HandleFunc("POST /disarm", func(w http.ResponseWriter, r *http.Request) {
		d.submit(w, r, &request{kind: reqStop})
	})
	m.Handle("GET /stream/live", hub)
	m.Handle("GET /stream/status", hub.StatusHandler())
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s\n\n"+
			"  POST   /bus/%[2]s/lease         take the bus's lease: {\"holder\": ..., \"ttl\": \"30s\"}\n"+
			"  PUT    /bus/%[2]s/lease         renew it\n"+
			"  DELETE /bus/%[2]s/lease         release it, disarming the bus\n"+
			"  POST   /bus/%[2]s/arm           arm the bus\n"+
			"  POST   /bus/%[2]s/disarm        disarm it and stop every task; no lease needed\n"+
			"  POST   /bus/%[2]s/send          put one frame on the bus\n"+
			"  PUT    /bus/%[2]s/cyclic/{id}   start or change a cyclic task\n"+
			"  DELETE /bus/%[2]s/cyclic/{id}   stop one\n"+
			"  POST   /psu/{name}/lease         a supply's lease, taken, renewed and released as the bus's is\n"+
			"  POST   /psu/{name}/arm           arm a supply\n"+
			"  POST   /psu/{name}/disarm        disarm it and switch its outputs off; no lease needed\n"+
			"  POST   /scope/{name}/lease       a scope's lease, taken, renewed and released as the bus's is\n"+
			"  POST   /scope/{name}/arm         arm a scope: its settings may then be written\n"+
			"  POST   /scope/{name}/disarm      stop taking writes for it; no lease needed\n"+
			"  PUT    /channels/{channel}       set an instrument: psu1.ch1.v.set, scope1.ch1.vert.vdiv.set,\n"+
			"                                   scope1.timebase.tdiv.set, {\"value\": 5}\n"+
			"  POST   /disarm                   stop: disarm everything, bus and supplies; no lease needed\n"+
			"  GET    /panel                    the rack's panel, its sha256 as the ETag\n"+
			"  PUT    /panel                    save a panel, with If-Match: the ETag of the one it was edited from\n"+
			"  GET    /stream/live              the recording, live, as Logb\n"+
			"  GET    /stream/status            what the fan-out is doing\n\n"+
			"Every request but lease and disarm carries the instrument's lease token as the %[3]s header.\n",
			version, d.bus, leaseHeader)
	})
	return m
}

// rack serves the browser UI beside the API on the network listener. The page
// and its scripts are public code with no data in them, so they are served to
// anyone, which is how a browser gets as far as asking for a token; everything
// that carries data or control goes through api, and so through the token.
func rack(ui, api http.Handler) http.Handler {
	m := http.NewServeMux()
	m.Handle("/bus/", api)
	m.Handle("/psu/", api)
	m.Handle("/scope/", api)
	m.Handle("/channels/", api)
	m.Handle("/disarm", api)
	m.Handle("/stream/", api)
	m.Handle("/panel", api)
	m.Handle("/", ui)
	return m
}

// readOnly is plain -http: the streams, and nothing that acts.
func readOnly(hub, rackHub *stream.Hub) http.Handler {
	m := hub.Mux()
	m.Handle("GET /stream/rack", rackHub)
	return m
}

// plain is a handler for a request that carries nothing but the lease.
func (d *daemon) plain(kind reqKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in, ok := d.instrument(w, r)
		if !ok {
			return
		}
		d.submit(w, r, &request{kind: kind, inst: in})
	}
}

// leaseBody asks for the bus's lease.
//
//	{"holder": "thermal sweep", "ttl": "30s"}
type leaseBody struct {
	Holder string `json:"holder"`
	TTL    string `json:"ttl"`
}

// leaseTake is POST /bus/{bus}/lease.
func (d *daemon) leaseTake(w http.ResponseWriter, r *http.Request) {
	in, ok := d.instrument(w, r)
	if !ok {
		return
	}
	var body leaseBody
	if !decode(w, r, &body) {
		return
	}
	// Over the network the holder is whoever authenticated, not a name the
	// client chooses: the recording's word for who held the bus has to be one
	// nobody could have typed in for someone else.
	if who := callerOf(r); who.via == record.ViaTLS {
		if body.Holder != "" && body.Holder != who.name {
			httpError(w, http.StatusBadRequest, 0, "over TLS the holder is the name you authenticated as, %q", who.name)
			return
		}
		body.Holder = who.name
	}
	// The holder's name is what the recording says held the bus, so it has
	// to be there, and to fit.
	switch {
	case body.Holder == "":
		httpError(w, http.StatusBadRequest, 0, `"holder" is required: the name the recording will give whoever holds the bus`)
		return
	case len(body.Holder) > record.HolderMax || !utf8.ValidString(body.Holder):
		httpError(w, http.StatusBadRequest, 0, `"holder" must be UTF-8 of at most %d bytes`, record.HolderMax)
		return
	}
	ttl, err := parseTTL(body.TTL)
	if err != nil {
		httpError(w, http.StatusBadRequest, 0, "%v", err)
		return
	}
	if ttl == 0 {
		ttl = defaultTTL
	}
	d.submit(w, r, &request{kind: reqLeaseTake, inst: in, holder: body.Holder, ttl: ttl})
}

// leaseRenew is PUT /bus/{bus}/lease, with an optional {"ttl": "30s"}.
func (d *daemon) leaseRenew(w http.ResponseWriter, r *http.Request) {
	in, ok := d.instrument(w, r)
	if !ok {
		return
	}
	var body struct {
		TTL string `json:"ttl"`
	}
	if r.ContentLength != 0 && !decode(w, r, &body) {
		return
	}
	ttl, err := parseTTL(body.TTL)
	if err != nil {
		httpError(w, http.StatusBadRequest, 0, "%v", err)
		return
	}
	d.submit(w, r, &request{kind: reqLeaseRenew, inst: in, ttl: ttl})
}

// send is POST /bus/{bus}/send.
func (d *daemon) send(w http.ResponseWriter, r *http.Request) {
	if !d.ownBus(w, r) {
		return
	}
	var body sendBody
	if !decode(w, r, &body) {
		return
	}
	f, err := body.frame(d.db)
	if err != nil {
		httpError(w, http.StatusBadRequest, 0, "%v", err)
		return
	}
	d.submit(w, r, &request{kind: reqSend, inst: d.bus, f: *f})
}

// cyclicPut is PUT /bus/{bus}/cyclic/{task}.
func (d *daemon) cyclicPut(w http.ResponseWriter, r *http.Request) {
	if !d.ownBus(w, r) {
		return
	}
	id, err := taskID(r.PathValue("task"))
	if err != nil {
		httpError(w, http.StatusBadRequest, 0, "%v", err)
		return
	}
	var body cyclicBody
	if !decode(w, r, &body) {
		return
	}
	period, err := parsePeriod(body.Period)
	if err != nil {
		httpError(w, http.StatusBadRequest, 0, "%v", err)
		return
	}
	sb := sendBody{Data: body.Data, FD: body.FD, BRS: body.BRS, Message: body.Message, Signals: body.Signals}
	if body.Message == "" {
		cid := canID(id & unix.CAN_EFF_MASK)
		sb.ID, sb.Ext = &cid, id&unix.CAN_EFF_FLAG != 0
	}
	f, err := sb.frame(d.db)
	if err != nil {
		httpError(w, http.StatusBadRequest, 0, "%v", err)
		return
	}
	// A task is named by its identifier, in the URL and in the recording, so
	// a message has to be sent by the task that bears its identifier.
	if f.ID != id {
		httpError(w, http.StatusBadRequest, 0, "message %q is %s, and this task is %s",
			body.Message, record.TaskName(f.Arbitration(), f.Extended()), r.PathValue("task"))
		return
	}
	d.submit(w, r, &request{kind: reqCyclicSet, inst: d.bus, f: *f, period: period, count: body.Count})
}

// cyclicDelete is DELETE /bus/{bus}/cyclic/{task}.
func (d *daemon) cyclicDelete(w http.ResponseWriter, r *http.Request) {
	if !d.ownBus(w, r) {
		return
	}
	id, err := taskID(r.PathValue("task"))
	if err != nil {
		httpError(w, http.StatusBadRequest, 0, "%v", err)
		return
	}
	d.submit(w, r, &request{kind: reqCyclicDelete, inst: d.bus, f: can.Frame{ID: id}})
}

// channelPut is PUT /channels/{channel}: {"value": 5}, or for an output
// {"value": true} — 1 and 0 do as well.
func (d *daemon) channelPut(w http.ResponseWriter, r *http.Request) {
	cn, err := parseChannel(r.PathValue("channel"))
	if err != nil {
		httpError(w, http.StatusNotFound, 0, "%v", err)
		return
	}
	if !d.hasChannel(cn) {
		httpError(w, http.StatusNotFound, 0, "no channel %s here", cn)
		return
	}
	var body struct {
		Value any `json:"value"`
	}
	if !decode(w, r, &body) {
		return
	}
	// A switch takes a boolean and a setting takes a number, and neither takes
	// the other: a value the daemon quietly coerced would be recorded as though
	// it had been asked for.
	sw := isSwitch(cn.what)
	var v float64
	switch x := body.Value.(type) {
	case bool:
		if !sw {
			httpError(w, http.StatusBadRequest, 0, "%s is a number, not %v", cn, x)
			return
		}
		if x {
			v = 1
		}
	case float64:
		v = x
		switch {
		case sw && x != 0 && x != 1:
			httpError(w, http.StatusBadRequest, 0, "%s is on or off: true, false, 1 or 0, not %v", cn, x)
			return
		case math.IsNaN(x) || math.IsInf(x, 0):
			httpError(w, http.StatusBadRequest, 0, "%s: %v is not a value an instrument can hold", cn, x)
			return
		case !signed(cn.what) && x < 0:
			// A supply takes a negative setpoint as out of range, or as its
			// magnitude; neither is what the recording would say was asked. A
			// scope's offset and trigger delay really are signed, and are the
			// exception rather than the rule.
			httpError(w, http.StatusBadRequest, 0, "%s: %v; this setting is a number, not negative", cn, x)
			return
		}
	default:
		httpError(w, http.StatusBadRequest, 0, `"value" is required: a number, or for a switch true or false`)
		return
	}
	d.submit(w, r, &request{kind: reqChannelSet, inst: cn.inst, part: cn.part,
		ch: chanOf(cn.part), what: cn.what, value: v})
}

// signed says whether a setting may be negative. Most may not, and the ones
// that may are named here rather than inferred, so that a new setting is
// refused a negative value until someone decides it takes one.
func signed(what string) bool { return what == "ofst" || what == "trdl" }

// hasChannel says whether this daemon has the channel a request names.
func (d *daemon) hasChannel(cn channelName) bool {
	if n, ok := d.supplies[cn.inst]; ok {
		ch := chanOf(cn.part)
		return cn.part == fmt.Sprintf("ch%d", ch) && ch >= 1 && ch <= n &&
			slices.Contains(quantitiesOf(cn.part), cn.what)
	}
	if n, ok := d.scopes[cn.inst]; ok {
		if !slices.Contains(scopeParts(n), cn.part) {
			return false
		}
		return slices.Contains(quantitiesOf(cn.part), cn.what)
	}
	return false
}

// instrument is the instrument a request's path names: the bus, a supply or a
// scope. Each has its own lease and its own arming (§7), so the path says which
// kind it is and the daemon answers for that kind alone.
func (d *daemon) instrument(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.PathValue("bus") != "" {
		return d.bus, d.ownBus(w, r)
	}
	if name := r.PathValue("psu"); name != "" {
		if _, ok := d.supplies[name]; !ok {
			httpError(w, http.StatusNotFound, 0, "no supply %q here", name)
			return "", false
		}
		return name, true
	}
	name := r.PathValue("scope")
	if _, ok := d.scopes[name]; !ok {
		httpError(w, http.StatusNotFound, 0, "no scope %q here", name)
		return "", false
	}
	return name, true
}

func (d *daemon) ownBus(w http.ResponseWriter, r *http.Request) bool {
	if bus := r.PathValue("bus"); bus != d.bus {
		httpError(w, http.StatusNotFound, 0, "no bus %q here; this daemon owns %s", bus, d.bus)
		return false
	}
	return true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		httpError(w, http.StatusBadRequest, 0, "%v", err)
		return false
	}
	return true
}

// submit hands a request to the loop and answers with what came back: the
// request's sequence number and whether it was done — never what that did,
// which is on the stream (§6). A refusal carries its number too, because it
// is recorded under it.
func (d *daemon) submit(w http.ResponseWriter, r *http.Request, req *request) {
	req.who = callerOf(r)
	req.token = r.Header.Get(leaseHeader)
	req.reply = make(chan reply, 1)
	select {
	case d.reqs <- req:
	case <-d.closing:
		httpError(w, http.StatusServiceUnavailable, 0, "the daemon is shutting down")
		return
	case <-r.Context().Done():
		return
	}
	var rep reply
	select {
	case rep = <-req.reply:
	case <-r.Context().Done():
		return
	case <-d.gone:
		// Taken, and handed to a supply that stopped before carrying it out;
		// the recording has the request and no readback, which is the truth.
		httpError(w, http.StatusServiceUnavailable, 0, "the daemon stopped before the supply answered")
		return
	}
	switch {
	case rep.err == nil:
		body := map[string]any{"seq": rep.seq}
		code := http.StatusAccepted
		if rep.token != "" {
			body["token"], code = rep.token, http.StatusCreated
		}
		if rep.ttl > 0 {
			body["ttl"] = rep.ttl.String()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(body)
	case errors.Is(rep.err, errNoTask):
		httpError(w, http.StatusNotFound, 0, "no cyclic task %s on %s",
			record.TaskName(req.f.Arbitration(), req.f.Extended()), d.bus)
	case errors.Is(rep.err, errNoBCM), errors.Is(rep.err, errNotConnected), errors.Is(rep.err, errBusy),
		errors.Is(rep.err, errInstrument):
		httpError(w, http.StatusServiceUnavailable, rep.seq, "%v", rep.err)
	case errors.Is(rep.err, errStale):
		httpError(w, http.StatusPreconditionFailed, rep.seq, "%v", rep.err)
	case errors.Is(rep.err, errPanelFile):
		httpError(w, http.StatusInternalServerError, rep.seq, "%v", rep.err)
	case errors.Is(rep.err, errNoLease), errors.Is(rep.err, errBelowFloor), errors.Is(rep.err, errForbidden),
		errors.Is(rep.err, errAboveLimit):
		httpError(w, http.StatusForbidden, rep.seq, "%v", rep.err)
	case errors.Is(rep.err, errNotArmed), errors.Is(rep.err, errLeaseHeld):
		httpError(w, http.StatusConflict, rep.seq, "%v", rep.err)
	default:
		// Recorded all the same, under this number: the request is part of
		// what was asked for even though the kernel did not do it.
		httpError(w, http.StatusServiceUnavailable, rep.seq, "the kernel refused: %v", rep.err)
	}
}

func httpError(w http.ResponseWriter, code int, seq uint32, format string, a ...any) {
	body := map[string]any{"error": fmt.Sprintf(format, a...)}
	if seq != 0 {
		body["seq"] = seq
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(body)
}

// sendBody is one frame, as JSON:
//
//	{"id": "0x123", "data": "DEADBEEF"}
//	{"id": "0x1ABCDEF", "ext": true, "data": "0102"}
//	{"id": 256, "fd": true, "brs": true, "data": "00112233445566778899AABB"}
//
// or, with -dbc, a message and its signals in physical units:
//
//	{"message": "EngineData", "signals": {"EngineSpeed": 3000, "Gear": "Reverse"}}
type sendBody struct {
	ID   *canID `json:"id"`
	Ext  bool   `json:"ext"`
	Data string `json:"data"`
	FD   bool   `json:"fd"`
	BRS  bool   `json:"brs"`

	Message string         `json:"message"`
	Signals map[string]any `json:"signals"`
}

// cyclicBody is one cyclic task, as JSON. The identifier is in the URL.
//
//	{"data": "0102", "period": "10ms"}
//	{"data": "0102", "period": "250us", "count": 20}
//	{"data": "00112233445566778899AABB", "period": "5ms", "fd": true}
//
// or, with -dbc, by message, whose identifier must be the task's name:
//
//	{"message": "EngineData", "signals": {"EngineSpeed": 3000}, "period": "10ms"}
type cyclicBody struct {
	Data   string `json:"data"`
	Period string `json:"period"`
	Count  uint32 `json:"count"`
	FD     bool   `json:"fd"`
	BRS    bool   `json:"brs"`

	Message string         `json:"message"`
	Signals map[string]any `json:"signals"`
}

// canID is an identifier, as a JSON number (decimal) or a "0x" string (hex).
//
// A string without the prefix is refused rather than read either way:
// candump and cansend write identifiers in bare hex, so "123" from an engineer
// means 0x123, and reading it as decimal would send 0x7B without a word.
type canID uint32

func (c *canID) UnmarshalJSON(b []byte) error {
	s := string(b)
	var n uint64
	var err error
	if strings.HasPrefix(s, `"`) {
		u, uerr := strconv.Unquote(s)
		if uerr != nil {
			return uerr
		}
		h, ok := strings.CutPrefix(strings.ToLower(u), "0x")
		if !ok {
			return fmt.Errorf(`id %s: write hex as "0x%s", or decimal as a bare number`, s, u)
		}
		n, err = strconv.ParseUint(h, 16, 32)
	} else {
		n, err = strconv.ParseUint(s, 10, 32)
	}
	if err != nil {
		return fmt.Errorf("id %s: %w", s, err)
	}
	*c = canID(n)
	return nil
}

// taskID reads a cyclic task's name, which is its identifier as candump
// writes it: three hex digits for a standard frame, eight for an extended one.
// It is the string that ends the task's stream name, so the URL and the
// recording name a task identically, and the width is what says extended, as
// it does for candump — no bare string is read as decimal here either.
func taskID(name string) (uint32, error) {
	n, err := strconv.ParseUint(name, 16, 32)
	switch {
	case err != nil:
	case len(name) == 3 && n <= unix.CAN_SFF_MASK:
		return uint32(n), nil
	case len(name) == 8 && n <= unix.CAN_EFF_MASK:
		return uint32(n) | unix.CAN_EFF_FLAG, nil
	}
	return 0, fmt.Errorf(`task %q: name a task by its identifier as candump writes it, `+
		`three hex digits for a standard frame ("123") or eight for an extended one ("01ABCDEF")`, name)
}

// parsePeriod reads a cyclic period, a Go duration such as "10ms" or "250us".
func parsePeriod(s string) (time.Duration, error) {
	if s == "" {
		return 0, errors.New(`"period" is required, as a duration: "10ms", "250us"`)
	}
	p, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("period: %w", err)
	}
	if p <= 0 {
		return 0, fmt.Errorf("period %v: must be positive", p)
	}
	if p%time.Microsecond != 0 {
		// bcm_timeval carries microseconds; the kernel would run a different
		// period from the one the recording says was asked for.
		return 0, fmt.Errorf("period %v: CAN_BCM counts whole microseconds", p)
	}
	return p, nil
}

// parseTTL reads a lease's term, "" for none given.
func parseTTL(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	ttl, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("ttl: %w", err)
	}
	if ttl < minTTL || ttl > maxTTL {
		return 0, fmt.Errorf("ttl %v: a lease's term is between %v and %v", ttl, minTTL, maxTTL)
	}
	return ttl, nil
}

// fdLens are the payload sizes above eight that a CAN FD frame can carry.
var fdLens = map[int]bool{12: true, 16: true, 20: true, 24: true, 32: true, 48: true, 64: true}

// frame checks a request and builds its frame. Everything the kernel or the
// driver would otherwise adjust silently is refused here instead, because the
// recording would then say one frame was asked for and another went out.
func (b *sendBody) frame(db *dbc.File) (*can.Frame, error) {
	if b.Message != "" {
		return b.fromSignals(db)
	}
	if b.Signals != nil {
		return nil, errors.New(`"signals" go with "message"`)
	}
	if b.ID == nil {
		return nil, errors.New(`"id" is required, or "message" with -dbc`)
	}
	id := uint32(*b.ID)
	if b.Ext {
		if id > unix.CAN_EFF_MASK {
			return nil, fmt.Errorf("id %#x does not fit 29 bits", id)
		}
		id |= unix.CAN_EFF_FLAG
	} else if id > unix.CAN_SFF_MASK {
		// Not promoted: 0x123 and extended 0x00000123 are different frames.
		return nil, fmt.Errorf(`id %#x does not fit 11 bits; say "ext": true for an extended identifier`, id)
	}
	data, err := hex.DecodeString(b.Data)
	if err != nil {
		return nil, fmt.Errorf("data: %w", err)
	}
	switch {
	case b.BRS && !b.FD:
		return nil, errors.New(`"brs" is a CAN FD flag and needs "fd": true`)
	case !b.FD && len(data) > 8:
		return nil, fmt.Errorf(`%d bytes needs "fd": true; a classic frame carries at most 8`, len(data))
	case b.FD && len(data) > 8 && !fdLens[len(data)]:
		// The driver would pad it to the next length a DLC can express.
		return nil, fmt.Errorf("a CAN FD frame carries 0-8, 12, 16, 20, 24, 32, 48 or 64 bytes, not %d", len(data))
	}
	f := can.New(id, data)
	if b.FD {
		f.Wire = can.FDMTU
		if b.BRS {
			f.Flags |= can.FlagBRS
		}
	}
	return f, nil
}

// fromSignals builds a frame from a message and its signals in physical units,
// encoded with the database the daemon was started with. It is the database
// the recording embeds and decodes the bus with, so the signals a recording
// shows for this frame are the ones the request named, rounded to the steps
// the database gives them.
func (b *sendBody) fromSignals(db *dbc.File) (*can.Frame, error) {
	switch {
	case db == nil:
		return nil, errors.New(`sending by "message" needs a database: start the daemon with -dbc`)
	case b.ID != nil || b.Ext || b.FD || b.Data != "":
		return nil, errors.New(`a "message" names the frame: its identifier, length and payload come from the database, ` +
			`not from "id", "ext", "fd" or "data"`)
	}
	m := db.Message(b.Message)
	if m == nil {
		return nil, fmt.Errorf("the database has no message %q", b.Message)
	}
	signals := b.Signals
	if signals == nil {
		signals = map[string]any{}
	}
	payload, err := m.Encode(signals)
	if err != nil {
		return nil, err
	}
	id := m.ID
	if m.Extended {
		id |= unix.CAN_EFF_FLAG
	}
	fd := len(payload) > 8
	switch {
	case fd && !fdLens[len(payload)]:
		return nil, fmt.Errorf("message %q is %d bytes, which no CAN FD frame carries", m.Name, len(payload))
	case b.BRS && !fd:
		return nil, fmt.Errorf(`"brs" is a CAN FD flag, and message %q is a classic frame`, m.Name)
	}
	f := can.New(id, payload)
	if fd {
		f.Wire = can.FDMTU
		if b.BRS {
			f.Flags |= can.FlagBRS
		}
	}
	return f, nil
}

// listenUnix opens the control socket, owner and group only.
//
// A socket file left by a daemon that died is removed. One that answers
// belongs to a daemon that is running, and two daemons must not own one bus.
func listenUnix(path string) (net.Listener, error) {
	// sun_path holds the path and its terminating NUL. A longer path fails in
	// bind with EINVAL, which says nothing about why.
	if max := len(unix.RawSockaddrUnix{}.Path); len(path) >= max {
		return nil, fmt.Errorf("%s: a Unix socket path must be shorter than %d bytes, and this one is %d",
			path, max, len(path))
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s: another daemon is listening there", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	// The mode is set by the umask at creation rather than by a chmod after
	// it, so that there is no moment at which the socket is more open.
	old := unix.Umask(0o117)
	ln, err := net.Listen("unix", path)
	unix.Umask(old)
	return ln, err
}

func serve(srv *http.Server, ln net.Listener) {
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "ktestd: http: %v\n", err)
	}
}

// shutdown lets open streams finish sending what the Hub queued for them.
func shutdown(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		srv.Close()
	}
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "ktestd: %s\n", fmt.Sprintf(format, a...))
	os.Exit(1)
}

func usage() {
	fmt.Fprint(os.Stderr, strings.TrimLeft(`
ktestd records a CAN bus and transmits on it when asked.

It does not configure interfaces. Bring one up first:

    ip link set can0 up type can bitrate 500000

or, for a virtual bus:

    modprobe vcan
    ip link add dev vcan0 type vcan
    ip link set up vcan0

then run it, take the bus's lease, arm it, and transmit:

    ktestd -sock /tmp/ktest.sock vcan0
    curl --unix-socket /tmp/ktest.sock -d '{"holder":"me"}' http://ktest/bus/vcan0/lease
        (the answer carries a token; send it as the Ktest-Lease header from now on)
    curl --unix-socket /tmp/ktest.sock -H 'Ktest-Lease: TOKEN' -X POST http://ktest/bus/vcan0/arm
    curl --unix-socket /tmp/ktest.sock -H 'Ktest-Lease: TOKEN' \
        -d '{"id":"0x123","data":"DEADBEEF"}' http://ktest/bus/vcan0/send
    curl --unix-socket /tmp/ktest.sock -H 'Ktest-Lease: TOKEN' -X PUT \
        -d '{"data":"01","period":"10ms"}' http://ktest/bus/vcan0/cyclic/123
    curl --unix-socket /tmp/ktest.sock -X POST http://ktest/bus/vcan0/disarm

With -dbc, frames can be named by message and signal instead:

    curl --unix-socket /tmp/ktest.sock -H 'Ktest-Lease: TOKEN' \
        -d '{"message":"EngineData","signals":{"EngineSpeed":3000}}' http://ktest/bus/vcan0/send

Over the network, with TLS and named tokens:

    ktestd -new-token rolf:operate >> tokens    the token to the terminal, its hash to the file
    ktestd -https :8443 -tokens tokens vcan0
    curl --cacert ~/.local/state/ktestd/tls-cert.pem -H 'Authorization: Bearer TOKEN' \
        https://HOST:8443/stream/live > run.logb

Bench supplies, each with its own lease, armed on its own:

    ktestd -psu psu1=192.168.1.20:5025,2 -sock /tmp/ktest.sock vcan0
    curl --unix-socket /tmp/ktest.sock -d '{"holder":"me"}' http://ktest/psu/psu1/lease
    curl --unix-socket /tmp/ktest.sock -H 'Ktest-Lease: TOKEN' -X POST http://ktest/psu/psu1/arm
    curl --unix-socket /tmp/ktest.sock -H 'Ktest-Lease: TOKEN' -X PUT \
        -d '{"value":5}' http://ktest/channels/psu1.ch1.v.set
    curl --unix-socket /tmp/ktest.sock -X POST http://ktest/disarm      (the stop: everything, outputs off)

A limits file bounds what even the lease holder may do:

    {"min_period": "1ms", "channels": {"psu1.ch1": {"v_max": 12, "i_max": 1}}}

The rack, on the -https listener, lays out a panel and edits it; a save is
written back to the -panel file, so start a new one as an empty panel:

    echo '{"widgets":[]}' > bench.json
    ktestd -https :8443 -tokens tokens -panel bench.json vcan0

Flags:
`, "\n"))
	flag.PrintDefaults()
	os.Exit(2)
}
