//go:build !linux

package archive

import (
	"archive/tar"
	"errors"
)

var errLinuxOnly = errors.New("overlay layers need Linux")

func mkWhiteout(string) error               { return errLinuxOnly }
func setOpaque(string) error                { return errLinuxOnly }
func setXattr(string, string, string) error { return nil }
func mknod(string, *tar.Header) error       { return errLinuxOnly }
func isWhiteout(string) bool                { return false }
func isOpaque(string) bool                  { return false }
