package turnstatehook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// syncDirectoryForDurability is a narrow test seam. Production always points
// at syncDirectoryDurably; tests replace it to prove that a failed directory
// sync is returned fail-closed after an inode/rename is already visible.
var syncDirectoryForDurability = syncDirectoryDurably

// syncDirectoryDurably persists directory-entry changes. Unsupported
// directory fsync (including EINVAL on a filesystem that cannot promise it) is
// deliberately an error: exact-once admission must not claim crash durability
// when the filesystem did not confirm it.
func syncDirectoryDurably(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open durability directory: %w", err)
	}
	info, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return fmt.Errorf("stat durability directory: %w", statErr)
	}
	if !info.IsDir() {
		_ = f.Close()
		return errors.New("durability target is not a directory")
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync durability directory: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close durability directory: %w", err)
	}
	return nil
}

func syncParentDirectoryForDurability(path string) error {
	return syncDirectoryForDurability(filepath.Dir(path))
}
