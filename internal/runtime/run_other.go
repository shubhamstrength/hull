//go:build !linux

package runtime

import (
	"io"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

const (
	InitArg = "__hull_init"
	ShimArg = "__hull_shim"
)

type Stdio struct {
	In       io.Reader
	Out, Err io.Writer
}

type CreateOptions struct {
	Name       string
	Image      string
	Digest     string
	Ports      []string
	AutoRemove bool
	Spec       Spec
}

func Supported() error                                { return ErrUnsupported }
func Init() error                                     { return ErrUnsupported }
func Shim(string) error                               { return ErrUnsupported }
func Exec(*Spec, Stdio, func(int)) (int, error)       { return -1, ErrUnsupported }
func Create(CreateOptions) (*State, error)            { return nil, ErrUnsupported }
func RunForeground(*State, Stdio) (int, error)        { return -1, ErrUnsupported }
func StartDetached(*State) error                      { return ErrUnsupported }
func Stop(*State, time.Duration) error                { return ErrUnsupported }
func Remove(*State, bool) error                       { return ErrUnsupported }
func EnsureLayers(v1.Image) (string, []string, error) { return "", nil, ErrUnsupported }
func AdoptLayer(v1.Hash, string) error                { return ErrUnsupported }
func LayerDirName(d v1.Hash) string                   { return d.Hex[:16] }
