// Command hull is a lightweight, daemonless container tool: it builds OCI
// images from a per-project hull.yaml (or a Dockerfile), runs them with Linux
// namespaces and cgroups, and pushes them to any OCI registry.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/shubhamstrength/hull/internal/build"
	"github.com/shubhamstrength/hull/internal/config"
	"github.com/shubhamstrength/hull/internal/engine"
	"github.com/shubhamstrength/hull/internal/registry"
	"github.com/shubhamstrength/hull/internal/runtime"
	"github.com/shubhamstrength/hull/internal/store"
	"github.com/shubhamstrength/hull/internal/ui"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `hull: build, run and ship OCI containers

Usage: hull <command> [flags] [args]

Project
  init        Create a hull.yaml for the current project
  build       Build the project's image (from hull.yaml or a Dockerfile)

Containers (Linux, as root)
  run         Run a container from an image
  ps          List containers
  start       Start stopped containers
  stop        Stop running containers
  logs        Show a container's output
  rm          Remove containers

Images
  images      List local images
  pull        Pull an image from a registry
  push        Push an image to a registry
  tag         Give an image another name
  rmi         Remove local images
  save        Write an image to a tar file (loadable with docker load)
  inspect     Show an image's or container's details
  login       Save credentials for a registry

Other
  ui          Open the web dashboard
  version     Print the version

Run 'hull <command> -h' for a command's flags.
`

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case runtime.InitArg:
			if err := runtime.Init(); err != nil {
				fmt.Fprintln(os.Stderr, "hull: container init:", err)
				os.Exit(127)
			}
			return
		case runtime.ShimArg:
			if len(os.Args) < 3 {
				os.Exit(2)
			}
			if err := runtime.Shim(os.Args[2]); err != nil {
				fmt.Fprintln(os.Stderr, "hull:", err)
				os.Exit(1)
			}
			return
		}
	}
	code, err := dispatch(os.Args[1:])
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "hull:", err)
		}
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func dispatch(args []string) (int, error) {
	if len(args) == 0 {
		fmt.Print(usage)
		return 2, nil
	}
	cmd, rest := args[0], args[1:]
	cmds := map[string]func([]string) error{
		"init": cmdInit, "build": cmdBuild, "ps": cmdPs, "start": cmdStart, "stop": cmdStop,
		"logs": cmdLogs, "rm": cmdRm, "images": cmdImages, "pull": cmdPull, "push": cmdPush,
		"tag": cmdTag, "rmi": cmdRmi, "save": cmdSave, "inspect": cmdInspect, "login": cmdLogin,
		"ui":      cmdUI,
		"version": func([]string) error { fmt.Println("hull", version); return nil },
	}
	switch cmd {
	case "run":
		return cmdRun(rest)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0, nil
	}
	fn, ok := cmds[cmd]
	if !ok {
		return 2, fmt.Errorf("unknown command %q (see hull --help)", cmd)
	}
	return 0, fn(rest)
}

// parse parses flags that may appear before or after positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func newFlags(name, args, desc string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: hull %s %s\n\n%s\n", name, args, desc)
		if hasFlags(fs) {
			fmt.Fprintln(fs.Output(), "\nFlags:")
			fs.PrintDefaults()
		}
	}
	return fs
}

func hasFlags(fs *flag.FlagSet) bool {
	n := 0
	fs.VisitAll(func(*flag.Flag) { n++ })
	return n > 0
}

func loadProject(file string) (*config.Project, error) {
	p, err := config.Load(file)
	if os.IsNotExist(errors.Unwrap(err)) || os.IsNotExist(err) {
		return nil, fmt.Errorf("no %s here; run `hull init` first or pass -f", file)
	}
	return p, err
}

func openStore() *store.Store {
	s, err := store.Open()
	if err != nil {
		fmt.Fprintln(os.Stderr, "hull: opening image store:", err)
		os.Exit(1)
	}
	return s
}

