package registry

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
)

func TestPushPull(t *testing.T) {
	t.Setenv("HULL_HOME", t.TempDir())
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	img, err := random.Image(1024, 2)
	if err != nil {
		t.Fatal(err)
	}
	ref := host + "/team/app:v1"
	if err := Push(ref, img, nil); err != nil {
		t.Fatal(err)
	}
	got, err := Pull(ref, v1.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := img.Digest()
	have, _ := got.Digest()
	if want != have {
		t.Errorf("pulled %s, pushed %s", have, want)
	}
	if err := Login(host, "user", "pass"); err != nil {
		t.Errorf("login to an open registry: %v", err)
	}
}
