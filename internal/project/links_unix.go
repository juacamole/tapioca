//go:build unix

package project

import (
	"os"
	"syscall"
)

// links returns how many names a file has. More than one means the same inode
// is reachable by another path, which is the whole of the hard-link problem
// aliasesTheConfigDir exists for.
func links(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 1
}
