//go:build !linux

// Package can is Linux-only: it speaks AF_CAN directly. Per §8 of the design
// outline the core is Linux and other platforms reach it through the browser,
// so this file exists to keep `go build ./...` honest elsewhere rather than to
// promise a port.
package can

import "errors"

// ErrUnsupported is returned by every operation on a platform without
// SocketCAN.
var ErrUnsupported = errors.New("can: SocketCAN is available on Linux only")
