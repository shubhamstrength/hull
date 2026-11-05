// Package store keeps images on disk as an OCI image layout, so the files hull
// produces can be read by any OCI tool.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/match"

	"github.com/shubhamstrength/hull/internal/home"
)

// RefAnnotation is the standard OCI annotation hull uses for image names.
const RefAnnotation = "org.opencontainers.image.ref.name"

// ErrNotFound is returned when an image is not in the store.
var ErrNotFound = errors.New("image not found")

// Store is the local image store.
type Store struct {
	path layout.Path
	dir  string
}

// Image describes a stored image.
type Image struct {
	Ref     string    `json:"ref"`
	Digest  string    `json:"digest"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
	OS      string    `json:"os"`
	Arch    string    `json:"arch"`
}

// Open opens (creating if needed) the store in hull's data directory.
func Open() (*Store, error) {
	dir, err := home.Sub("store")
	if err != nil {
		return nil, err
	}
	return OpenAt(dir)
}

// OpenAt opens a store in dir.
func OpenAt(dir string) (*Store, error) {
	if _, err := os.Stat(filepath.Join(dir, "index.json")); os.IsNotExist(err) {
		if _, err := layout.Write(dir, empty.Index); err != nil {
			return nil, err
		}
	}
	return &Store{path: layout.Path(dir), dir: dir}, nil
}

// Normalize turns a user-supplied reference into the canonical key used in the
// store, e.g. "alpine" -> "index.docker.io/library/alpine:latest".
func Normalize(ref string) (string, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return "", err
	}
	return r.Name(), nil
}

// Short is the human form of a canonical reference.
func Short(ref string) string {
	ref = strings.TrimPrefix(ref, "index.docker.io/library/")
	return strings.TrimPrefix(ref, "index.docker.io/")
}

func (s *Store) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(s.dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// Put stores img under ref, replacing any image that had that name.
func (s *Store) Put(ref string, img v1.Image) error {
	key, err := Normalize(ref)
	if err != nil {
		return err
	}
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	return s.path.ReplaceImage(img, match.Annotation(RefAnnotation, key),
		layout.WithAnnotations(map[string]string{RefAnnotation: key}))
}

// Get loads the image stored as ref.
func (s *Store) Get(ref string) (v1.Image, error) {
	key, err := Normalize(ref)
	if err != nil {
		return nil, err
	}
	idx, err := s.path.ImageIndex()
	if err != nil {
		return nil, err
	}
	m, err := idx.IndexManifest()
	if err != nil {
		return nil, err
	}
	for _, d := range m.Manifests {
		if d.Annotations[RefAnnotation] == key {
			return s.path.Image(d.Digest)
		}
	}
	// Also accept a digest or digest prefix, as shown by `hull images`.
	id := strings.TrimPrefix(ref, "sha256:")
	if len(id) >= 6 && !strings.ContainsAny(id, ":/") {
		for _, d := range m.Manifests {
			if strings.HasPrefix(d.Digest.Hex, id) {
				return s.path.Image(d.Digest)
			}
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, ref)
}

// List returns all stored images, newest first.
func (s *Store) List() ([]Image, error) {
	idx, err := s.path.ImageIndex()
	if err != nil {
		return nil, err
	}
	m, err := idx.IndexManifest()
	if err != nil {
		return nil, err
	}
	var out []Image
	for _, d := range m.Manifests {
		img, err := s.path.Image(d.Digest)
		if err != nil {
			continue
		}
		info := Image{Ref: Short(d.Annotations[RefAnnotation]), Digest: d.Digest.String()}
		if cf, err := img.ConfigFile(); err == nil {
			info.Created, info.OS, info.Arch = cf.Created.Time, cf.OS, cf.Architecture
		}
		if ls, err := img.Layers(); err == nil {
			for _, l := range ls {
				if sz, err := l.Size(); err == nil {
					info.Size += sz
				}
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// Remove deletes the image name and garbage-collects blobs no image uses.
func (s *Store) Remove(ref string) error {
	key, err := Normalize(ref)
	if err != nil {
		return err
	}
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	idx, err := s.path.ImageIndex()
	if err != nil {
		return err
	}
	m, err := idx.IndexManifest()
	if err != nil {
		return err
	}
	found := false
	for _, d := range m.Manifests {
		if d.Annotations[RefAnnotation] == key {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	if err := s.path.RemoveDescriptors(match.Annotation(RefAnnotation, key)); err != nil {
		return err
	}
	return s.gc()
}

func (s *Store) gc() error {
	idx, err := s.path.ImageIndex()
	if err != nil {
		return err
	}
	m, err := idx.IndexManifest()
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, d := range m.Manifests {
		keep[d.Digest.Hex] = true
		img, err := s.path.Image(d.Digest)
		if err != nil {
			continue
		}
		if mf, err := img.Manifest(); err == nil {
			keep[mf.Config.Digest.Hex] = true
			for _, l := range mf.Layers {
				keep[l.Digest.Hex] = true
			}
		}
	}
	blobs := filepath.Join(s.dir, "blobs", "sha256")
	ents, err := os.ReadDir(blobs)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if !keep[e.Name()] {
			os.Remove(filepath.Join(blobs, e.Name()))
		}
	}
	return nil
}

// Inspect returns the image's OCI config as indented JSON.
func Inspect(img v1.Image) ([]byte, error) {
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	d, _ := img.Digest()
	return json.MarshalIndent(struct {
		Digest string `json:"digest"`
		*v1.ConfigFile
	}{d.String(), cf}, "", "  ")
}
