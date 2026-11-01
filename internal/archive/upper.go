package archive

import (
	"archive/tar"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"
)

// WriteUpperTar converts an overlayfs upper directory (the changes a RUN step
// made) into an OCI layer: whiteout devices become .wh.<name> files and opaque
// directories get a .wh..wh..opq marker. File timestamps are kept as is,
// except that skip paths (relative, slash-separated) are left out.
func WriteUpperTar(w io.Writer, upper string, skip map[string]bool) error {
	var names []string
	err := filepath.WalkDir(upper, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == upper {
			return nil
		}
		rel, _ := filepath.Rel(upper, p)
		rel = filepath.ToSlash(rel)
		if skip[rel] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		names = append(names, rel)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(names)
	tw := tar.NewWriter(w)
	for _, rel := range names {
		p := filepath.Join(upper, filepath.FromSlash(rel))
		if isWhiteout(p) {
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg, Name: path.Join(path.Dir(rel), whiteoutPrefix+path.Base(rel)),
				Mode: 0o644, ModTime: Epoch, Format: tar.FormatPAX,
			}); err != nil {
				return err
			}
			continue
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		link := ""
		if fi.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		hdr.Name = rel
		if fi.IsDir() {
			hdr.Name += "/"
		}
		setOwner(hdr, fi)
		hdr.AccessTime, hdr.ChangeTime = time.Time{}, time.Time{}
		hdr.Uname, hdr.Gname = "", ""
		hdr.Format = tar.FormatPAX
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.IsDir() && isOpaque(p) {
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg, Name: path.Join(rel, whiteoutOpaque),
				Mode: 0o644, ModTime: Epoch, Format: tar.FormatPAX,
			}); err != nil {
				return err
			}
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			if err != nil {
				return err
			}
		}
	}
	return tw.Close()
}
