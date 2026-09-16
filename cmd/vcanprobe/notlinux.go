//go:build !linux

// vcanprobe is Linux-only: it speaks AF_CAN directly. Per §8 of the design
// outline the core is Linux and other platforms reach it through the browser,
// so this file exists to keep `go build ./...` honest elsewhere rather than to
// promise a port.
package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	fmt.Fprintf(os.Stderr, "vcanprobe needs SocketCAN and does not run on %s\n", runtime.GOOS)
	os.Exit(1)
}
