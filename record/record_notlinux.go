//go:build !linux

// Package record is Linux-only, because the frames it writes come from
// SocketCAN. Per §8 the core is Linux and other platforms reach it through the
// browser, so this file keeps `go build ./...` honest elsewhere.
package record

import "errors"

// ErrUnsupported is returned by every operation on a platform without
// SocketCAN.
var ErrUnsupported = errors.New("record: SocketCAN is available on Linux only")
