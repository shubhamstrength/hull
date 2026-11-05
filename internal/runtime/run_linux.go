//go:build linux

package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// ShimArg is the hidden argument for the process that supervises a detached
// container: it starts it, records its exit code and cleans up.
const ShimArg = "__hull_shim"

// Supported reports whether containers can run here.
func Supported() error {
	if os.Geteuid() != 0 {
		return errors.New("running containers needs root for namespaces, mounts and cgroups: try sudo")
	}
	return nil
}

// NewID returns a random 64-character container ID.
func NewID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Stdio connects a foreground process to the terminal.
type Stdio struct {
	In       io.Reader
	Out, Err io.Writer
}

// Exec runs spec to completion in new namespaces and returns its exit code.
// onStart, if set, is called with the init PID once it has started.
func Exec(spec *Spec, stdio Stdio, onStart func(pid int)) (int, error) {
	if err := Supported(); err != nil {
		return -1, err
	}
	self, err := os.Executable()
	if err != nil {
		return -1, err
	}
	for _, d := range []string{spec.Upper, spec.Work, spec.Rootfs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return -1, err
		}
	}
	r, w, err := os.Pipe()
	if err != nil {
		return -1, err
	}
	cmd := exec.Command(self, InitArg)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.In, stdio.Out, stdio.Err
	cmd.ExtraFiles = []*os.File{r}
	cmd.Env = []string{}
	flags := syscall.CLONE_NEWNS | syscall.CLONE_NEWPID | syscall.CLONE_NEWUTS | syscall.CLONE_NEWIPC
	if spec.Network == "none" {
		flags |= syscall.CLONE_NEWNET
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: uintptr(flags), Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return -1, fmt.Errorf("starting container: %w", err)
	}
	r.Close()

	cg, err := newCgroup(spec.ID, spec.Limits)
	if err == nil {
		err = cg.add(cmd.Process.Pid)
	}
	if err != nil {
		w.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if cg != nil {
			cg.remove()
		}
		return -1, err
	}
	defer cg.remove()

	// The init process blocks until it has the spec, so it cannot start the
	// workload before it is inside its cgroup.
	err = json.NewEncoder(w).Encode(spec)
	w.Close()
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return -1, err
	}
	if onStart != nil {
		onStart(cmd.Process.Pid)
	}

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				_ = cmd.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()
	err = cmd.Wait()
	close(done)
	return exitCode(cmd, err)
}

func exitCode(cmd *exec.Cmd, err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return ee.ExitCode(), nil
	}
	return -1, err
}

// CreateOptions describe a new container.
type CreateOptions struct {
	Name       string
	Image      string
	Digest     string
	Ports      []string
	AutoRemove bool
	Spec       Spec // ID, Upper, Work and Rootfs are filled in by Create
}

// Create records a new container on disk without starting it.
func Create(o CreateOptions) (*State, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	id := NewID()
	if o.Name == "" {
		o.Name = "hull-" + id[:8]
	}
	for _, s := range all {
		if s.Name == o.Name {
			return nil, fmt.Errorf("a container named %q already exists; remove it or pick another --name", o.Name)
		}
	}
	dir := Dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	sp := o.Spec
	sp.ID = id
	sp.Upper = filepath.Join(dir, "upper")
	sp.Work = filepath.Join(dir, "work")
	sp.Rootfs = filepath.Join(dir, "rootfs")
	if sp.Hostname == "" {
		sp.Hostname = id[:12]
	}
	st := &State{
		ID: id, Name: o.Name, Image: o.Image, Digest: o.Digest, Command: sp.Args, Ports: o.Ports,
		Status: StatusCreated, CreatedAt: time.Now().UTC(), AutoRemove: o.AutoRemove, Spec: sp,
	}
	return st, st.Save()
}

// RunForeground starts the container attached to the terminal. Output is also
// appended to the container log so the dashboard can show it.
func RunForeground(st *State, stdio Stdio) (int, error) {
	logf, err := os.OpenFile(LogPath(st.ID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return -1, err
	}
	defer logf.Close()
	stdio.Out = io.MultiWriter(stdio.Out, logf)
	stdio.Err = io.MultiWriter(stdio.Err, logf)
	st.ShimPid = os.Getpid()
	return supervise(st, stdio)
}

func supervise(st *State, stdio Stdio) (int, error) {
	code, err := Exec(&st.Spec, stdio, func(pid int) {
		st.Pid, st.Status, st.StartedAt = pid, StatusRunning, time.Now().UTC()
		st.ExitCode, st.Error, st.FinishedAt = 0, "", time.Time{}
		_ = st.Save()
	})
	st.Status, st.ExitCode, st.FinishedAt, st.Pid, st.ShimPid = StatusExited, code, time.Now().UTC(), 0, 0
	if err != nil {
		st.Error = err.Error()
	}
	if st.AutoRemove {
		_ = os.RemoveAll(Dir(st.ID))
	} else {
		_ = st.Save()
	}
	return code, err
}

// StartDetached starts the container in the background under a shim process
// and returns once it is running (or has already failed).
func StartDetached(st *State) error {
	if err := Supported(); err != nil {
		return err
	}
	if st.Status == StatusRunning {
		return fmt.Errorf("container %s is already running", st.Name)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(LogPath(st.ID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd := exec.Command(self, ShimArg, st.ID)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	st.Status, st.Error = StatusCreated, ""
	if err := st.Save(); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	shimPid := cmd.Process.Pid
	// Reap the shim when it exits so a long-lived caller (the dashboard)
	// does not collect zombies, which would still look alive.
	go func() { _ = cmd.Wait() }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		cur, err := Load(st.ID)
		if err == nil && cur.Status != StatusCreated {
			*st = *cur
			if cur.Status == StatusExited && cur.Error != "" {
				return errors.New(cur.Error)
			}
			return nil
		}
		if err != nil && st.AutoRemove {
			return nil // already ran and was removed
		}
		if !alive(shimPid) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("container %s did not start; see `hull logs %s`", st.Name, st.Name)
}

// Shim is the entry point of the background supervisor process.
func Shim(id string) error {
	st, err := Load(id)
	if err != nil {
		return err
	}
	signal.Ignore(syscall.SIGHUP)
	st.ShimPid = os.Getpid()
	_, err = supervise(st, Stdio{Out: os.Stdout, Err: os.Stderr})
	return err
}

// Stop sends SIGTERM, then SIGKILL after timeout. A process that is PID 1 in
// its namespace ignores SIGTERM unless it handles it, so the kill is often
// what ends it, as with Docker.
func Stop(st *State, timeout time.Duration) error {
	if st.Status != StatusRunning || st.Pid == 0 {
		return nil
	}
	_ = syscall.Kill(st.Pid, syscall.SIGTERM)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !alive(st.Pid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if alive(st.Pid) {
		_ = syscall.Kill(st.Pid, syscall.SIGKILL)
	}
	// Wait for the supervisor to record the exit.
	for i := 0; i < 50; i++ {
		cur, err := Load(st.ID)
		if err != nil || cur.Status != StatusRunning {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// Remove deletes a container and its writable layer.
func Remove(st *State, force bool) error {
	if st.Status == StatusRunning {
		if !force {
			return fmt.Errorf("container %s is running; stop it first or use --force", st.Name)
		}
		if err := Stop(st, 0); err != nil {
			return err
		}
	}
	return os.RemoveAll(Dir(st.ID))
}
