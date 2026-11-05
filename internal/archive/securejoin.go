package archive

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// SecureJoin joins unsafe (a path inside an image) onto root, resolving any
// symlinks along the way as if root were "/". The result is always inside root,
// so a malicious layer cannot write to the host through "../" or a symlink.
func SecureJoin(root, unsafe string) (string, error) {
	unsafe = path.Clean("/" + filepath.ToSlash(unsafe))
	parts := strings.Split(strings.TrimPrefix(unsafe, "/"), "/")
	cur := "/"
	for hops := 0; len(parts) > 0; {
		p := parts[0]
		parts = parts[1:]
		if p == "" || p == "." {
			continue
		}
		next := path.Join(cur, p)
		// The last component is not resolved: callers create, remove or
		// replace it themselves.
		if len(parts) == 0 {
			cur = next
			break
		}
		fi, err := os.Lstat(filepath.Join(root, next))
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		hops++
		if hops > 255 {
			return "", fmt.Errorf("too many symlinks resolving %s", unsafe)
		}
		target, err := os.Readlink(filepath.Join(root, next))
		if err != nil {
			return "", err
		}
		if path.IsAbs(target) {
			cur = "/"
		}
		parts = append(strings.Split(path.Clean(target), "/"), parts...)
		if path.IsAbs(target) {
			parts = parts[1:]
		}
	}
	return filepath.Join(root, filepath.FromSlash(path.Clean(cur))), nil
}
