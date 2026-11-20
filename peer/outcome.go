package peer

import "fmt"

// Outcome is how a DUT operation ended (doc/peer-protocol.md §7.4): the codes
// a DUT rack's dut.action.<name> records carry, and what this package's DUT
// operations fail with.
type Outcome string

const (
	OK       Outcome = "ok"
	NOK      Outcome = "nok"
	Timeout  Outcome = "timeout"   // a wait ran out
	Refused  Outcome = "refused"   // the DUT rack refused a request
	LinkLost Outcome = "link_lost" // the DUT rack stopped answering, or its watermark stopped
	Fail     Outcome = "fail"      // an action or a mode's entry action failed
	Stopped  Outcome = "stopped"   // stopped before its end
)

// Outcomes is every outcome, in the order the protocol numbers them from 1.
var Outcomes = []Outcome{OK, NOK, Timeout, Refused, LinkLost, Fail, Stopped}

// OutcomeOf is the outcome of a protocol code; an unknown code is Fail.
func OutcomeOf(code int) Outcome {
	if code >= 1 && code <= len(Outcomes) {
		return Outcomes[code-1]
	}
	return Fail
}

// Failure is a DUT operation ending other than ok, with why.
type Failure struct {
	Outcome Outcome
	Msg     string
}

func (f *Failure) Error() string { return string(f.Outcome) + ": " + f.Msg }

func failf(o Outcome, format string, a ...any) error {
	return &Failure{Outcome: o, Msg: fmt.Sprintf(format, a...)}
}
