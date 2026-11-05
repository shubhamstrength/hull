// Package ui serves hull's web dashboard and the small JSON API behind it.
package ui

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/shubhamstrength/hull/internal/build"
	"github.com/shubhamstrength/hull/internal/engine"
	"github.com/shubhamstrength/hull/internal/registry"
	"github.com/shubhamstrength/hull/internal/runtime"
	"github.com/shubhamstrength/hull/internal/store"
)

//go:embed static
var static embed.FS

// Server is the dashboard HTTP handler.
type Server struct {
	Store   *store.Store
	Version string
	mu      sync.Mutex // serialises container create/start so names stay unique
}

// Serve listens on addr until the process exits.
func Serve(addr, version string, out io.Writer) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	s := &Server{Store: st, Version: version}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "hull dashboard on http://%s (Ctrl-C to stop)\n", ln.Addr())
	if err := runtime.Supported(); err != nil {
		fmt.Fprintf(out, "note: %v; containers can be viewed but not started\n", err)
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(ln)
}

// Handler returns the dashboard's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(static, "static")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	mux.HandleFunc("GET /api/info", s.info)
	mux.HandleFunc("GET /api/containers", s.containers)
	mux.HandleFunc("GET /api/containers/{id}/logs", s.logs)
	mux.HandleFunc("POST /api/containers/{id}/{action}", s.containerAction)
	mux.HandleFunc("GET /api/images", s.images)
	mux.HandleFunc("POST /api/images/remove", s.removeImage)
	mux.HandleFunc("POST /api/pull", s.pull)
	mux.HandleFunc("POST /api/run", s.run)
	return guard(mux)
}

// guard protects the API from other websites: the Host header must be a
// loopback name (which stops DNS rebinding), and requests that change state
// must carry a custom header, which a cross-site form or image cannot send.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if ip := net.ParseIP(host); !(host == "localhost" || (ip != nil && ip.IsLoopback())) {
			http.Error(w, "the hull dashboard only answers on localhost", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Hull") != "1" {
			http.Error(w, "missing X-Hull header", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func (s *Server) info(w http.ResponseWriter, _ *http.Request) {
	info := map[string]any{"version": s.Version, "os": goruntime.GOOS, "arch": goruntime.GOARCH, "canRun": true}
	if err := runtime.Supported(); err != nil {
		info["canRun"], info["runError"] = false, err.Error()
	}
	writeJSON(w, info)
}

type containerView struct {
	*runtime.State
	ImageShort string `json:"imageShort"`
	ShortID    string `json:"shortId"`
}

func (s *Server) containers(w http.ResponseWriter, _ *http.Request) {
	list, err := runtime.List()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	out := make([]containerView, 0, len(list))
	for _, c := range list {
		out = append(out, containerView{State: c, ImageShort: store.Short(c.Image), ShortID: c.ID[:12]})
	}
	writeJSON(w, out)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	st, err := runtime.Load(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	if n <= 0 || n > 5000 {
		n = 500
	}
	b, err := engine.TailLines(runtime.LogPath(st.ID), n)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) containerAction(w http.ResponseWriter, r *http.Request) {
	st, err := runtime.Load(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.PathValue("action") {
	case "start":
		err = runtime.StartDetached(st)
	case "stop":
		err = runtime.Stop(st, 10*time.Second)
	case "restart":
		if err = runtime.Stop(st, 10*time.Second); err == nil {
			if st, err = runtime.Load(st.ID); err == nil {
				err = runtime.StartDetached(st)
			}
		}
	case "remove":
		err = runtime.Remove(st, r.URL.Query().Get("force") == "1")
	default:
		writeErr(w, 404, errors.New("unknown action"))
		return
	}
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) images(w http.ResponseWriter, _ *http.Request) {
	list, err := s.Store.List()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if list == nil {
		list = []store.Image{}
	}
	writeJSON(w, list)
}

type refBody struct {
	Ref string `json:"ref"`
}

func (s *Server) removeImage(w http.ResponseWriter, r *http.Request) {
	var b refBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Ref == "" {
		writeErr(w, 400, errors.New("ref is required"))
		return
	}
	if err := s.Store.Remove(b.Ref); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) pull(w http.ResponseWriter, r *http.Request) {
	var b refBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Ref == "" {
		writeErr(w, 400, errors.New("ref is required"))
		return
	}
	plat, _ := v1.ParsePlatform(build.DefaultPlatform())
	img, err := registry.Pull(b.Ref, *plat)
	if err == nil {
		err = s.Store.Put(b.Ref, img)
	}
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	var o engine.RunOptions
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil || o.Image == "" {
		writeErr(w, 400, errors.New("image is required"))
		return
	}
	o.Detach = true
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := engine.CreateContainer(s.Store, o, io.Discard)
	if err == nil {
		err = runtime.StartDetached(st)
	}
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, map[string]string{"id": st.ID, "name": st.Name})
}
