// Package buildinfo exposes metadata injected when binaries are built.
package buildinfo

var (
	// Version is the semantic version injected at build time.
	Version = "dev"
	// Commit is the source revision injected at build time.
	Commit = "unknown"
	// BuildTime is the RFC 3339 build timestamp injected at build time.
	BuildTime = "unknown"
)
