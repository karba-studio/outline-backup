// Package version carries build metadata, stamped by the linker at release time.
package version

// Overridden with -ldflags "-X .../internal/version.Version=v1.2.3".
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String renders a one-line human-readable build identifier.
func String() string {
	return "outline-backup " + Version + " (" + Commit + ", built " + Date + ")"
}
