//go:build linux

package record

import (
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// Provenance is §11: what a recorder knows and nobody remembers to record.
// program names the recorder and its version, iface the bus it recorded.
func Provenance(program, iface string) map[string]string {
	m := map[string]string{
		"daemon.version": program,
		"go.version":     runtime.Version(),
		"bus.interface":  iface,
	}
	var u unix.Utsname
	if err := unix.Uname(&u); err == nil {
		m["kernel.release"] = cstr(u.Release[:])
		m["kernel.version"] = cstr(u.Version[:])
		m["host.name"] = cstr(u.Nodename[:])
		m["host.machine"] = cstr(u.Machine[:])
	}
	return m
}

func cstr(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}
