package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/random"
)

func TestPutGetListRemove(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := random.Image(512, 1)
	b, _ := random.Image(512, 1)
	if err := s.Put("web", a); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("ghcr.io/me/web:v1", a); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("web:latest", b); err != nil { // replaces "web"
		t.Fatal(err)
	}
	got, err := s.Get("web")
	if err != nil {
		t.Fatal(err)
	}
	gd, _ := got.Digest()
	bd, _ := b.Digest()
	if gd != bd {
		t.Error("Put did not replace the tag")
	}
	list, _ := s.List()
	if len(list) != 2 {
		t.Fatalf("list = %+v", list)
	}
	if _, err := s.Get(bd.Hex[:12]); err != nil {
		t.Errorf("lookup by digest prefix: %v", err)
	}
	if err := s.Remove("web"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("web"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after remove: %v", err)
	}
	// b's blobs are gone, a's are kept for ghcr.io/me/web:v1.
	if _, err := os.Stat(filepath.Join(dir, "blobs", "sha256", bd.Hex)); !os.IsNotExist(err) {
		t.Error("unused manifest blob was not garbage collected")
	}
	if _, err := s.Get("ghcr.io/me/web:v1"); err != nil {
		t.Error(err)
	}
}

func TestShort(t *testing.T) {
	for in, want := range map[string]string{
		"index.docker.io/library/alpine:3.20": "alpine:3.20",
		"index.docker.io/me/app:v1":           "me/app:v1",
		"ghcr.io/me/app:v1":                   "ghcr.io/me/app:v1",
	} {
		if got := Short(in); got != want {
			t.Errorf("Short(%q) = %q", in, got)
		}
	}
}
