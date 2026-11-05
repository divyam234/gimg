//go:build !unix

package diskcache

import "os"

// Portable Root operations prevent directory escape. Lstat and SameFile reject
// symlinks or pathname substitutions before exposing any payload bytes. Unlike
// the Unix helper, platforms without unlinkat require exclusive trusted access
// to the directory against adversarial replacement during removal.
func (c *Cache) openRegular(name string) (*os.File, error) {
	before, err := c.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errUnsafeEntry
	}
	f, err := c.root.Open(name)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		f.Close()
		if err == nil {
			err = errUnsafeEntry
		}
		return nil, err
	}
	return f, nil
}

func (c *Cache) unlink(name string) error {
	info, err := c.root.Lstat(name)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return errUnsafeEntry
	}
	return c.root.Remove(name)
}