func cmdBuild(args []string) error {
	fs := newFlags("build", "[flags]", "Build the image described by hull.yaml and store it locally.")
	file := fs.String("f", config.DefaultFile, "project file")
	var tags multi
	fs.Var(&tags, "t", "extra tag or full image reference (repeatable)")
	noCache := fs.Bool("no-cache", false, "rebuild RUN steps instead of using cached layers")
	pull := fs.Bool("pull", false, "always pull base images, even if stored locally")
	platform := fs.String("platform", "", "target platform, e.g. linux/amd64 (default from hull.yaml, else this machine)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	p, err := loadProject(*file)
	if err != nil {
		return err
	}
	b := &build.Builder{Store: openStore(), Out: os.Stdout, NoCache: *noCache, Pull: *pull, ExtraTags: tags, Platform: *platform}
	start := time.Now()
	res, err := b.Build(p)
	if err != nil {
		return err
	}
	fmt.Printf("\nBuilt %s in %s\n", res.Digest.String()[:19], time.Since(start).Round(100*time.Millisecond))
	for _, r := range res.Refs {
		fmt.Println("  tagged", r)
	}
	return nil
}

func cmdRun(args []string) (int, error) {
	fs := newFlags("run", "[flags] [image] [command...]",
		"Run a container. With no image, runs this project's image using the run: settings in hull.yaml.\nFlags must come before the image; anything after the image is the command.")
	detach := fs.Bool("d", false, "run in the background")
	nameFlag := fs.String("name", "", "container name")
	file := fs.String("f", config.DefaultFile, "project file used when no image is given")
	var ports, envs, vols multi
	fs.Var(&ports, "p", "publish a port, host:container (repeatable; containers share the host network)")
	fs.Var(&envs, "e", "set an environment variable, KEY=value (repeatable)")
	fs.Var(&vols, "v", "bind mount, /host/path:/container/path[:ro] (repeatable)")
	mem := fs.String("m", "", "memory limit, e.g. 256m or 1g")
	cpus := fs.Float64("cpus", 0, "CPU limit, e.g. 0.5")
	pids := fs.Int64("pids", 0, "maximum number of processes")
	network := fs.String("network", "", "host (default) or none")
	rm := fs.Bool("rm", false, "remove the container when it exits")
	interactive := fs.Bool("i", false, "keep stdin open")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	rest := fs.Args()

	var proj *config.Project
	var image string
	if len(rest) > 0 {
		image, rest = rest[0], rest[1:]
	} else {
		p, err := loadProject(*file)
		if err != nil {
			return 2, fmt.Errorf("no image given and %v", err)
		}
		proj = p
		image = p.ImageName() + ":" + p.ImageTags()[0]
	}
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	opts := runOptions{
		Image: image, Name: *nameFlag, Cmd: rest, Ports: ports, Env: envs, Volumes: vols,
		Memory: *mem, CPUs: *cpus, Pids: *pids, Network: *network, Detach: *detach, Remove: *rm,
	}
	if proj != nil {
		opts.ApplyProject(proj)
	}
	st, err := createContainer(openStore(), opts, os.Stdout)
	if err != nil {
		return 125, err
	}
	if *detach {
		if err := runtime.StartDetached(st); err != nil {
			return 125, err
		}
		fmt.Println(st.Name, st.ID[:12])
		return 0, nil
	}
	// If our output is piped into something like `head`, keep going so the
	// container's exit is still recorded instead of dying on SIGPIPE.
	signal.Ignore(syscall.SIGPIPE)
	stdio := runtime.Stdio{Out: os.Stdout, Err: os.Stderr}
	if *interactive {
		stdio.In = os.Stdin
	}
	code, err := runtime.RunForeground(st, stdio)
	if err != nil {
		return 125, err
	}
	return code, nil
}

type runOptions = engine.RunOptions

var createContainer = engine.CreateContainer

func cmdPs(args []string) error {
	fs := newFlags("ps", "[-a]", "List containers. Only running ones are shown unless -a is given.")
	all := fs.Bool("a", false, "show all containers")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	list, err := runtime.List()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tID\tIMAGE\tSTATUS\tPORTS\tCOMMAND")
	for _, s := range list {
		if !*all && s.Status != runtime.StatusRunning {
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, s.ID[:12], store.Short(s.Image), statusText(s),
			strings.Join(s.Ports, ","), truncate(strings.Join(s.Command, " "), 40))
	}
	return w.Flush()
}

func statusText(s *runtime.State) string {
	switch s.Status {
	case runtime.StatusRunning:
		return "Up " + ago(s.StartedAt)
	case runtime.StatusExited:
		return fmt.Sprintf("Exited (%d) %s ago", s.ExitCode, ago(s.FinishedAt))
	}
	return "Created"
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func forEach(args []string, fn func(*runtime.State) error) error {
	if len(args) == 0 {
		return errors.New("name at least one container")
	}
	var errs []error
	for _, a := range args {
		st, err := runtime.Load(a)
		if err == nil {
			err = fn(st)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a, err))
			continue
		}
		fmt.Println(a)
	}
	return errors.Join(errs...)
}

