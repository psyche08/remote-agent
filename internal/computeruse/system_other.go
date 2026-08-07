//go:build !darwin

package computeruse

// NewSystem returns the platform system boundary. Locked Use participates in
// the macOS unlock flow via an Apple Authorization Plug-in and has no analogue
// elsewhere, so every non-Darwin build gets a system that refuses everything.
// This keeps the package building and testable on Linux CI while making it
// impossible for a non-Mac deployment to believe it opened an unlock window.
func NewSystem(scriptPath string) System {
	return unsupportedSystem{}
}
