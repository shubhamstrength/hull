//go:build linux

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const cgroupRoot = "/sys/fs/cgroup"

type cgroup struct{ dirs []string }

func cgroupV2() bool {
	_, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers"))
	return err == nil
}

// newCgroup creates a cgroup for the container with the given limits. It
// supports cgroup v2 and falls back to the v1 memory, cpu and pids controllers.
func newCgroup(id string, l Limits) (*cgroup, error) {
	if l.MemoryBytes == 0 && l.CPUs == 0 && l.Pids == 0 {
		return &cgroup{}, nil
	}
	write := func(dir, file, val string) error {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(val), 0o644); err != nil {
			return fmt.Errorf("cgroup %s: %w", file, err)
		}
		return nil
	}
	cg := &cgroup{}
	if cgroupV2() {
		parent := filepath.Join(cgroupRoot, "hull")
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return nil, err
		}
		_ = write(cgroupRoot, "cgroup.subtree_control", "+cpu +memory +pids")
		_ = write(parent, "cgroup.subtree_control", "+cpu +memory +pids")
		dir := filepath.Join(parent, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		cg.dirs = []string{dir}
		if l.MemoryBytes > 0 {
			if err := write(dir, "memory.max", strconv.FormatInt(l.MemoryBytes, 10)); err != nil {
				return cg, err
			}
			_ = write(dir, "memory.swap.max", "0")
		}
		if l.CPUs > 0 {
			if err := write(dir, "cpu.max", fmt.Sprintf("%d 100000", int64(l.CPUs*100000))); err != nil {
				return cg, err
			}
		}
		if l.Pids > 0 {
			if err := write(dir, "pids.max", strconv.FormatInt(l.Pids, 10)); err != nil {
				return cg, err
			}
		}
		return cg, nil
	}
	mk := func(ctrl string) (string, error) {
		dir := filepath.Join(cgroupRoot, ctrl, "hull", id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("cgroup v1 %s: %w", ctrl, err)
		}
		cg.dirs = append(cg.dirs, dir)
		return dir, nil
	}
	if l.MemoryBytes > 0 {
		dir, err := mk("memory")
		if err != nil {
			return cg, err
		}
		if err := write(dir, "memory.limit_in_bytes", strconv.FormatInt(l.MemoryBytes, 10)); err != nil {
			return cg, err
		}
	}
	if l.CPUs > 0 {
		dir, err := mk("cpu")
		if err != nil {
			return cg, err
		}
		if err := write(dir, "cpu.cfs_period_us", "100000"); err != nil {
			return cg, err
		}
		if err := write(dir, "cpu.cfs_quota_us", strconv.FormatInt(int64(l.CPUs*100000), 10)); err != nil {
			return cg, err
		}
	}
	if l.Pids > 0 {
		dir, err := mk("pids")
		if err != nil {
			return cg, err
		}
		if err := write(dir, "pids.max", strconv.FormatInt(l.Pids, 10)); err != nil {
			return cg, err
		}
	}
	return cg, nil
}

func (c *cgroup) add(pid int) error {
	for _, d := range c.dirs {
		if err := os.WriteFile(filepath.Join(d, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
			return fmt.Errorf("joining cgroup: %w", err)
		}
	}
	return nil
}

func (c *cgroup) remove() {
	for _, d := range c.dirs {
		for i := 0; i < 20; i++ {
			if err := os.Remove(d); err == nil || os.IsNotExist(err) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}
