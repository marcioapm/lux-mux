//go:build darwin || freebsd

package config

import (
	"syscall"
	"testing"
)

// statfsBytes is the size of the filesystem holding dir; Bsize is the
// fundamental block size on darwin and FreeBSD, which have no Frsize.
func statfsBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	return int64(st.Blocks) * int64(st.Bsize)
}
