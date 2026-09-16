package psu

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SimChannel is one output of a simulated supply: its rating, and the
// resistive load across its terminals.
type SimChannel struct {
	VMax, IMax float64 // volts and amps the channel is rated for
	Load       float64 // ohms
}

// Sim is a bench supply that exists only as a TCP listener speaking the
// Keysight dialect, so that the driver and the daemon can be exercised with no
// hardware. It is simple and deterministic on purpose, and honest in the ways
// that matter for §4:
//
//   - A setpoint beyond the channel's rating is clamped to it, and error -222
//     is queued, as a real supply does — so what was asked for and what was
//     applied differ, and only the error queue says why.
//   - With the output on, the channel regulates voltage until the load would
//     draw more than the current setpoint, and then regulates current: the
//     measured values come from the load, not from the setpoints.
//
// If Transcript is set, every command received is written to it, one per line
// with the time: what the supply was told, in a form nothing in Logb wrote.
type Sim struct {
	Model      string
	Channels   []SimChannel
	Transcript io.Writer

	mu    sync.Mutex
	state []simState
	errs  []string
}

type simState struct {
	v, i float64
	on   bool
}

// Serve accepts connections until the listener closes. Connections share the
// supply, as they do on a real one.
func (s *Sim) Serve(l net.Listener) error {
	s.mu.Lock()
	if s.state == nil {
		s.state = make([]simState, len(s.Channels))
	}
	s.mu.Unlock()
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go s.handle(c)
	}
}

func (s *Sim) handle(c net.Conn) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	for sc.Scan() {
		for cmd := range strings.SplitSeq(sc.Text(), ";") {
			cmd = strings.TrimSpace(cmd)
			if cmd == "" {
				continue
			}
			if resp, ok := s.Exec(cmd); ok {
				if _, err := io.WriteString(c, resp+"\n"); err != nil {
					return
				}
			}
		}
	}
}

// Exec runs one command and returns its answer, with ok false for a command
// that is not a query and so answers nothing.
func (s *Sim) Exec(cmd string) (resp string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == nil {
		s.state = make([]simState, len(s.Channels))
	}
	if s.Transcript != nil {
		fmt.Fprintf(s.Transcript, "%s %s\n", time.Now().Format("15:04:05.000000"), cmd)
	}

	head, args, _ := strings.Cut(strings.TrimSpace(cmd), " ")
	head = strings.ToUpper(head)
	query := strings.HasSuffix(head, "?")
	head = strings.TrimSuffix(head, "?")
	for _, p := range []string{"SOURCE:", "SOUR:"} {
		head = strings.TrimPrefix(head, p)
	}

	switch head {
	case "*IDN":
		return fmt.Sprintf("ktest,%s,0,0.1", s.Model), true
	case "*CLS":
		s.errs = nil
		return "", false
	case "*RST":
		for i := range s.state {
			s.state[i] = simState{}
		}
		return "", false
	case "SYST:ERR", "SYSTEM:ERROR", "SYST:ERR:NEXT", "SYSTEM:ERROR:NEXT":
		if len(s.errs) == 0 {
			return `+0,"No error"`, true
		}
		e := s.errs[0]
		s.errs = s.errs[1:]
		return e, true
	}

	switch head {
	case "VOLT", "VOLTAGE", "CURR", "CURRENT", "OUTP", "OUTPUT",
		"MEAS:VOLT", "MEASURE:VOLTAGE", "MEAS:CURR", "MEASURE:CURRENT":
	default:
		// The header first: a command the supply does not know is that, whatever
		// its arguments are.
		s.fail(`-113,"Undefined header"`)
		return "", false
	}
	value, ch, err := s.split(args)
	if err != nil {
		s.fail(err.Error())
		return "", false
	}
	st := &s.state[ch]
	rate := s.Channels[ch]

	switch head {
	case "VOLT", "VOLTAGE":
		if query {
			return sci(st.v), true
		}
		st.v = s.setpoint(value, rate.VMax)
	case "CURR", "CURRENT":
		if query {
			return sci(st.i), true
		}
		st.i = s.setpoint(value, rate.IMax)
	case "OUTP", "OUTPUT":
		if query {
			if st.on {
				return "1", true
			}
			return "0", true
		}
		switch strings.ToUpper(value) {
		case "ON", "1":
			st.on = true
		case "OFF", "0":
			st.on = false
		default:
			s.fail(`-224,"Illegal parameter value"`)
		}
	case "MEAS:VOLT", "MEASURE:VOLTAGE", "MEAS:CURR", "MEASURE:CURRENT":
		v, i := measure(*st, rate)
		if strings.Contains(head, "CURR") {
			return sci(i), true
		}
		return sci(v), true
	}
	return "", false
}

// split takes "5,(@1)" or "(@1)" apart into a value and a zero-based channel.
func (s *Sim) split(args string) (value string, ch int, err error) {
	args = strings.TrimSpace(args)
	i := strings.Index(args, "(@")
	if i < 0 || !strings.HasSuffix(args, ")") {
		return "", 0, fmt.Errorf(`-109,"Missing parameter"`)
	}
	n, err := strconv.Atoi(args[i+2 : len(args)-1])
	if err != nil || n < 1 || n > len(s.Channels) {
		return "", 0, fmt.Errorf(`-222,"Data out of range"`)
	}
	return strings.TrimSuffix(strings.TrimSpace(args[:i]), ","), n - 1, nil
}

// setpoint parses a value and clamps it into [0, max], queueing -222 when it
// had to.
func (s *Sim) setpoint(value string, limit float64) float64 {
	v, err := strconv.ParseFloat(value, 64)
	if err != nil {
		s.fail(`-224,"Illegal parameter value"`)
		return 0
	}
	if v < 0 || v > limit {
		s.fail(`-222,"Data out of range"`)
		return min(max(v, 0), limit)
	}
	return v
}

func (s *Sim) fail(e string) {
	if len(s.errs) < 16 {
		s.errs = append(s.errs, e)
	}
}

// measure is the channel at its terminals: off, nothing; on, constant voltage
// until the load would draw more than the current setpoint, then constant
// current.
func measure(st simState, rate SimChannel) (v, i float64) {
	if !st.on || rate.Load <= 0 {
		return 0, 0
	}
	if st.v/rate.Load <= st.i {
		return st.v, st.v / rate.Load
	}
	return st.i * rate.Load, st.i
}

func sci(v float64) string { return strconv.FormatFloat(v, 'E', 6, 64) }
