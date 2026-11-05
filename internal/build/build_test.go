package build

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/shubhamstrength/hull/internal/config"
	"github.com/shubhamstrength/hull/internal/store"
)

// A COPY-only build from scratch needs no network and no root, so it runs
// everywhere, including macOS.
func TestCopyOnlyBuild(t *testing.T) {
	t.Setenv("HULL_HOME", t.TempDir())
	ctx := t.TempDir()
	os.MkdirAll(filepath.Join(ctx, "dist/assets"), 0o755)
	os.WriteFile(filepath.Join(ctx, "dist/index.html"), []byte("<h1>hi</h1>"), 0o644)
	os.WriteFile(filepath.Join(ctx, "dist/assets/app.js"), []byte("console.log(1)"), 0o644)
	os.WriteFile(filepath.Join(ctx, "dist/secret.env"), []byte("KEY=1"), 0o644)
	os.WriteFile(filepath.Join(ctx, ".hullignore"), []byte("**/*.env\n"), 0o644)

	p, err := config.Parse([]byte(`
name: site
tags: [latest, v1]
platform: linux/arm64
stages:
  - name: files
    from: scratch
    steps:
      - copy: { src: dist, dest: /out }
  - from: scratch
    steps:
      - copy: { from: files, src: /out, dest: /www }
      - env: { PORT: "8080" }
      - workdir: /www
      - expose: "8080"
      - entrypoint: ["/server"]
      - cmd: ["--root", "."]
`))
	if err != nil {
		t.Fatal(err)
	}
	p.Dir = ctx
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	res, err := (&Builder{Store: st}).Build(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Refs) != 2 {
		t.Fatalf("refs = %v", res.Refs)
	}
	img, err := st.Get("site:v1")
	if err != nil {
		t.Fatal(err)
	}
	cf, _ := img.ConfigFile()
	if cf.Architecture != "arm64" || cf.OS != "linux" {
		t.Errorf("platform = %s/%s", cf.OS, cf.Architecture)
	}
	c := cf.Config
	if c.WorkingDir != "/www" || c.Env[0] != "PORT=8080" || c.Entrypoint[0] != "/server" || c.Cmd[0] != "--root" {
		t.Errorf("config = %+v", c)
	}
	if _, ok := c.ExposedPorts["8080/tcp"]; !ok {
		t.Errorf("exposed = %v", c.ExposedPorts)
	}
	layers, _ := img.Layers()
	if len(layers) != 1 {
		t.Fatalf("layers = %d, want 1", len(layers))
	}
	rc, _ := layers[0].Uncompressed()
	defer rc.Close()
	tr := tar.NewReader(rc)
	files := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		files[h.Name] = true
	}
	for _, want := range []string{"www/index.html", "www/assets/app.js"} {
		if !files[want] {
			t.Errorf("missing %s in %v", want, files)
		}
	}
	if files["www/secret.env"] {
		t.Error("ignored file was copied")
	}

	// Building again gives the same layers.
	res2, err := (&Builder{Store: st}).Build(p)
	if err != nil {
		t.Fatal(err)
	}
	l2, _ := res2.Image.Layers()
	d1, _ := layers[0].Digest()
	d2, _ := l2[0].Digest()
	if d1 != d2 {
		t.Errorf("layer digest changed between identical builds: %s vs %s", d1, d2)
	}
}

func TestRunNeedsMatchingPlatform(t *testing.T) {
	t.Setenv("HULL_HOME", t.TempDir())
	p, _ := config.Parse([]byte("name: x\nplatform: linux/s390x\nstages: [{from: scratch, steps: [{run: echo hi}]}]"))
	p.Dir = t.TempDir()
	st, _ := store.Open()
	if _, err := (&Builder{Store: st}).Build(p); err == nil {
		t.Fatal("expected RUN on a foreign platform to fail")
	}
}
