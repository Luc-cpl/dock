package dock

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeReportsRuntimeFailureAndCleansUp(t *testing.T) {
	stopped, removed := false, false
	e := &Engine{Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/containers/json":
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message":"runtime unavailable"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/containers/dock-traefik/stop":
			stopped = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/containers/dock-traefik":
			removed = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		return w.Result(), nil
	})}}
	err := Serve(context.Background(), &Manager{engine: e})
	if err == nil || !strings.Contains(err.Error(), "runtime unavailable") {
		t.Fatalf("startup failure was hidden: %v", err)
	}
	if !stopped || !removed {
		t.Fatalf("managed container was not cleaned up: stopped=%v removed=%v", stopped, removed)
	}
}

func TestStartReportsMissingEngine(t *testing.T) {
	m := &Manager{engineErr: "runtime socket not found"}
	if err := m.Start(context.Background()); err == nil || err.Error() != m.engineErr {
		t.Fatalf("missing engine failure was hidden: %v", err)
	}
}

func TestDashboardHasHTTPRedirect(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "certs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "certs", certID("localhost")+".pem"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{dataDir: dir, state: State{Routes: map[string]Route{}, Overrides: map[string]Route{}}}
	dynamic, _, _, err := m.buildDynamicLocked()
	if err != nil {
		t.Fatal(err)
	}
	httpConfig := dynamic["http"].(map[string]any)
	redirect := httpConfig["routers"].(map[string]any)["dock-panel-redirect"].(map[string]any)
	if redirect["rule"] != "Host(`localhost`)" || redirect["service"] != "noop@internal" || redirect["entryPoints"].([]string)[0] != "web" {
		t.Fatalf("invalid dashboard HTTP redirect: %#v", redirect)
	}
	name := redirect["middlewares"].([]string)[0]
	scheme := httpConfig["middlewares"].(map[string]any)[name].(map[string]any)["redirectScheme"].(map[string]any)
	if scheme["scheme"] != "https" || scheme["port"] != "443" || scheme["permanent"] != true {
		t.Fatalf("invalid dashboard HTTPS target: %#v", scheme)
	}
}
