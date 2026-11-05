// Package runtime runs containers using Linux namespaces, cgroups and overlayfs.
package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shubhamstrength/hull/internal/home"
)

// ErrUnsupported is returned on platforms where containers cannot run.
var ErrUnsupported = errors.New("running containers needs Linux (on macOS or Windows, run hull inside a Linux VM; build, push and save still work)")

// Spec is everything the container's init process needs. It is passed from
// the parent to the init process as JSON over a pipe.
type Spec struct {
	ID         string   `json:"id"`
	Hostname   string   `json:"hostname"`
	LayersDir  string   `json:"layersDir"`
	Layers     []string `json:"layers"` // directory names under LayersDir, bottom first
	Upper      string   `json:"upper"`
	Work       string   `json:"work"`
	Rootfs     string   `json:"rootfs"`
	Args       []string `json:"args"`
	Env        []string `json:"env"`
	Workdir    string   `json:"workdir"`
	User       string   `json:"user"`
	Volumes    []Volume `json:"volumes,omitempty"`
	Network    string   `json:"network"` // "host" (default) or "none"
	ResolvConf bool     `json:"resolvConf"`
	Limits     Limits   `json:"limits"`
}

// Volume is a bind mount from the host into the container.
type Volume struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// Limits are cgroup resource limits. Zero means unlimited.
type Limits struct {
	MemoryBytes int64   `json:"memoryBytes,omitempty"`
	CPUs        float64 `json:"cpus,omitempty"`
	Pids        int64   `json:"pids,omitempty"`
}

// Status of a container.
const (
	StatusCreated = "created"
	StatusRunning = "running"
	StatusExited  = "exited"
)

// State is what hull records about a container on disk.
type State struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Image      string    `json:"image"`
	Digest     string    `json:"digest"`
	Command    []string  `json:"command"`
	Ports      []string  `json:"ports,omitempty"`
	Status     string    `json:"status"`
	Pid        int       `json:"pid,omitempty"`
	ShimPid    int       `json:"shimPid,omitempty"`
	ExitCode   int       `json:"exitCode"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	AutoRemove bool      `json:"autoRemove,omitempty"`
	Spec       Spec      `json:"spec"`
}

// ContainersDir is where container state, logs and writable layers live.
func ContainersDir() (string, error) { return home.Sub("containers") }

// Dir returns the directory for container id.
func Dir(id string) string {
	d, _ := ContainersDir()
	return filepath.Join(d, id)
}

// LogPath returns the path of the container's combined stdout/stderr log.
func LogPath(id string) string { return filepath.Join(Dir(id), "container.log") }

// Save writes the container state atomically.
func (s *State) Save() error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(Dir(s.ID), "state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(Dir(s.ID), "state.json"))
}

// Load reads a container's state by ID, ID prefix or name.
func Load(idOrName string) (*State, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	var matches []*State
	for _, s := range all {
		if s.ID == idOrName || s.Name == idOrName {
			return s, nil
		}
		if len(idOrName) >= 3 && strings.HasPrefix(s.ID, idOrName) {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no container named %q", idOrName)
	case 1:
		return matches[0], nil
	}
	return nil, fmt.Errorf("%q matches more than one container; use more of the ID", idOrName)
}

// List returns all containers, newest first. Containers whose process has
// gone away (for example after a reboot) are reported as exited.
func List() ([]*State, error) {
	dir, err := ContainersDir()
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*State
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "state.json"))
		if err != nil {
			continue
		}
		var s State
		if json.Unmarshal(b, &s) != nil {
			continue
		}
		if s.Status == StatusRunning && !alive(s.ShimPid) && !alive(s.Pid) {
			s.Status = StatusExited
			s.ExitCode = -1
			if s.Error == "" {
				s.Error = "process disappeared (host restarted?)"
			}
		}
		out = append(out, &s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// ParseMemory parses sizes like 512m, 1g, 256Mi or a plain byte count.
func ParseMemory(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"gib", 1 << 30}, {"mib", 1 << 20}, {"kib", 1 << 10}, {"gi", 1 << 30}, {"mi", 1 << 20}, {"ki", 1 << 10},
		{"gb", 1 << 30}, {"mb", 1 << 20}, {"kb", 1 << 10}, {"g", 1 << 30}, {"m", 1 << 20}, {"k", 1 << 10}, {"b", 1}} {
		if strings.HasSuffix(s, u.suf) {
			mult = u.m
			s = strings.TrimSuffix(s, u.suf)
			break
		}
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid memory size %q", s)
	}
	return int64(n * float64(mult)), nil
}

// ParseVolume parses "host:container[:ro]". Relative host paths are resolved
// against base.
func ParseVolume(v, base string) (Volume, error) {
	parts := strings.Split(v, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return Volume{}, fmt.Errorf("volume %q: expected host:container[:ro]", v)
	}
	src := parts[0]
	if !filepath.IsAbs(src) {
		src = filepath.Join(base, src)
	}
	if _, err := os.Stat(src); err != nil {
		return Volume{}, fmt.Errorf("volume %q: %w", v, err)
	}
	if !strings.HasPrefix(parts[1], "/") {
		return Volume{}, fmt.Errorf("volume %q: container path must be absolute", v)
	}
	vol := Volume{Source: src, Target: parts[1]}
	if len(parts) == 3 {
		switch parts[2] {
		case "ro":
			vol.ReadOnly = true
		case "rw":
		default:
			return Volume{}, fmt.Errorf("volume %q: unknown option %q", v, parts[2])
		}
	}
	return vol, nil
}