func cmdStart(args []string) error {
	fs := newFlags("start", "<container>...", "Start stopped containers in the background.")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	return forEach(pos, runtime.StartDetached)
}

func cmdStop(args []string) error {
	fs := newFlags("stop", "[-t seconds] <container>...", "Stop containers: SIGTERM, then SIGKILL after the timeout.")
	t := fs.Int("t", 10, "seconds to wait before killing")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	return forEach(pos, func(s *runtime.State) error { return runtime.Stop(s, time.Duration(*t)*time.Second) })
}

func cmdRm(args []string) error {
	fs := newFlags("rm", "[-f] <container>...", "Remove containers and their writable layers.")
	force := fs.Bool("f", false, "stop running containers first")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	return forEach(pos, func(s *runtime.State) error { return runtime.Remove(s, *force) })
}

func cmdLogs(args []string) error {
	fs := newFlags("logs", "[-f] [--tail n] <container>", "Show a container's stdout and stderr.")
	follow := fs.Bool("f", false, "keep printing new output until the container exits")
	tail := fs.Int("tail", 0, "only show the last n lines")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("name one container")
	}
	st, err := runtime.Load(pos[0])
	if err != nil {
		return err
	}
	f, err := os.Open(runtime.LogPath(st.ID))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if *tail > 0 {
		lines, err := engine.TailLines(runtime.LogPath(st.ID), *tail)
		if err != nil {
			return err
		}
		os.Stdout.Write(lines)
		f.Seek(0, io.SeekEnd)
	} else if _, err := io.Copy(os.Stdout, f); err != nil {
		return err
	}
	for *follow {
		if _, err := io.Copy(os.Stdout, f); err != nil {
			return err
		}
		cur, err := runtime.Load(st.ID)
		if err != nil || cur.Status != runtime.StatusRunning {
			_, _ = io.Copy(os.Stdout, f)
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return nil
}

func cmdImages(args []string) error {
	fs := newFlags("images", "", "List images in the local store.")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	list, err := openStore().List()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	fmt.Fprintln(w, "IMAGE\tDIGEST\tPLATFORM\tCREATED\tSIZE")
	for _, i := range list {
		fmt.Fprintf(w, "%s\t%s\t%s/%s\t%s ago\t%s\n", i.Ref, strings.TrimPrefix(i.Digest, "sha256:")[:12], i.OS, i.Arch, ago(i.Created), engine.HumanSize(i.Size))
	}
	return w.Flush()
}

func cmdPull(args []string) error {
	fs := newFlags("pull", "[--platform p] <image>", "Pull an image from its registry into the local store.")
	platform := fs.String("platform", build.DefaultPlatform(), "platform to pull")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("name one image")
	}
	plat, err := v1.ParsePlatform(*platform)
	if err != nil {
		return err
	}
	fmt.Printf("Pulling %s...\n", pos[0])
	img, err := registry.Pull(pos[0], *plat)
	if err != nil {
		return err
	}
	if err := openStore().Put(pos[0], img); err != nil {
		return err
	}
	d, _ := img.Digest()
	fmt.Println("Pulled", pos[0], d)
	return nil
}

