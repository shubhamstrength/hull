package archive

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/shubhamstrength/hull/internal/ignore"
)

func names(t *testing.T, b []byte) []string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(b))
	var out []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h.Name)
	}
}

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWriteTarDeterministicAndIgnored(t *testing.T) {
	ctx := t.TempDir()
	write(t, ctx, map[string]string{
		"src/App.jsx": "app", "src/index.js": "idx", "node_modules/x/y.js": "junk", "debug.log": "log",
	})
	ign := ignore.New([]string{"node_modules", "*.log"})
	entries := []Entry{{Src: ctx, Dest: "/app", Root: ctx, Ignore: ign}}
	var a, b bytes.Buffer
	if err := WriteTar(&a, entries); err != nil {
		t.Fatal(err)
	}
	// Touch a file: timestamps must not change the layer.
	os.Chtimes(filepath.Join(ctx, "src/App.jsx"), Epoch.AddDate(5, 0, 0), Epoch.AddDate(5, 0, 0))
	if err := WriteTar(&b, entries); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Error("same input produced different tarballs")
	}
	got := names(t, a.Bytes())
	want := []string{"app/", "app/src/", "app/src/App.jsx", "app/src/index.js"}
	if !equal(got, want) {
		t.Errorf("entries = %v, want %v", got, want)
	}
}

func TestUnpackFlattenWhiteouts(t *testing.T) {
	dir := t.TempDir()
	layer := func(entries ...*tar.Header) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, h := range entries {
			h.Mode = 0o644
			if h.Typeflag == tar.TypeDir {
				h.Mode = 0o755
			}
			tw.WriteHeader(h)
			if h.Size > 0 {
				tw.Write(bytes.Repeat([]byte("x"), int(h.Size)))
			}
		}
		tw.Close()
		return buf.Bytes()
	}
	l1 := layer(
		&tar.Header{Name: "etc/", Typeflag: tar.TypeDir},
		&tar.Header{Name: "etc/motd", Typeflag: tar.TypeReg, Size: 3},
		&tar.Header{Name: "cache/", Typeflag: tar.TypeDir},
		&tar.Header{Name: "cache/a", Typeflag: tar.TypeReg, Size: 1},
		&tar.Header{Name: "lib", Typeflag: tar.TypeSymlink, Linkname: "/usr/lib"},
	)
	l2 := layer(
		&tar.Header{Name: "etc/.wh.motd", Typeflag: tar.TypeReg},
		&tar.Header{Name: "cache/.wh..wh..opq", Typeflag: tar.TypeReg},
		&tar.Header{Name: "cache/b", Typeflag: tar.TypeReg, Size: 1},
		// Writing through the symlink must stay inside dir.
		&tar.Header{Name: "lib/evil", Typeflag: tar.TypeReg, Size: 1},
		&tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Size: 1},
	)
	for _, l := range [][]byte{l1, l2} {
		if err := Unpack(bytes.NewReader(l), dir, Flatten); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "etc/motd")); !os.IsNotExist(err) {
		t.Error("whiteout did not remove etc/motd")
	}
	if _, err := os.Stat(filepath.Join(dir, "cache/a")); !os.IsNotExist(err) {
		t.Error("opaque whiteout did not clear cache/")
	}
	if _, err := os.Stat(filepath.Join(dir, "cache/b")); err != nil {
		t.Error("cache/b missing")
	}
	if _, err := os.Stat(filepath.Join(dir, "usr/lib/evil")); err != nil {
		t.Error("symlinked write should land at dir/usr/lib/evil")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape")); !os.IsNotExist(err) {
		t.Error("../escape was written outside the target directory")
	}
}

func TestSecureJoin(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "usr/lib"), 0o755)
	os.Symlink("/usr/lib", filepath.Join(root, "lib"))
	os.Symlink("../../../../../../etc", filepath.Join(root, "up"))
	cases := map[string]string{
		"/lib/x":           "usr/lib/x",
		"../../etc/passwd": "etc/passwd",
		"/up/passwd":       "etc/passwd",
		"a/./b/../c":       "a/c",
	}
	for in, want := range cases {
		got, err := SecureJoin(root, in)
		if err != nil {
			t.Fatal(err)
		}
		if got != filepath.Join(root, want) {
			t.Errorf("SecureJoin(%q) = %q, want %q", in, got, filepath.Join(root, want))
		}
	}
}

func equal(a, b []string) bool {
	sort.Strings(a)
	sort.Strings(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
