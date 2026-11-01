// Package engine holds the operations shared by the CLI and the dashboard.
package engine

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/shubhamstrength/hull/internal/build"
	"github.com/shubhamstrength/hull/internal/config"
	"github.com/shubhamstrength/hull/internal/registry"
	"github.com/shubhamstrength/hull/internal/runtime"
	"github.com/shubhamstrength/hull/internal/store"
)

// RunOptions describe a container to create.
type RunOptions struct {
	Image   string   `json:"image"`
	Name    string   `json:"name,omitempty"`
	Cmd     []string `json:"cmd,omitempty"`
	Ports   []string `json:"ports,omitempty"`
	Env     []string `json:"env,omitempty"`
	Volumes []string `json:"volumes,omitempty"`
	Memory  string   `json:"memory,omitempty"`
	CPUs    float64  `json:"cpus,omitempty"`
	Pids    int64    `json:"pids,omitempty"`
	Network string   `json:"network,omitempty"`
	Detach  bool     `json:"detach,omitempty"`
	Remove  bool     `json:"remove,omitempty"`

	baseDir string
}

// ApplyProject fills in defaults from a project's run: section. Values given
// on the command line win; lists are combined.
func (o *RunOptions) ApplyProject(p *config.Project) {
	r := p.Run
	o.baseDir = p.Dir
	if len(o.Cmd) == 0 {
		o.Cmd = r.Cmd
	}
	o.Ports = append(append([]string{}, r.Ports...), o.Ports...)
	o.Volumes = append(append([]string{}, r.Volumes...), o.Volumes...)
	var env []string
	for _, k := range config.SortedKeys(r.Env) {
		env = append(env, k+"="+r.Env[k])
	}
	o.Env = append(env, o.Env...)
	if o.Memory == "" {
		o.Memory = r.Memory
	}
	if o.CPUs == 0 {
		o.CPUs = r.CPUs
	}
	if o.Pids == 0 {
		o.Pids = r.Pids
	}
	if o.Network == "" {
		o.Network = r.Network
	}
	if o.Name == "" && p.Name != "" {
		o.Name = uniqueName(p.Name)
	}
}

func uniqueName(base string) string {
	list, _ := runtime.List()
	taken := map[string]bool{}
	for _, s := range list {
		taken[s.Name] = true
	}
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		if n := fmt.Sprintf("%s-%d", base, i); !taken[n] {
			return n
		}
	}
}

// CreateContainer resolves the image (pulling it if it is not stored), works
// out the command, environment and limits, and records a new container.
func CreateContainer(st *store.Store, o RunOptions, out io.Writer) (*runtime.State, error) {
	if err := runtime.Supported(); err != nil {
		return nil, err
	}
	img, err := st.Get(o.Image)
	if err != nil {
		plat, perr := v1.ParsePlatform(build.DefaultPlatform())
		if perr != nil {
			return nil, perr
		}
		fmt.Fprintf(out, "Image %s not found locally, pulling...\n", o.Image)
		remote, perr := registry.Pull(o.Image, *plat)
		if perr != nil {
			return nil, fmt.Errorf("%v (and pulling failed: %v)", err, perr)
		}
		if err := st.Put(o.Image, remote); err != nil {
			return nil, err
		}
		if img, err = st.Get(o.Image); err != nil {
			return nil, err
		}
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	digest, _ := img.Digest()

	args := append([]string{}, cf.Config.Entrypoint...)
	if len(o.Cmd) > 0 {
		args = append(args, o.Cmd...)
	} else {
		args = append(args, cf.Config.Cmd...)
	}
	env := append([]string{}, cf.Config.Env...)
	for _, kv := range o.Env {
		if !strings.Contains(kv, "=") {
			kv = kv + "=" + os.Getenv(kv) // -e NAME passes the host value through
		}
		env = setEnv(env, kv)
	}
	mem, err := runtime.ParseMemory(o.Memory)
	if err != nil {
		return nil, err
	}
	base := o.baseDir
	if base == "" {
		base, _ = os.Getwd()
	}
	var vols []runtime.Volume
	for _, v := range o.Volumes {
		vol, err := runtime.ParseVolume(v, base)
		if err != nil {
			return nil, err
		}
		vols = append(vols, vol)
	}
	network := o.Network
	if network == "" {
		network = "host"
	}
	if network != "host" && network != "none" {
		return nil, fmt.Errorf("network %q: use host or none", network)
	}
	for _, p := range o.Ports {
		h, c, ok := strings.Cut(strings.TrimSuffix(strings.TrimSuffix(p, "/tcp"), "/udp"), ":")
		if ok && h != c {
			fmt.Fprintf(out, "warning: containers share the host network, so port %s is reachable on the host as %s, not %s\n", p, c, h)
		}
	}
	layersDir, layers, err := runtime.EnsureLayers(img)
	if err != nil {
		return nil, err
	}
	return runtime.Create(runtime.CreateOptions{
		Name: o.Name, Image: o.Image, Digest: digest.String(), Ports: o.Ports, AutoRemove: o.Remove,
		Spec: runtime.Spec{
			LayersDir: layersDir, Layers: layers, Args: args, Env: env,
			Workdir: cf.Config.WorkingDir, User: cf.Config.User, Volumes: vols,
			Network: network, ResolvConf: network == "host",
			Limits: runtime.Limits{MemoryBytes: mem, CPUs: o.CPUs, Pids: o.Pids},
		},
	})
}

func setEnv(env []string, kv string) []string {
	k, _, _ := strings.Cut(kv, "=")
	for i, e := range env {
		if strings.HasPrefix(e, k+"=") {
			env[i] = kv
			return env
		}
	}
	return append(env, kv)
}

// TailLines returns the last n lines of a file.
func TailLines(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	const chunk = 64 * 1024
	size := fi.Size()
	var buf []byte
	for off := size; off > 0 && bytes.Count(buf, []byte("\n")) <= n; {
		step := int64(chunk)
		if off < step {
			step = off
		}
		off -= step
		b := make([]byte, step)
		if _, err := f.ReadAt(b, off); err != nil && err != io.EOF {
			return nil, err
		}
		buf = append(b, buf...)
	}
	lines := bytes.SplitAfter(buf, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return bytes.Join(lines, nil), nil
}

// HumanSize formats a byte count, e.g. 12.3 MB.
func HumanSize(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
