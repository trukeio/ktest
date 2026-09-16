// Package psu drives a programmable bench supply over SCPI on a raw TCP
// socket, the LXI "socket" interface on port 5025.
//
// It says what a supply was told and what it answered, and nothing else: it
// holds no idea of what a setpoint should be, no limits and no leases, which
// are the daemon's (§7). What it offers is the three accounts §4 keeps apart —
// a setpoint as sent, the setpoint the supply reports holding, and what it
// measures — so that a supply that clamps, refuses or trips shows it in the
// recording instead of being papered over by the value that was asked for.
//
// The command spellings are a Dialect, so that a supply that speaks another
// family's SCPI is a table rather than a new driver.
package psu

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// A Dialect spells the commands the driver uses. Each is a format with the
// channel as argument 1 and the value, where there is one, as argument 2.
type Dialect struct {
	Name                 string
	SetVolt, QueryVolt   string
	SetCurr, QueryCurr   string
	SetOut, QueryOut     string // SetOut's value is ON or OFF
	MeasVolt, MeasCurr   string
	Identify, Clear, Err string
}

// Keysight is SCPI with channel lists, as the E36300 series and a good many
// others speak it: VOLT 5,(@1).
var Keysight = Dialect{
	Name:      "keysight",
	SetVolt:   "VOLT %[2]s,(@%[1]d)",
	QueryVolt: "VOLT? (@%[1]d)",
	SetCurr:   "CURR %[2]s,(@%[1]d)",
	QueryCurr: "CURR? (@%[1]d)",
	SetOut:    "OUTP %[2]s,(@%[1]d)",
	QueryOut:  "OUTP? (@%[1]d)",
	MeasVolt:  "MEAS:VOLT? (@%[1]d)",
	MeasCurr:  "MEAS:CURR? (@%[1]d)",
	Identify:  "*IDN?",
	Clear:     "*CLS",
	Err:       "SYST:ERR?",
}

// A Supply is one connection to one supply. It is not safe for concurrent use:
// SCPI over a socket is a conversation, and two interleaved ones get each
// other's answers.
type Supply struct {
	d       Dialect
	timeout time.Duration
	conn    net.Conn
	r       *bufio.Reader
}

// Dial connects and clears the supply's error queue, so that the errors read
// later are the ones this connection caused.
func Dial(addr string, d Dialect, timeout time.Duration) (*Supply, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	s := &Supply{d: d, timeout: timeout, conn: conn, r: bufio.NewReader(conn)}
	if err := s.send(d.Clear); err != nil {
		conn.Close()
		return nil, err
	}
	return s, nil
}

func (s *Supply) Close() error { return s.conn.Close() }

func (s *Supply) send(format string, a ...any) error {
	s.conn.SetWriteDeadline(time.Now().Add(s.timeout))
	_, err := fmt.Fprintf(s.conn, format+"\n", a...)
	return err
}

func (s *Supply) query(format string, a ...any) (string, error) {
	if err := s.send(format, a...); err != nil {
		return "", err
	}
	s.conn.SetReadDeadline(time.Now().Add(s.timeout))
	line, err := s.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (s *Supply) number(format string, ch int) (float64, error) {
	resp, err := s.query(format, ch)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseFloat(resp, 64)
	if err != nil {
		return 0, fmt.Errorf("psu: %q answered %q, which is not a number", fmt.Sprintf(format, ch), resp)
	}
	return v, nil
}

// num spells a value for a command: shortest exact form, never an exponent a
// supply's parser might not take for a plain setpoint.
func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// Identify is the supply's *IDN? answer, for the recording's provenance (§11).
func (s *Supply) Identify() (string, error) { return s.query(s.d.Identify) }

func (s *Supply) SetVoltage(ch int, v float64) error { return s.send(s.d.SetVolt, ch, num(v)) }
func (s *Supply) SetCurrent(ch int, a float64) error { return s.send(s.d.SetCurr, ch, num(a)) }

// Voltage and Current are the setpoints the supply reports holding, which is
// not always what was sent: a supply clamps, and says so in its error queue.
func (s *Supply) Voltage(ch int) (float64, error) { return s.number(s.d.QueryVolt, ch) }
func (s *Supply) Current(ch int) (float64, error) { return s.number(s.d.QueryCurr, ch) }

func (s *Supply) SetOutput(ch int, on bool) error {
	state := "OFF"
	if on {
		state = "ON"
	}
	return s.send(s.d.SetOut, ch, state)
}

func (s *Supply) Output(ch int) (bool, error) {
	resp, err := s.query(s.d.QueryOut, ch)
	if err != nil {
		return false, err
	}
	switch strings.ToUpper(resp) {
	case "1", "ON":
		return true, nil
	case "0", "OFF":
		return false, nil
	}
	return false, fmt.Errorf("psu: output state %q", resp)
}

// MeasureVoltage and MeasureCurrent are what the supply measures at its
// terminals, the third account.
func (s *Supply) MeasureVoltage(ch int) (float64, error) { return s.number(s.d.MeasVolt, ch) }
func (s *Supply) MeasureCurrent(ch int) (float64, error) { return s.number(s.d.MeasCurr, ch) }

// Errors drains the supply's error queue. SCPI keeps errors rather than
// answering commands with them, so a set that was clamped or refused says so
// only here, and only if someone asks.
func (s *Supply) Errors() ([]string, error) {
	var errs []string
	for i := 0; i < 32; i++ {
		resp, err := s.query(s.d.Err)
		if err != nil {
			return errs, err
		}
		code, _, _ := strings.Cut(resp, ",")
		if n, err := strconv.Atoi(strings.TrimSpace(code)); err == nil && n == 0 {
			return errs, nil
		}
		errs = append(errs, resp)
	}
	return errs, fmt.Errorf("psu: the error queue did not empty in 32 reads")
}
