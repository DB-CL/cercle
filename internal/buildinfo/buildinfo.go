// Package buildinfo exposes the version injected at build time with -ldflags.
package buildinfo

// Values replaced by -ldflags "-X github.com/DB-CL/cercle/internal/buildinfo.Version=…".
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String returns the readable version, e.g. "1.2.0 (abc1234, 2026-11-04)".
func String() string {
	return Version + " (" + Commit + ", " + Date + ")"
}
