//go:build !darwin

package desktopasset

import "errors"

// LaunchAgentLabel exists on every platform so callers need no build tags.
const LaunchAgentLabel = "dev.linsheng.agenthalo.desktop"

// DefaultHelperPath has no meaning off macOS.
func DefaultHelperPath() string { return "" }

// EnsureCurrent is a no-op off macOS. The helper drives a Mac desktop; there is
// nothing here it could usefully be installed into, and reporting success would
// let a non-Mac deployment believe computer use is present.
func EnsureCurrent(string, string) (bool, error) {
	return false, errors.New("the desktop helper is macOS only")
}
