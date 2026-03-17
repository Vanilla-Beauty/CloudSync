package main

import "runtime"

// Build-time variables injected via:
//
//	go build -ldflags "-X main.version=v1.2.3 \
//	                    -X main.buildTime=2026-03-17T12:00:00Z"
//
// The Makefile sets both automatically.  When built with plain
// "go build" (no -ldflags), each variable falls back to a dev default.
// goVersion is always read from the runtime — no injection needed.

var (
	version   = "dev"     // git tag, e.g. "v0.3.1"
	buildTime = "unknown" // RFC 3339 UTC, e.g. "2026-03-17T09:41:00Z"
)

// goVersion returns the Go toolchain version that compiled this binary.
// It is always accurate because it comes from the runtime, not ldflags.
func runtimeGoVersion() string {
	return runtime.Version()
}
