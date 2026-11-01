//go:build !unix

package archive

import (
	"archive/tar"
	"io/fs"
)

func setOwner(*tar.Header, fs.FileInfo) {}
