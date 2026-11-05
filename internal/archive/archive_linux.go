//go:build linux

package archive

import (
	"archive/tar"
	"golang.org/x/sys/unix"
)

func mkWhiteout(p string) error { return unix.Mknod(p, unix.S_IFCHR|0o000, 0) }

func setOpaque(dir string) error {
	return unix.Setxattr(dir, "trusted.overlay.opaque", []byte("y"), 0)
}

func setXattr(p, k, v string) error { return unix.Lsetxattr(p, k, []byte(v), 0) }

func mknod(p string, h *tar.Header) error {
	mode := uint32(h.Mode & 0o7777)
	switch h.Typeflag {
	case tar.TypeChar:
		mode |= unix.S_IFCHR
	case tar.TypeBlock:
		mode |= unix.S_IFBLK
	case tar.TypeFifo:
		mode |= unix.S_IFIFO
	}
	return unix.Mknod(p, mode, int(unix.Mkdev(uint32(h.Devmajor), uint32(h.Devminor))))
}

// isWhiteout reports whether an overlay upper-dir entry is a whiteout device.
func isWhiteout(p string) bool {
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		return false
	}
	return st.Mode&unix.S_IFMT == unix.S_IFCHR && st.Rdev == 0
}

func isOpaque(dir string) bool {
	buf := make([]byte, 4)
	n, err := unix.Lgetxattr(dir, "trusted.overlay.opaque", buf)
	return err == nil && n > 0 && buf[0] == 'y'
}
