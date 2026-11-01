// Package archive creates and unpacks OCI layer tarballs.
package archive

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shubhamstrength/hull/internal/ignore"
)

// Epoch is the timestamp written into layers hull creates, so the same inputs
// always produce the same layer digest (and the cache can find it again).
var Epoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	whiteoutPrefix = ".wh."
	whiteoutOpaque = ".wh..wh..opq"
)

// Entry maps a file or directory on disk to a destination path in the image.
// As with Dockerfile COPY, a directory's contents are copied into Dest.
type Entry struct {
	Src  string // absolute source path on disk
	Dest string // absolute destination path in the image
	// Root, when set, is the directory Src lives in. Paths below it are
	// checked against Ignore using their path relative to Root.
	Root   string
	Ignore *ignore.Matcher
}

type item struct {
	name string // image path, no leading slash
	src  string // "" for a synthesized parent directory
	fi   fs.FileInfo
}

// WriteTar writes a deterministic layer containing entries to w. Parent
// directories of each destination are included so the layer applies cleanly.
func WriteTar(w io.Writer, entries []Entry) error {
	items := map[string]item{}
	addParents := func(name string) {
		for d := path.Dir(name); d != "." && d != "/" && d != ""; d = path.Dir(d) {
			if _, ok := items[d]; !ok {
				items[d] = item{name: d}
			}
		}
	}
	for _, e := range entries {
		dest := strings.TrimPrefix(path.Clean("/"+filepath.ToSlash(e.Dest)), "/")
		fi, err := os.Lstat(e.Src)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			items[dest] = item{name: dest, src: e.Src, fi: fi}
			addParents(dest)
			continue
		}
		err = filepath.WalkDir(e.Src, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.Ignore != nil && e.Root != "" && p != e.Root {
				rel, _ := filepath.Rel(e.Root, p)
				if e.Ignore.Excluded(rel) {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
			}
			rel, _ := filepath.Rel(e.Src, p)
			name := path.Join(dest, filepath.ToSlash(rel))
			if name == "" || name == "." {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			items[name] = item{name: name, src: p, fi: info}
			return nil
		})
		if err != nil {
			return err
		}
		addParents(dest)
	}

	names := make([]string, 0, len(items))
	for n := range items {
		names = append(names, n)
	}
	sort.Strings(names)

	tw := tar.NewWriter(w)
	for _, n := range names {
		it := items[n]
		if err := writeItem(tw, it); err != nil {
			return err
		}
	}
	return tw.Close()
}

func writeItem(tw *tar.Writer, it item) error {
	if it.src == "" {
		return tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir, Name: it.name + "/", Mode: 0o755,
			ModTime: Epoch, Format: tar.FormatPAX,
		})
	}
	link := ""
	if it.fi.Mode()&os.ModeSymlink != 0 {
		var err error
		if link, err = os.Readlink(it.src); err != nil {
			return err
		}
	}
	hdr, err := tar.FileInfoHeader(it.fi, link)
	if err != nil {
		return err
	}
	hdr.Name = it.name
	if it.fi.IsDir() {
		hdr.Name += "/"
	}
	hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
	hdr.ModTime, hdr.AccessTime, hdr.ChangeTime = Epoch, time.Time{}, time.Time{}
	hdr.Format = tar.FormatPAX
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if !it.fi.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(it.src)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(tw, f)
	return err
}

// Mode controls how whiteouts are handled when unpacking a layer.
type Mode int

const (
	// Flatten applies whiteouts by deleting files, producing a plain merged
	// filesystem when layers are unpacked in order into one directory.
	Flatten Mode = iota
	// Overlay keeps the layer as an overlayfs lower directory: whiteouts
	// become 0/0 character devices and opaque directories get the
	// trusted.overlay.opaque xattr. Requires root.
	Overlay
)

// Unpack extracts a layer tarball (uncompressed) into dir.
func Unpack(r io.Reader, dir string, mode Mode) error {
	tr := tar.NewReader(r)
	type dirTime struct {
		p string
		t time.Time
	}
	var dirs []dirTime
	isRoot := os.Geteuid() == 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := path.Clean("/" + hdr.Name)
		if name == "/" {
			continue
		}
		base := path.Base(name)
		parent := path.Dir(name)

		if strings.HasPrefix(base, whiteoutPrefix) {
			pdir, err := SecureJoin(dir, parent)
			if err != nil {
				return err
			}
			if base == whiteoutOpaque {
				if mode == Overlay {
					if err := os.MkdirAll(pdir, 0o755); err != nil {
						return err
					}
					if err := setOpaque(pdir); err != nil {
						return err
					}
				} else {
					ents, _ := os.ReadDir(pdir)
					for _, e := range ents {
						os.RemoveAll(filepath.Join(pdir, e.Name()))
					}
				}
				continue
			}
			target := filepath.Join(pdir, strings.TrimPrefix(base, whiteoutPrefix))
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			if mode == Overlay {
				if err := os.MkdirAll(pdir, 0o755); err != nil {
					return err
				}
				if err := mkWhiteout(target); err != nil {
					return err
				}
			}
			continue
		}

		target, err := SecureJoin(dir, name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if fi, err := os.Lstat(target); err == nil && !(fi.IsDir() && hdr.Typeflag == tar.TypeDir) {
			if err := os.RemoveAll(target); err != nil {
				return err
			}
		}
		perm := os.FileMode(hdr.Mode & 0o777)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			dirs = append(dirs, dirTime{target, hdr.ModTime})
		case tar.TypeReg, tar.TypeRegA:
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			src, err := SecureJoin(dir, hdr.Linkname)
			if err != nil {
				return err
			}
			if err := os.Link(src, target); err != nil {
				return err
			}
			continue
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			if !isRoot {
				continue // device nodes need root; skip them when unprivileged
			}
			if err := mknod(target, hdr); err != nil {
				return fmt.Errorf("mknod %s: %w", name, err)
			}
		default:
			continue
		}
		if isRoot {
			_ = os.Lchown(target, hdr.Uid, hdr.Gid)
		}
		if hdr.Typeflag != tar.TypeSymlink {
			if err := os.Chmod(target, perm|specialBits(hdr.Mode)); err != nil {
				return err
			}
			_ = os.Chtimes(target, hdr.ModTime, hdr.ModTime)
		}
		if mode == Overlay {
			for k, v := range hdr.PAXRecords {
				if strings.HasPrefix(k, "SCHILY.xattr.") {
					_ = setXattr(target, strings.TrimPrefix(k, "SCHILY.xattr."), v)
				}
			}
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Chtimes(dirs[i].p, dirs[i].t, dirs[i].t)
	}
	return nil
}

func specialBits(m int64) os.FileMode {
	var out os.FileMode
	if m&0o4000 != 0 {
		out |= os.ModeSetuid
	}
	if m&0o2000 != 0 {
		out |= os.ModeSetgid
	}
	if m&0o1000 != 0 {
		out |= os.ModeSticky
	}
	return out
}
