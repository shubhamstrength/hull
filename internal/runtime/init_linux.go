//go:build linux

package runtime

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/shubhamstrength/hull/internal/archive"
)

// InitArg is the hidden argument hull re-executes itself with to become a
// container's init process.
const InitArg = "__hull_init"

// Init is the container side of a run. It is called in the new namespaces,
// reads the Spec from fd 3, sets up the root filesystem and execs the command.
// It only returns on error.
func Init() error {
	f := os.NewFile(3, "spec")
	var spec Spec
	if err := json.NewDecoder(f).Decode(&spec); err != nil {
		return fmt.Errorf("reading spec: %w", err)
	}
	f.Close()

	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("making mounts private: %w", err)
	}
	if err := mountRootfs(&spec); err != nil {
		return err
	}
	if err := unix.Sethostname([]byte(spec.Hostname)); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}
	if spec.Network == "none" {
		_ = loopbackUp()
	}
	if err := pivot(spec.Rootfs); err != nil {
		return err
	}
	return execProcess(&spec)
}

func mountRootfs(spec *Spec) error {
	root := spec.Rootfs
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	lowers := make([]string, 0, len(spec.Layers))
	for i := len(spec.Layers) - 1; i >= 0; i-- {
		lowers = append(lowers, spec.Layers[i])
	}
	if len(lowers) == 0 {
		// overlayfs needs at least one lower directory.
		empty := filepath.Join(filepath.Dir(spec.Upper), "empty")
		if err := os.MkdirAll(empty, 0o755); err != nil {
			return err
		}
		lowers = []string{empty}
	}
	// Lower directories are given relative to the layer cache to keep the
	// option string short; mount resolves them against the working directory.
	if err := os.Chdir(spec.LayersDir); err != nil {
		return err
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", strings.Join(lowers, ":"), spec.Upper, spec.Work)
	if err := unix.Mount("overlay", root, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mounting overlay root filesystem: %w", err)
	}

	for _, v := range spec.Volumes {
		target, err := archive.SecureJoin(root, v.Target)
		if err != nil {
			return err
		}
		fi, err := os.Stat(v.Source)
		if err != nil {
			return err
		}
		if err := ensureMountpoint(target, fi.IsDir()); err != nil {
			return err
		}
		if err := unix.Mount(v.Source, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("mounting volume %s: %w", v.Target, err)
		}
		if v.ReadOnly {
			if err := unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_REC, ""); err != nil {
				return fmt.Errorf("making volume %s read-only: %w", v.Target, err)
			}
		}
	}

	if spec.ResolvConf {
		if _, err := os.Stat("/etc/resolv.conf"); err == nil {
			target, _ := archive.SecureJoin(root, "/etc/resolv.conf")
			if ensureMountpoint(target, false) == nil {
				_ = unix.Mount("/etc/resolv.conf", target, "", unix.MS_BIND, "")
				_ = unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, "")
			}
		}
	}

	// /proc for the new PID namespace.
	proc := filepath.Join(root, "proc")
	if err := os.MkdirAll(proc, 0o555); err != nil {
		return err
	}
	if err := unix.Mount("proc", proc, "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mounting /proc: %w", err)
	}
	// /sys read-only.
	sys := filepath.Join(root, "sys")
	if err := os.MkdirAll(sys, 0o555); err != nil {
		return err
	}
	if err := unix.Mount("sysfs", sys, "sysfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC|unix.MS_RDONLY, ""); err != nil {
		if err := unix.Mount("/sys", sys, "", unix.MS_BIND|unix.MS_REC, ""); err == nil {
			_ = unix.Mount("", sys, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_REC, "")
		}
	}
	return setupDev(root)
}

// setupDev gives the container a minimal /dev with only safe devices.
func setupDev(root string) error {
	dev := filepath.Join(root, "dev")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", dev, "tmpfs", unix.MS_NOSUID|unix.MS_STRICTATIME, "mode=755,size=65536k"); err != nil {
		return fmt.Errorf("mounting /dev: %w", err)
	}
	for _, d := range []string{"null", "zero", "full", "random", "urandom", "tty"} {
		target := filepath.Join(dev, d)
		if err := ensureMountpoint(target, false); err != nil {
			return err
		}
		if err := unix.Mount("/dev/"+d, target, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("binding /dev/%s: %w", d, err)
		}
	}
	pts := filepath.Join(dev, "pts")
	if err := os.MkdirAll(pts, 0o755); err != nil {
		return err
	}
	if err := unix.Mount("devpts", pts, "devpts", unix.MS_NOSUID|unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620"); err == nil {
		_ = os.Symlink("pts/ptmx", filepath.Join(dev, "ptmx"))
	}
	shm := filepath.Join(dev, "shm")
	if err := os.MkdirAll(shm, 0o1777); err != nil {
		return err
	}
	_ = unix.Mount("shm", shm, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=1777,size=65536k")
	for link, target := range map[string]string{
		"fd": "/proc/self/fd", "stdin": "/proc/self/fd/0", "stdout": "/proc/self/fd/1", "stderr": "/proc/self/fd/2",
	} {
		_ = os.Symlink(target, filepath.Join(dev, link))
	}
	return nil
}

