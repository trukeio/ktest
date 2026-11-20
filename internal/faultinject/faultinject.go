// Package faultinject reaches into the protocol's implementations to make
// them misbehave on purpose, for the conformance kit's reference rack. It is
// internal, so that no program outside this module can freeze a DUT rack.
package faultinject

import "time"

// Freeze stops a *secondary.Server for d, as a stopped process stops: its
// clock, evaluator and streams stand still, and its watermark with them.
// Package secondary sets it.
var Freeze func(server any, d time.Duration)
