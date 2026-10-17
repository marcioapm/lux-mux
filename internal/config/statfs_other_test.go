//go:build !linux && !darwin && !freebsd

package config

import "testing"

// statfsBytes skips: syscall.Statfs_t has no Blocks/Bsize here (OpenBSD
// names them F_blocks/F_bsize; NetBSD has no syscall.Statfs).
func statfsBytes(t *testing.T, _ string) int64 {
	t.Skipf("no portable statfs in syscall")
	return 0
}
