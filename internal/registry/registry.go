// Package registry pulls and pushes images using the OCI distribution API.
package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/shubhamstrength/hull/internal/home"
)

// authFile is hull's own credentials file, in Docker's config.json format.
func authFile() string { return filepath.Join(home.Dir(), "auth.json") }

type authConfig struct {
	Auths map[string]struct {
		Auth string `json:"auth"`
	} `json:"auths"`
}

type keychain struct{}

// Resolve looks up credentials in this order: HULL_REGISTRY_USER and
// HULL_REGISTRY_PASSWORD (handy in CI), then `hull login`, then Docker's
// ~/.docker/config.json (including credential helpers such as ECR's).
func (keychain) Resolve(r authn.Resource) (authn.Authenticator, error) {
	if u, p := os.Getenv("HULL_REGISTRY_USER"), os.Getenv("HULL_REGISTRY_PASSWORD"); u != "" && p != "" {
		return &authn.Basic{Username: u, Password: p}, nil
	}
	b, err := os.ReadFile(authFile())
	if err == nil {
		var cfg authConfig
		if json.Unmarshal(b, &cfg) == nil {
			for _, key := range []string{r.RegistryStr(), "https://" + r.RegistryStr()} {
				if a, ok := cfg.Auths[key]; ok && a.Auth != "" {
					raw, err := base64.StdEncoding.DecodeString(a.Auth)
					if err != nil {
						return nil, err
					}
					user, pass, _ := strings.Cut(string(raw), ":")
					return &authn.Basic{Username: user, Password: pass}, nil
				}
			}
		}
	}
	return authn.DefaultKeychain.Resolve(r)
}

// Keychain is the credential source used for every registry call.
var Keychain authn.Keychain = keychain{}

func opts(p *v1.Platform) []remote.Option {
	o := []remote.Option{remote.WithAuthFromKeychain(Keychain), remote.WithUserAgent("hull")}
	if p != nil {
		o = append(o, remote.WithPlatform(*p))
	}
	return o
}

// Pull fetches the image for platform from its registry. Layers are fetched
// lazily, when the image is written to the store.
func Pull(ref string, platform v1.Platform) (v1.Image, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return nil, err
	}
	img, err := remote.Image(r, opts(&platform)...)
	if err != nil {
		return nil, fmt.Errorf("pulling %s: %w", ref, err)
	}
	return img, nil
}

// Push uploads img as ref. Layers the registry already has are skipped.
func Push(ref string, img v1.Image, progress chan<- v1.Update) error {
	r, err := name.ParseReference(ref)
	if err != nil {
		return err
	}
	o := opts(nil)
	if progress != nil {
		o = append(o, remote.WithProgress(progress))
	}
	if err := remote.Write(r, img, o...); err != nil {
		return fmt.Errorf("pushing %s: %w", ref, err)
	}
	return nil
}

// Login checks the credentials against the registry and saves them.
func Login(registry, user, password string) error {
	reg, err := name.NewRegistry(registry)
	if err != nil {
		return err
	}
	auth := &authn.Basic{Username: user, Password: password}
	if err := checkAuth(reg, auth); err != nil {
		return err
	}
	cfg := authConfig{Auths: map[string]struct {
		Auth string `json:"auth"`
	}{}}
	if b, err := os.ReadFile(authFile()); err == nil {
		_ = json.Unmarshal(b, &cfg)
		if cfg.Auths == nil {
			cfg.Auths = map[string]struct {
				Auth string `json:"auth"`
			}{}
		}
	}
	cfg.Auths[reg.RegistryStr()] = struct {
		Auth string `json:"auth"`
	}{base64.StdEncoding.EncodeToString([]byte(user + ":" + password))}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home.Dir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(authFile(), b, 0o600)
}

func checkAuth(reg name.Registry, auth authn.Authenticator) error {
	// Exchanging the credentials for a registry token fails on a bad password.
	ctx := context.Background()
	t, err := transport.NewWithContext(ctx, reg, auth, http.DefaultTransport, []string{reg.Scope(transport.PullScope)})
	if err == nil {
		var resp *http.Response
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s://%s/v2/", reg.Scheme(), reg.RegistryStr()), nil)
		if resp, err = (&http.Client{Transport: t}).Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				err = fmt.Errorf("registry returned 401 Unauthorized")
			}
		}
	}
	if err != nil {
		return fmt.Errorf("login to %s failed: %w", reg.RegistryStr(), err)
	}
	return nil
}
