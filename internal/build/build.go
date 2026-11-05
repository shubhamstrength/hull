// Package build turns a hull project into an OCI image.
package build

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/shubhamstrength/hull/internal/archive"
	"github.com/shubhamstrength/hull/internal/config"
	"github.com/shubhamstrength/hull/internal/dockerfile"
	"github.com/shubhamstrength/hull/internal/home"
	"github.com/shubhamstrength/hull/internal/ignore"
	"github.com/shubhamstrength/hull/internal/registry"
	"github.com/shubhamstrength/hull/internal/runtime"
	"github.com/shubhamstrength/hull/internal/store"
)

// Builder builds projects into the local store.
type Builder struct {
	Store *store.Store
	Out   io.Writer
	// NoCache rebuilds RUN steps even when a cached layer exists.
	NoCache bool
	// Pull always fetches base images from the registry, even if stored.
	Pull bool
	// ExtraTags are added to the project's tags.
	ExtraTags []string
	// Platform overrides the project's platform.
	Platform string

	tmp    string
	stages map[string]v1.Image
	roots  map[string]string
}

// Result describes a finished build.
type Result struct {
	Image  v1.Image
	Digest v1.Hash
	Refs   []string
}

// DefaultPlatform is linux on the host's CPU architecture.
func DefaultPlatform() string { return "linux/" + goruntime.GOARCH }

// Build runs every stage and stores the final image under the project's tags.
func (b *Builder) Build(p *config.Project) (*Result, error) {
	if b.Out == nil {
		b.Out = io.Discard
	}
	stages := p.Stages
	if p.Dockerfile != "" {
		df := p.Dockerfile
		if !filepath.IsAbs(df) {
			df = filepath.Join(p.Dir, df)
		}
		res, err := dockerfile.ParseFile(df, p.Args)
		if err != nil {
			return nil, err
		}
		for _, w := range res.Warnings {
			fmt.Fprintf(b.Out, "warning: Dockerfile %s\n", w)
		}
		stages = res.Stages
	}
	platStr := p.Platform
	if b.Platform != "" {
		platStr = b.Platform
	}
	if platStr == "" {
		platStr = DefaultPlatform()
	}
	plat, err := v1.ParsePlatform(platStr)
	if err != nil {
		return nil, fmt.Errorf("platform %q: %w", platStr, err)
	}
	ign, err := ignore.Load(p.ContextDir())
	if err != nil {
		return nil, err
	}
	tmpRoot, err := home.Sub("tmp")
	if err != nil {
		return nil, err
	}
	if b.tmp, err = os.MkdirTemp(tmpRoot, "build-"); err != nil {
		return nil, err
	}
	defer os.RemoveAll(b.tmp)
	b.stages, b.roots = map[string]v1.Image{}, map[string]string{}

	var final v1.Image
	for i, st := range stages {
		label := st.Name
		if label == "" {
			label = fmt.Sprintf("%d", i+1)
		}
		fmt.Fprintf(b.Out, "==> stage %s/%d: FROM %s\n", label, len(stages), st.From)
		img, err := b.runStage(st, *plat, p.ContextDir(), ign)
		if err != nil {
			return nil, fmt.Errorf("stage %s: %w", label, err)
		}
		if st.Name != "" {
			b.stages[st.Name] = img
		}
		final = img
	}

	cfg, err := final.ConfigFile()
	if err != nil {
		return nil, err
	}
	cfg = cfg.DeepCopy()
	cfg.Created = v1.Time{Time: time.Now().UTC()}
	if final, err = mutate.ConfigFile(final, cfg); err != nil {
		return nil, err
	}

	res := &Result{Image: final}
	tags := append(append([]string{}, p.ImageTags()...), b.ExtraTags...)
	for _, t := range tags {
		ref := t
		if !strings.Contains(t, "/") && !strings.Contains(t, ":") {
			ref = p.ImageName() + ":" + t
		}
		if err := b.Store.Put(ref, final); err != nil {
			return nil, fmt.Errorf("saving %s: %w", ref, err)
		}
		res.Refs = append(res.Refs, ref)
	}
	if res.Digest, err = final.Digest(); err != nil {
		return nil, err
	}
	return res, nil
}