func cmdPush(args []string) error {
	fs := newFlags("push", "[image...]", "Push images to their registries. With no image, pushes every tag of this project.")
	file := fs.String("f", config.DefaultFile, "project file used when no image is given")
	digestFile := fs.String("digest-file", "", "write the pushed repository@digest to this file (for Terraform or CI)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		p, err := loadProject(*file)
		if err != nil {
			return err
		}
		for _, t := range p.ImageTags() {
			pos = append(pos, p.ImageName()+":"+t)
		}
	}
	st := openStore()
	for _, ref := range pos {
		img, err := st.Get(ref)
		if err != nil {
			return err
		}
		fmt.Printf("Pushing %s\n", ref)
		updates := make(chan v1.Update, 16)
		done := make(chan struct{})
		go func() {
			defer close(done)
			last := time.Time{}
			for u := range updates {
				if u.Total > 0 && time.Since(last) > 500*time.Millisecond {
					fmt.Printf("\r  %s / %s", engine.HumanSize(u.Complete), engine.HumanSize(u.Total))
					last = time.Now()
				}
			}
		}()
		err = registry.Push(ref, img, updates)
		<-done
		if err != nil {
			fmt.Println()
			return err
		}
		d, _ := img.Digest()
		r, _ := name.ParseReference(ref)
		pinned := fmt.Sprintf("%s@%s", r.Context().Name(), d)
		fmt.Printf("\r\033[K  pushed %s\n", pinned)
		if *digestFile != "" {
			if err := os.WriteFile(*digestFile, []byte(pinned+"\n"), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

func cmdTag(args []string) error {
	fs := newFlags("tag", "<image> <new-name>", "Store an existing image under another name, e.g. before pushing to a registry.")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errors.New("usage: hull tag <image> <new-name>")
	}
	st := openStore()
	img, err := st.Get(pos[0])
	if err != nil {
		return err
	}
	return st.Put(pos[1], img)
}

func cmdRmi(args []string) error {
	fs := newFlags("rmi", "<image>...", "Remove images from the local store.")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("name at least one image")
	}
	st := openStore()
	var errs []error
	for _, r := range pos {
		if err := st.Remove(r); err != nil {
			errs = append(errs, err)
			continue
		}
		fmt.Println("Removed", r)
	}
	return errors.Join(errs...)
}

func cmdSave(args []string) error {
	fs := newFlags("save", "-o file.tar <image>", "Write an image to a tar file that `docker load` and other tools can read.")
	out := fs.String("o", "", "output file")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *out == "" {
		return errors.New("usage: hull save -o file.tar <image>")
	}
	img, err := openStore().Get(pos[0])
	if err != nil {
		return err
	}
	tag, err := name.NewTag(pos[0])
	if err != nil {
		return err
	}
	if err := tarball.WriteToFile(*out, tag, img); err != nil {
		return err
	}
	fmt.Println("Wrote", *out)
	return nil
}

func cmdInspect(args []string) error {
	fs := newFlags("inspect", "<image|container>", "Print an image's config or a container's state as JSON.")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("name one image or container")
	}
	if st, err := runtime.Load(pos[0]); err == nil {
		return printJSON(st)
	}
	img, err := openStore().Get(pos[0])
	if err != nil {
		return err
	}
	b, err := store.Inspect(img)
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func cmdLogin(args []string) error {
	fs := newFlags("login", "-u user [--password-stdin] <registry>",
		"Check and save registry credentials, e.g. hull login ghcr.io -u me --password-stdin < token.txt")
	user := fs.String("u", "", "username")
	stdin := fs.Bool("password-stdin", false, "read the password or token from stdin")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *user == "" {
		return errors.New("usage: hull login -u user --password-stdin <registry>")
	}
	var pass string
	if *stdin {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		pass = strings.TrimSpace(string(b))
	} else {
		fmt.Fprint(os.Stderr, "Password: ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return err
		}
		pass = strings.TrimSpace(line)
	}
	if err := registry.Login(pos[0], *user, pass); err != nil {
		return err
	}
	fmt.Println("Login succeeded for", pos[0])
	return nil
}

func cmdUI(args []string) error {
	fs := newFlags("ui", "[--addr host:port]", "Serve the web dashboard for managing containers and images.")
	addr := fs.String("addr", "127.0.0.1:7070", "address to listen on (keep it on localhost)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	return ui.Serve(*addr, version, os.Stdout)
}

func cmdInit(args []string) error {
	fs := newFlags("init", "[--template t] [--name n]", "Create hull.yaml and .hullignore for this project.\nTemplates: auto (default), react, static, node, dockerfile.")
	tmpl := fs.String("template", "auto", "project template")
	nameFlag := fs.String("name", "", "project name (default: directory name)")
	force := fs.Bool("force", false, "overwrite an existing hull.yaml")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	if _, err := os.Stat(config.DefaultFile); err == nil && !*force {
		return errors.New("hull.yaml already exists (use --force to overwrite)")
	}
	n := *nameFlag
	if n == "" {
		n = sanitizeName(filepath.Base(dir))
	}
	content, kind, err := initTemplate(dir, *tmpl, n)
	if err != nil {
		return err
	}
	if err := os.WriteFile(config.DefaultFile, []byte(content), 0o644); err != nil {
		return err
	}
	if _, err := os.Stat(".hullignore"); os.IsNotExist(err) {
		if _, err := os.Stat(".dockerignore"); os.IsNotExist(err) {
			_ = os.WriteFile(".hullignore", []byte(defaultIgnore), 0o644)
		}
	}
	fmt.Printf("Created hull.yaml (%s template). Next: hull build\n", kind)
	return nil
}
