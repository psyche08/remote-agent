// Package desktopasset carries the macOS desktop helper inside the agent
// binary and writes it out on the device.
//
// It exists to keep the deploy model intact. A release is one artifact in the
// relay manifest, verified by sha256 and signing team, and devices pick it up
// automatically; shipping the helper separately would mean a second artifact,
// a second verification path, and devices whose two halves are on different
// versions. A code signature lives inside the Mach-O, so an embedded binary
// written back out keeps the signature and byte-for-byte notarization ticket
// recorded when publish-release.sh also submits that helper as a top-level
// notary payload entry. This is what lets it keep TCC grants across updates.
//
// The asset is absent in ordinary development builds. That is deliberate: the
// package compiles either way, and a device with no embedded helper reports
// computer use as unavailable rather than failing to start.
package desktopasset

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// assets is a directory rather than a file so `go build` works without the
// helper present. deploy/publish-release.sh drops the signed binary in before
// building the release.
//
//go:embed assets
var assets embed.FS

const assetName = "assets/agenthalo-desktop"

// ErrNotEmbedded means this build carries no helper.
var ErrNotEmbedded = errors.New("this build does not embed the desktop helper")

// Bytes returns the embedded helper.
func Bytes() ([]byte, error) {
	data, err := assets.ReadFile(assetName)
	if err != nil {
		return nil, ErrNotEmbedded
	}
	if len(data) == 0 {
		return nil, ErrNotEmbedded
	}
	return data, nil
}

// Embedded reports whether this build carries a helper.
func Embedded() bool {
	_, err := Bytes()
	return err == nil
}

// SHA256 identifies the embedded helper, so an installer can tell "already
// current" from "needs replacing" without comparing megabytes.
func SHA256() (string, error) {
	data, err := Bytes()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Materialize writes the helper to path, replacing an existing path unless it
// is already a regular, 0755 file with identical contents.
//
// Rewriting an identical binary is not free: the path is what launchd starts
// and what TCC's grants are recorded against, so replacing it needlessly
// invites a restart and a re-prompt for permissions the user already gave.
//
// The write goes to a temporary file in the same directory and is renamed over
// the target, so a reader never sees a half-written executable — which, for a
// binary launchd may start at any moment, would be a crash rather than a
// retry. Returns true when the file was replaced.
func Materialize(path string) (bool, error) {
	data, err := Bytes()
	if err != nil {
		return false, err
	}
	return materialize(path, data)
}

func materialize(path string, data []byte) (bool, error) {
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0o755 &&
		info.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) == 0:
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return false, readErr
		}
		if len(existing) == len(data) && sha256.Sum256(existing) == sha256.Sum256(data) {
			return false, nil
		}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return false, err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(dir, ".agenthalo-desktop-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, err
	}
	return true, nil
}

// Size reports the embedded helper's size, for diagnostics.
func Size() int64 {
	info, err := fs.Stat(assets, assetName)
	if err != nil {
		return 0
	}
	return info.Size()
}