func (b *Builder) base(ref string, plat v1.Platform) (v1.Image, error) {
	if ref == "scratch" {
		img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
		img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
		cfg, err := img.ConfigFile()
		if err != nil {
			return nil, err
		}
		cfg = cfg.DeepCopy()
		cfg.OS, cfg.Architecture, cfg.Variant = plat.OS, plat.Architecture, plat.Variant
		return mutate.ConfigFile(img, cfg)
	}
	if img, ok := b.stages[ref]; ok {
		return img, nil
	}
	if !b.Pull {
		if img, err := b.Store.Get(ref); err == nil {
			if cf, err := img.ConfigFile(); err == nil && cf.OS == plat.OS && cf.Architecture == plat.Architecture {
				return img, nil
			}
		}
	}
	fmt.Fprintf(b.Out, "    pulling %s (%s)\n", ref, plat.String())
	remote, err := registry.Pull(ref, plat)
	if err != nil {
		return nil, err
	}
	if err := b.Store.Put(ref, remote); err != nil {
		return nil, err
	}
	return b.Store.Get(ref)
}

func (b *Builder) runStage(st config.Stage, plat v1.Platform, ctxDir string, ign *ignore.Matcher) (v1.Image, error) {
	img, err := b.base(st.From, plat)
	if err != nil {
		return nil, err
	}
	layerType := types.DockerLayer
	if mt, err := img.MediaType(); err == nil && mt == types.OCIManifestSchema1 {
		layerType = types.OCILayer
	}
	cmdSetInStage := false
	for i, step := range st.Steps {
		desc := describe(step)
		fmt.Fprintf(b.Out, "[%d/%d] %s\n", i+1, len(st.Steps), desc)
		cfg, err := img.ConfigFile()
		if err != nil {
			return nil, err
		}
		cfg = cfg.DeepCopy()
		c := &cfg.Config
		hist := v1.History{Created: v1.Time{Time: time.Now().UTC()}, CreatedBy: "hull: " + desc, EmptyLayer: true}

		switch {
		case step.Run != "":
			if img, err = b.run(img, cfg, step.Run, plat, layerType, hist); err != nil {
				return nil, err
			}
			continue
		case step.Copy != nil:
			if img, err = b.copy(img, cfg, step.Copy, ctxDir, ign, layerType, hist); err != nil {
				return nil, err
			}
			continue
		case len(step.Env) > 0:
			for _, k := range config.SortedKeys(step.Env) {
				c.Env = setEnv(c.Env, k, step.Env[k])
			}
		case step.Workdir != "":
			wd := step.Workdir
			if !path.IsAbs(wd) {
				wd = path.Join("/", c.WorkingDir, wd)
			}
			c.WorkingDir = path.Clean(wd)
		case len(step.Expose) > 0:
			if c.ExposedPorts == nil {
				c.ExposedPorts = map[string]struct{}{}
			}
			for _, port := range step.Expose {
				if !strings.Contains(port, "/") {
					port += "/tcp"
				}
				c.ExposedPorts[port] = struct{}{}
			}
		case len(step.Cmd) > 0:
			c.Cmd = step.Cmd
			cmdSetInStage = true
		case len(step.Entrypoint) > 0:
			c.Entrypoint = step.Entrypoint
			if !cmdSetInStage {
				c.Cmd = nil // as with Docker, a new ENTRYPOINT drops the base image's CMD
			}
		case step.User != "":
			c.User = step.User
		case len(step.Label) > 0:
			if c.Labels == nil {
				c.Labels = map[string]string{}
			}
			for k, v := range step.Label {
				c.Labels[k] = v
			}
		}
		cfg.History = append(cfg.History, hist)
		if img, err = mutate.ConfigFile(img, cfg); err != nil {
			return nil, err
		}
	}
	return img, nil
}

func setEnv(env []string, k, v string) []string {
	for i, kv := range env {
		if strings.HasPrefix(kv, k+"=") {
			env[i] = k + "=" + v
			return env
		}
	}
	return append(env, k+"="+v)
}

func describe(s config.Step) string {
	switch {
	case s.Run != "":
		return "RUN " + s.Run
	case s.Copy != nil:
		from := ""
		if s.Copy.From != "" {
			from = "--from=" + s.Copy.From + " "
		}
		return fmt.Sprintf("COPY %s%s %s", from, strings.Join(s.Copy.Src, " "), s.Copy.Dest)
	case len(s.Env) > 0:
		var kv []string
		for _, k := range config.SortedKeys(s.Env) {
			kv = append(kv, k+"="+s.Env[k])
		}
		return "ENV " + strings.Join(kv, " ")
	case s.Workdir != "":
		return "WORKDIR " + s.Workdir
	case len(s.Expose) > 0:
		return "EXPOSE " + strings.Join(s.Expose, " ")
	case len(s.Cmd) > 0:
		return "CMD " + jsonList(s.Cmd)
	case len(s.Entrypoint) > 0:
		return "ENTRYPOINT " + jsonList(s.Entrypoint)
	case s.User != "":
		return "USER " + s.User
	case len(s.Label) > 0:
		return fmt.Sprintf("LABEL (%d)", len(s.Label))
	}
	return "?"
}