func ensureMountpoint(p string, dir bool) error {
	if dir {
		return os.MkdirAll(p, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if _, err := os.Lstat(p); err == nil {
		return nil
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// pivot makes root the new "/" and detaches the host filesystem, using the
// pivot_root(".", ".") technique so no extra directory is needed.
func pivot(root string) error {
	if err := unix.Chdir(root); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detaching host root: %w", err)
	}
	return unix.Chdir("/")
}

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func execProcess(spec *Spec) error {
	env := spec.Env
	if getenv(env, "PATH") == "" {
		env = append(env, "PATH="+defaultPath)
	}
	uid, gid, homeDir, err := lookupUser(spec.User)
	if err != nil {
		return err
	}
	if getenv(env, "HOME") == "" {
		env = append(env, "HOME="+homeDir)
	}
	if getenv(env, "HOSTNAME") == "" {
		env = append(env, "HOSTNAME="+spec.Hostname)
	}
	wd := spec.Workdir
	if wd == "" {
		wd = "/"
	}
	if err := os.MkdirAll(wd, 0o755); err != nil {
		return err
	}
	if err := os.Chdir(wd); err != nil {
		return err
	}
	if len(spec.Args) == 0 {
		return fmt.Errorf("no command to run: the image has no CMD or ENTRYPOINT, so pass one")
	}
	if err := dropCapabilities(); err != nil {
		return err
	}
	if err := unix.Setgroups([]int{gid}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := unix.Setgid(gid); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := unix.Setuid(uid); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
	os.Clearenv()
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		os.Setenv(k, v)
	}
	bin := spec.Args[0]
	if !strings.Contains(bin, "/") {
		if bin, err = exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s: not found in the image's PATH", spec.Args[0])
		}
	}
	if err := syscall.Exec(bin, spec.Args, env); err != nil {
		return fmt.Errorf("exec %s: %w", spec.Args[0], err)
	}
	return nil
}

func getenv(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
			return v
		}
	}
	return ""
}

// lookupUser resolves "user", "uid", "user:group" or "uid:gid" using the
// image's /etc/passwd and /etc/group.
func lookupUser(u string) (uid, gid int, homeDir string, err error) {
	if u == "" {
		return 0, 0, "/root", nil
	}
	userPart, groupPart, hasGroup := strings.Cut(u, ":")
	uid, gid, homeDir = -1, -1, "/"
	if n, err := strconv.Atoi(userPart); err == nil {
		uid = n
	}
	for _, rec := range readColonFile("/etc/passwd") {
		if len(rec) < 6 {
			continue
		}
		recUID, _ := strconv.Atoi(rec[2])
		if rec[0] == userPart || (uid >= 0 && recUID == uid) {
			uid = recUID
			gid, _ = strconv.Atoi(rec[3])
			homeDir = rec[5]
			break
		}
	}
	if uid < 0 {
		return 0, 0, "", fmt.Errorf("user %q not found in the image's /etc/passwd", userPart)
	}
	if hasGroup {
		gid = -1
		if n, err := strconv.Atoi(groupPart); err == nil {
			gid = n
		} else {
			for _, rec := range readColonFile("/etc/group") {
				if len(rec) >= 3 && rec[0] == groupPart {
					gid, _ = strconv.Atoi(rec[2])
				}
			}
		}
		if gid < 0 {
			return 0, 0, "", fmt.Errorf("group %q not found in the image's /etc/group", groupPart)
		}
	}
	if gid < 0 {
		gid = uid
	}
	return uid, gid, homeDir, nil
}

func readColonFile(p string) [][]string {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, strings.Split(line, ":"))
		}
	}
	return out
}

// defaultCaps is the same small capability set Docker grants by default.
var defaultCaps = []uintptr{
	unix.CAP_CHOWN, unix.CAP_DAC_OVERRIDE, unix.CAP_FSETID, unix.CAP_FOWNER, unix.CAP_MKNOD,
	unix.CAP_NET_RAW, unix.CAP_SETGID, unix.CAP_SETUID, unix.CAP_SETFCAP, unix.CAP_SETPCAP,
	unix.CAP_NET_BIND_SERVICE, unix.CAP_SYS_CHROOT, unix.CAP_KILL, unix.CAP_AUDIT_WRITE,
}

func dropCapabilities() error {
	keep := map[uintptr]bool{}
	var mask [2]uint32
	for _, c := range defaultCaps {
		keep[c] = true
		mask[c/32] |= 1 << (c % 32)
	}
	last := uintptr(unix.CAP_LAST_CAP)
	if b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			last = uintptr(n)
		}
	}
	for c := uintptr(0); c <= last; c++ {
		if !keep[c] {
			if err := unix.Prctl(unix.PR_CAPBSET_DROP, c, 0, 0, 0); err != nil && err != unix.EINVAL {
				return fmt.Errorf("dropping capability %d: %w", c, err)
			}
		}
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{
		{Effective: mask[0], Permitted: mask[0], Inheritable: mask[0]},
		{Effective: mask[1], Permitted: mask[1], Inheritable: mask[1]},
	}
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return fmt.Errorf("capset: %w", err)
	}
	return nil
}

func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}
