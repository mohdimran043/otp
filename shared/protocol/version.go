// Package protocol defines the Optical Transport Protocol frame format: the
// on-screen geometry, the header and footer records, and the versioning rules
// that let a decoder read a frame produced by a different build.
//
// The package is deliberately free of runtime concerns. It has no database, no
// HTTP, and no logging dependencies, because the sender and receiver
// applications share this definition and nothing else.
package protocol

import "fmt"

// Protocol versions. A version is bumped whenever the wire layout of the header,
// footer, or grid geometry changes in a way an older decoder cannot read.
const (
	// Version1 is the initial adaptive-grid layout.
	Version1 uint16 = 1

	// Current is the version this build emits.
	Current = Version1

	// MinSupported is the oldest version this build can decode.
	MinSupported = Version1
)

// VersionInfo describes a protocol version for negotiation and documentation.
type VersionInfo struct {
	Version     uint16 `json:"version"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Deprecated  bool   `json:"deprecated"`
}

// Versions lists every version this build understands, oldest first.
func Versions() []VersionInfo {
	return []VersionInfo{
		{
			Version:     Version1,
			Name:        "otp/1",
			Description: "Adaptive grid with four corner finders, binary-modulated header and footer bands, and per-frame CRC32 plus SHA-256.",
		},
	}
}

// SupportsVersion reports whether this build can decode the given version.
func SupportsVersion(v uint16) bool {
	return v >= MinSupported && v <= Current
}

// CheckVersion validates a version read off the wire.
func CheckVersion(v uint16) error {
	if !SupportsVersion(v) {
		return fmt.Errorf("%w: frame declares version %d, this build supports %d..%d",
			ErrUnsupportedVersion, v, MinSupported, Current)
	}
	return nil
}
