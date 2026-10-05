package dock

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed web/dist
var web embed.FS

type API struct{ Manager *Manager }

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", a.status)
	mux.HandleFunc("/api/containers", a.containers)
	mux.HandleFunc("/api/routes", a.routes)
	mux.HandleFunc("/api/routes/", a.route)
	mux.HandleFunc("/api/manifests", a.manifest)
	static, err := fs.Sub(web, "web/dist")
	if err == nil {
		fileServer := http.FileServer(http.FS(static))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if _, err := fs.Stat(static, strings.TrimPrefix(r.URL.Path, "/")); err == nil {
				fileServer.ServeHTTP(w, r)
				return
			}
			http.ServeFileFS(w, r, static, "index.html")
		})
	}
	return security(a.Manager, mux)
}

func security(m *Manager, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			http.Error(w, "invalid host", http.StatusForbidden)
			return
		}
		// The application server is an HTTP backend for Traefik, not a browser
		// endpoint. Require both Traefik's private token and its HTTPS scheme
		// marker so requests to :9080 cannot access the dashboard or API directly.
		if r.Header.Get("X-Dock-Internal-Token") != m.proxyToken || !strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		origin := r.Header.Get("Origin")
		if origin != "" {
			u, err := url.Parse(origin)
			if err != nil || !sameOriginHost(u.Host, r.Host) {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func sameOriginHost(a, b string) bool { return strings.EqualFold(a, b) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func errorJSON(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (a *API) status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	writeJSON(w, 200, a.Manager.Status())
}
func (a *API) containers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	writeJSON(w, 200, a.Manager.Containers())
}

func (a *API) routes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items := []map[string]any{}
		for _, route := range a.Manager.Routes() {
			items = append(items, map[string]any{"key": routeKey(route.Owner, route.ID), "route": route})
		}
		writeJSON(w, 200, items)
	case http.MethodPost:
		var route Route
		if err := decodeJSON(w, r, &route); err != nil {
			errorJSON(w, 400, err)
			return
		}
		route.Owner = "ui"
		route.Enabled = true
		if err := a.Manager.SaveRoute(route, route.Owner, ""); err != nil {
			errorJSON(w, 400, err)
			return
		}
		if err := a.Manager.Sync(r.Context()); err != nil {
			writeJSON(w, 201, map[string]any{"route": route, "warning": err.Error()})
			return
		}
		writeJSON(w, 201, map[string]any{"route": route})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *API) route(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", 405)
		return
	}
	key, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/api/routes/"))
	if err != nil || key == "" {
		http.Error(w, "invalid route key", 400)
		return
	}
	if r.Method == http.MethodDelete {
		if err := a.Manager.DeleteRoute(key); err != nil {
			errorJSON(w, 404, err)
			return
		}
		if err := a.Manager.Sync(r.Context()); err != nil {
			writeJSON(w, 200, map[string]string{"warning": err.Error()})
			return
		}
		w.WriteHeader(204)
		return
	}
	var payload struct {
		Enabled *bool  `json:"enabled"`
		Route   *Route `json:"route"`
	}
	if err := decodeJSON(w, r, &payload); err != nil {
		errorJSON(w, 400, err)
		return
	}
	if payload.Route != nil {
		if err := a.Manager.UpdatePanelRoute(key, *payload.Route); err != nil {
			errorJSON(w, 400, err)
			return
		}
	} else if payload.Enabled != nil {
		if err := a.Manager.SetEnabled(key, *payload.Enabled); err != nil {
			errorJSON(w, 404, err)
			return
		}
	} else {
		errorJSON(w, 400, errors.New("provide enabled or route"))
		return
	}
	if err := a.Manager.Sync(r.Context()); err != nil {
		writeJSON(w, 200, map[string]any{"updated": true, "warning": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"updated": true})
}

func (a *API) manifest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var payload struct {
		Path     string   `json:"path"`
		Project  string   `json:"project"`
		Manifest Manifest `json:"manifest"`
	}
	if err := decodeJSON(w, r, &payload); err != nil {
		errorJSON(w, 400, err)
		return
	}
	if payload.Path == "" {
		errorJSON(w, 400, errors.New("manifest path is required"))
		return
	}
	if err := a.Manager.ApplyManifest(payload.Path, payload.Project, payload.Manifest); err != nil {
		errorJSON(w, 400, err)
		return
	}
	if err := a.Manager.Sync(r.Context()); err != nil {
		writeJSON(w, 200, map[string]any{"applied": payload.Manifest.RouteCount(), "warning": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"applied": payload.Manifest.RouteCount()})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

func ReadManifest(path string) (Manifest, string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Manifest{}, "", err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return Manifest{}, "", err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	var manifest Manifest
	if err := dec.Decode(&manifest); err != nil {
		return Manifest{}, "", fmt.Errorf("read %s: %w", abs, err)
	}
	return manifest, abs, nil
}

func Serve(ctx context.Context, m *Manager) error {
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = m.Stop(stopCtx)
	}()
	if err := m.Start(ctx); err != nil {
		return err
	}
	server := &http.Server{Addr: ":9080", Handler: (&API{Manager: m}).Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	fmt.Println("Dock is serving the dashboard at https://localhost")
	errch := make(chan error, 1)
	go func() { errch <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return nil
	case err := <-errch:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
