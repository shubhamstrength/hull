//go:build linux

package runtime

import (
	"fmt"
	"os"
	"path/filepath"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/shubhamstrength/hull/internal/archive"
	"github.com/shubhamstrength/hull/internal/home"
)

// LayerDirName is the short directory name for a layer. Names are kept short
// because overlayfs takes every lower directory in one mount option string,
// which is limited to a page (4 KiB).
func LayerDirName(diffID v1.Hash) string { return diffID.Hex[:16] }

// LayersDir is where unpacked layers are cached for overlayfs.
func LayersDir() (string, error) { return home.Sub("layers") }

// EnsureLayers unpacks each of img's layers (once, then cached) so they can be
// stacked with overlayfs. It returns the layers directory and the layer
// directory names, bottom layer first.
func EnsureLayers(img v1.Image) (string, []string, error) {
	root, err := LayersDir()
	if err != nil {
		return "", nil, err
	}
	layers, err := img.Layers()
	if err != nil {
		return "", nil, err
	}
	var names []string
	for _, l := range layers {
		diffID, err := l.DiffID()
		if err != nil {
			return "", nil, err
		}
		name := LayerDirName(diffID)
		dst := filepath.Join(root, name)
		names = append(names, name)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		tmp, err := os.MkdirTemp(root, ".unpack-")
		if err != nil {
			return "", nil, err
		}
		rc, err := l.Uncompressed()
		if err != nil {
			os.RemoveAll(tmp)
			return "", nil, err
		}
		err = archive.Unpack(rc, tmp, archive.Overlay)
		rc.Close()
		if err != nil {
			os.RemoveAll(tmp)
			return "", nil, fmt.Errorf("unpacking layer %s: %w", diffID.Hex[:12], err)
		}
		if err := os.Rename(tmp, dst); err != nil && !os.IsExist(err) {
			os.RemoveAll(tmp)
			if _, statErr := os.Stat(dst); statErr != nil {
				return "", nil, err
			}
		}
	}
	return root, names, nil
}

// AdoptLayer moves an overlay upper directory into the layer cache under the
// given diffID, so a freshly built RUN layer does not need unpacking again.
func AdoptLayer(diffID v1.Hash, dir string) error {
	root, err := LayersDir()
	if err != nil {
		return err
	}
	dst := filepath.Join(root, LayerDirName(diffID))
	if _, err := os.Stat(dst); err == nil {
		return os.RemoveAll(dir)
	}
	return os.Rename(dir, dst)
}
