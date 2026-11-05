//go:build unix

package diskcache

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// unlink never removes a directory, even if a pathname is concurrently replaced.
func (c *Cache) unlink(name string) error {
	dir, err := c.root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return unix.Unlinkat(int(dir.Fd()), name, 0)
}

func (c *Cache) openRegular(name string) (*os.File, error) {
	// Root may reject an escaping symlink before the OS returns ELOOP.
	// Classify existing nonregular paths without opening them at all.
	before, err := c.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errUnsafeEntry
	}
	// Do not follow symlinks or block while opening a substituted FIFO/device.
	f, err := c.root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ELOOP) {
		return nil, errUnsafeEntry
	}
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err == nil {
			err = errUnsafeEntry
		}
		return nil, err
	}
	return f, nil
}
