package httpapi

import "runtime"

// stack renders the goroutine stack for a recovered panic, capped so a single
// log line cannot grow without bound.
func stack() string {
	buf := make([]byte, 8<<10)
	n := runtime.Stack(buf, false)
	return string(buf[:n])
}