func (b *Builder) appendLayer(img v1.Image, file string, layerType types.MediaType, hist v1.History) (v1.Image, v1.Layer, error) {
	layer, err := tarball.LayerFromFile(file, tarball.WithMediaType(layerType))
	if err != nil {
		return nil, nil, err
	}
	hist.EmptyLayer = false
	img, err = mutate.Append(img, mutate.Addendum{Layer: layer, History: hist, MediaType: layerType})
	return img, layer, err
}

// copy adds files from the build context, or from an earlier stage, as a layer.
func (b *Builder) copy(img v1.Image, cfg *v1.ConfigFile, cp *config.Copy, ctxDir string, ign *ignore.Matcher, layerType types.MediaType, hist v1.History) (v1.Image, error) {
	srcRoot := ctxDir
	if cp.From != "" {
		var err error
		if srcRoot, err = b.stageRoot(cp.From); err != nil {
			return nil, err
		}
		ign = nil
	}
	dest := cp.Dest
	if !path.IsAbs(dest) {
		wd := cfg.Config.WorkingDir
		if wd == "" {
			wd = "/"
		}
		dest = path.Join(wd, dest)
	}
	destIsDir := strings.HasSuffix(cp.Dest, "/") || cp.Dest == "." || len(cp.Src) > 1

	type match struct{ abs, rel string }
	var matches []match
	for _, src := range cp.Src {
		rel := strings.TrimPrefix(path.Clean("/"+filepath.ToSlash(src)), "/")
		var full string
		if cp.From != "" {
			var err error
			if full, err = archive.SecureJoin(srcRoot, rel); err != nil {
				return nil, err
			}
		} else {
			if strings.HasPrefix(path.Clean(filepath.ToSlash(src)), "../") {
				return nil, fmt.Errorf("COPY %s: source is outside the build context", src)
			}
			full = filepath.Join(srcRoot, filepath.FromSlash(rel))
		}
		found := []string{full}
		if strings.ContainsAny(rel, "*?[") {
			var err error
			if found, err = filepath.Glob(full); err != nil {
				return nil, err
			}
			if len(found) > 1 {
				destIsDir = true
			}
		}
		if len(found) == 0 {
			return nil, fmt.Errorf("COPY %s: no files match", src)
		}
		for _, f := range found {
			r, _ := filepath.Rel(srcRoot, f)
			matches = append(matches, match{f, filepath.ToSlash(r)})
		}
	}

	var entries []archive.Entry
	for _, m := range matches {
		fi, err := os.Stat(m.abs)
		if err != nil {
			return nil, fmt.Errorf("COPY %s: %w", m.rel, err)
		}
		if fi.IsDir() {
			entries = append(entries, archive.Entry{Src: m.abs, Dest: dest, Root: srcRoot, Ignore: ign})
			continue
		}
		if ign != nil && ign.Excluded(m.rel) {
			return nil, fmt.Errorf("COPY %s: file is excluded by .hullignore/.dockerignore", m.rel)
		}
		d := dest
		if destIsDir {
			d = path.Join(dest, path.Base(m.rel))
		}
		entries = append(entries, archive.Entry{Src: m.abs, Dest: d})
	}

	f, err := os.CreateTemp(b.tmp, "copy-*.tar")
	if err != nil {
		return nil, err
	}
	if err := archive.WriteTar(f, entries); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	img, _, err = b.appendLayer(img, f.Name(), layerType, hist)
	return img, err
}

