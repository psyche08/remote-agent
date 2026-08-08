package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/psyche08/remote-agent/internal/desktopasset"
)

// runDesktop writes the embedded macOS desktop helper to disk.
//
// The agent also does this on startup, but an installer needs it to have
// happened *before* it registers the LaunchAgent — launchd will not bootstrap a
// job whose program does not exist yet, and an installer that depended on the
// agent having run once would work on a second install and not a first.
func runDesktop(args []string) int {
	if len(args) == 0 || args[0] != "install" {
		fmt.Fprintln(os.Stderr, "usage: remote-agent desktop install [--path <binary>]")
		return 2
	}
	fs := flag.NewFlagSet("remote-agent desktop install", flag.ContinueOnError)
	path := fs.String("path", "", "where to write the helper (default: user Application Support)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	target := *path
	if target == "" {
		target = desktopasset.DefaultHelperPath()
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "the desktop helper is macOS only")
		return 1
	}
	if !desktopasset.Embedded() {
		// Say which build this is rather than which file is missing: an
		// operator hitting this has the wrong binary, not a broken install.
		fmt.Fprintln(os.Stderr, "this build does not embed the desktop helper; use a published release")
		return 1
	}
	replaced, err := desktopasset.Materialize(target)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	sum, _ := desktopasset.SHA256()
	state := "already current"
	if replaced {
		state = "installed"
	}
	fmt.Printf("%s\n", target)
	fmt.Fprintf(os.Stderr, "==> %s (%d bytes, sha256 %s)\n", state, desktopasset.Size(), sum)
	return 0
}
