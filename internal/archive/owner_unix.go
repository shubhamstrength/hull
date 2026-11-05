//go:build unix

package archive

import (
	"archive/tar"
	"io/fs"
	"syscall"
)

func setOwner(h *tar.Header, fi fs.FileInfo) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		h.Uid, h.Gid = int(st.Uid), int(st.Gid)
	}
}
