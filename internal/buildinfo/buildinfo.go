// Package buildinfo carries the identity of the running binary. The values are
// set at link time; see the Makefile for the -ldflags incantation.
package buildinfo

import (
	"runtime"
	"runtime/debug"
)

var (
	// Version is the release version, e.g. "v1.2.3".
	Version = "dev"
	// Commit is the git revision the binary was built from.
	Commit = ""
	// BuildDate is the RFC 3339 timestamp of the build.
	BuildDate = ""
)

// Info describes the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	BuildDate string `json:"build_date,omitempty"`
	GoVersion string `json:"go_version"`
}

// Get returns the build identity, falling back to the VCS stamps the Go
// toolchain embeds when the linker flags were not supplied.
func Get() Info {
	info := Info{
		Version:   Version,
		Commit:    Commit,
		BuildDate: BuildDate,
		GoVersion: runtime.Version(),
	}
	if info.Commit != "" && info.BuildDate != "" {
		return info
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	for _, setting := range bi.Settings {
		switch setting.Key {
		case "vcs.revision":
			if info.Commit == "" {
				info.Commit = setting.Value
			}
		case "vcs.time":
			if info.BuildDate == "" {
				info.BuildDate = setting.Value
			}
		}
	}
	return info
}