// stageRoot unpacks an earlier stage into a plain directory so files can be
// copied out of it. This works without root, which is what lets a copy-only
// final stage be assembled on macOS.
func (b *Builder) stageRoot(name string) (string, error) {
	if dir, ok := b.roots[name]; ok {
		return dir, nil
	}
	img, ok := b.stages[name]
	if !ok {
		return "", fmt.Errorf("unknown stage %q", name)
	}
	dir := filepath.Join(b.tmp, "stage-"+name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	layers, err := img.Layers()
	if err != nil {
		return "", err
	}
	for _, l := range layers {
		rc, err := l.Uncompressed()
		if err != nil {
			return "", err
		}
		err = archive.Unpack(rc, dir, archive.Flatten)
		rc.Close()
		if err != nil {
			return "", err
		}
	}
	b.roots[name] = dir
	return dir, nil
}

// run executes a RUN step in a container on top of img and captures the
// changes it makes as a new layer.
func (b *Builder) run(img v1.Image, cfg *v1.ConfigFile, cmd string, plat v1.Platform, layerType types.MediaType, hist v1.History) (v1.Image, error) {
	if goruntime.GOOS != "linux" || plat.OS != "linux" {
		return nil, errors.New("RUN steps need Linux. On macOS, run hull build inside a Linux VM, or build your app first and use a COPY-only final stage")
	}
	if plat.Architecture != goruntime.GOARCH {
		return nil, fmt.Errorf("RUN steps run natively, so the platform (%s) must match this machine (%s/%s)", plat.String(), goruntime.GOOS, goruntime.GOARCH)
	}
	if err := runtime.Supported(); err != nil {
		return nil, err
	}
	// The cache key covers what the step can see: the filesystem (the layer
	// chain) plus its command, environment, working directory and user.
	keyData, _ := json.Marshal(struct {
		Layers             []v1.Hash
		Cmd, Workdir, User string
		Env                []string
		Platform           string
	}{cfg.RootFS.DiffIDs, cmd, cfg.Config.WorkingDir, cfg.Config.User, cfg.Config.Env, plat.String()})
	sum := sha256.Sum256(keyData)
	key := hex.EncodeToString(sum[:])
	cacheDir, err := home.Sub("cache", "run")
	if err != nil {
		return nil, err
	}
	cached := filepath.Join(cacheDir, key+".tar")
	if _, err := os.Stat(cached); err == nil && !b.NoCache {
		fmt.Fprintln(b.Out, "    CACHED")
		img, _, err := b.appendLayer(img, cached, layerType, hist)
		return img, err
	}

	layersDir, names, err := runtime.EnsureLayers(img)
	if err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(b.tmp, "run-")
	if err != nil {
		return nil, err
	}
	spec := &runtime.Spec{
		ID:         "build-" + key[:12],
		Hostname:   "hull-build",
		LayersDir:  layersDir,
		Layers:     names,
		Upper:      filepath.Join(work, "upper"),
		Work:       filepath.Join(work, "work"),
		Rootfs:     filepath.Join(work, "rootfs"),
		Args:       []string{"/bin/sh", "-c", cmd},
		Env:        append(append([]string{}, cfg.Config.Env...), proxyEnv()...),
		Workdir:    cfg.Config.WorkingDir,
		User:       cfg.Config.User,
		Network:    "host",
		ResolvConf: true,
	}
	code, err := runtime.Exec(spec, runtime.Stdio{Out: b.Out, Err: b.Out}, nil)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("RUN %s: exited with code %d", cmd, code)
	}

	// Leave out mount points hull itself created, if no layer below had them.
	skip := map[string]bool{}
	for _, p := range []string{"etc/resolv.conf", "proc", "sys", "dev"} {
		inLower := false
		for _, n := range names {
			if _, err := os.Lstat(filepath.Join(layersDir, n, p)); err == nil {
				inLower = true
				break
			}
		}
		if !inLower {
			skip[p] = true
		}
	}
	tmpTar := cached + ".tmp"
	f, err := os.Create(tmpTar)
	if err != nil {
		return nil, err
	}
	if err := archive.WriteUpperTar(f, spec.Upper, skip); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpTar, cached); err != nil {
		return nil, err
	}
	img, layer, err := b.appendLayer(img, cached, layerType, hist)
	if err != nil {
		return nil, err
	}
	if diffID, err := layer.DiffID(); err == nil {
		// Keep the cached overlay directory identical to the layer tarball.
		for p := range skip {
			_ = os.RemoveAll(filepath.Join(spec.Upper, p))
		}
		_ = runtime.AdoptLayer(diffID, spec.Upper)
	}
	return img, nil
}

// proxyEnv passes the host's proxy settings to RUN steps, as Docker does,
// without saving them in the image.
func proxyEnv() []string {
	var out []string
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
		if v := os.Getenv(k); v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}

func jsonList(l []string) string {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(l)
	return strings.TrimSpace(sb.String())
}
