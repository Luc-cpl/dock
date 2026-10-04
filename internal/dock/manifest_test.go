package dock

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestServiceManifestHTTPSAndCustomPorts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dock.yml")
	content := `version: 1
services:
  web.app:
    - hostname: web-app
      protocol: https
      port: "8443:80"
  pgsql:
    - protocol: tcp
      port: "15432:5432"
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	manifest, _, err := ReadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{dataDir: t.TempDir(), state: State{Version: 1, Routes: map[string]Route{}}, containers: []Container{
		{Project: "example", Service: "web.app", WorkingDir: filepath.Dir(path), State: "running", Address: "172.19.0.2", Ports: []Port{{PrivatePort: 80, Protocol: "tcp"}}},
		{Project: "example", Service: "pgsql", WorkingDir: filepath.Dir(path), State: "running", Address: "172.19.0.3", Ports: []Port{{PrivatePort: 5432, Protocol: "tcp"}}},
	}}
	for i := 0; i < 2; i++ {
		if err := m.ApplyManifest(path, "", manifest); err != nil {
			t.Fatal(err)
		}
	}
	if manifest.RouteCount() != 2 || len(m.state.Routes) != 2 || len(m.Routes()) != 2 {
		t.Fatalf("apply duplicated configured or automatic routes: %#v", m.Routes())
	}
	for _, route := range m.state.Routes {
		if route.Service == "web.app" && (route.Hostname != "web-app.example.localhost" || route.Protocol != "http" || !route.TLS || !route.RedirectTLS || route.ListenPort != 8443 || route.Port != 80) {
			t.Fatalf("wrong HTTPS route: %#v", route)
		}
		if route.Service == "pgsql" && (route.Protocol != "tcp" || route.ListenPort != 15432 || route.Port != 5432) {
			t.Fatalf("wrong TCP mapping: %#v", route)
		}
	}
	config, ports, _, err := m.buildDynamicLocked()
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 2 {
		t.Fatalf("expected custom HTTP and TCP listeners, got %#v", ports)
	}
	http := config["http"].(map[string]any)
	routers := http["routers"].(map[string]any)
	middlewares := http["middlewares"].(map[string]any)
	var secure, redirect bool
	for _, value := range routers {
		router := value.(map[string]any)
		entries := router["entryPoints"].([]string)
		if entries[0] == "app8443" {
			secure = true
			if _, ok := router["tls"]; !ok {
				t.Fatal("HTTPS router has no TLS")
			}
			if _, ok := router["middlewares"]; ok {
				t.Fatal("redirect middleware was attached to the HTTPS router")
			}
		}
		if entries[0] == "web" {
			redirect = true
			name := router["middlewares"].([]string)[0]
			scheme := middlewares[name].(map[string]any)["redirectScheme"].(map[string]any)
			if scheme["scheme"] != "https" || scheme["port"] != "8443" || scheme["permanent"] != true {
				t.Fatalf("wrong redirect: %#v", scheme)
			}
		}
	}
	if !secure || !redirect {
		t.Fatalf("missing HTTPS router or HTTP redirect: %#v", routers)
	}
}

func TestStarterManifestReadableRoundTripAndHTTPSDefaults(t *testing.T) {
	manifest := StarterManifest([]Route{
		{Service: "web.app", Protocol: "http", Port: 80, ListenPort: 80},
		{Service: "mailpit", Protocol: "http", Port: 8025, ListenPort: 8025},
		{Service: "pgsql", Protocol: "tcp", Port: 5432, ListenPort: 5432},
	})
	data, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"id:", "project:", ".localhost", "tls:", "redirectHttps:"} {
		if strings.Contains(string(data), unwanted) {
			t.Fatalf("generated manifest contains %q:\n%s", unwanted, data)
		}
	}
	var decoded Manifest
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	service := decoded.Services["web.app"]
	service[0].Protocol = "https"
	decoded.Services["web.app"] = service
	routes, err := decoded.expandRoutes("example")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		if route.Service == "web.app" {
			if route.ListenPort != 443 || route.Port != 80 || route.Hostname != "web-app" {
				t.Fatalf("changing just the protocol did not select HTTPS defaults: %#v", route)
			}
		}
	}
	// Numeric YAML ports are supported as well as quoted mappings.
	var numeric Manifest
	if err := yaml.Unmarshal([]byte("version: 1\nservices:\n  web:\n    - protocol: https\n      port: 80\n"), &numeric); err != nil {
		t.Fatal(err)
	}
	routes, err = numeric.expandRoutes("example")
	if err != nil || len(routes) != 1 || routes[0].Port != 80 || routes[0].ListenPort != 443 {
		t.Fatalf("numeric port did not select the default HTTPS listener: %#v, %v", routes, err)
	}
}

func TestManifestRejectsInvalidPortsAndRedirectConflicts(t *testing.T) {
	for _, port := range []string{"", "0", "65536", "abc", "8443:", "127.0.0.1:8443:80", "80/tcp"} {
		if _, _, err := parseManifestPort(port, "https"); err == nil {
			t.Errorf("accepted invalid port %q", port)
		}
	}
	m := &Manager{dataDir: t.TempDir(), state: State{Version: 1, Routes: map[string]Route{}}}
	manifest := Manifest{Version: 1, Services: map[string][]ServiceRoute{
		"web": {
			{Hostname: "app", Protocol: "https", Port: "80"},
			{Hostname: "app", Protocol: "http", Port: "80"},
		},
	}}
	if err := m.ApplyManifest(filepath.Join(t.TempDir(), "dock.yml"), "example", manifest); err == nil {
		t.Fatal("accepted HTTP route that conflicts with mandatory HTTPS redirect")
	}
	if len(m.state.Routes) != 0 {
		t.Fatal("invalid manifest partially changed state")
	}
	manifest.Services["web"] = []ServiceRoute{{Protocol: "https", Port: "80:80"}}
	if err := m.ApplyManifest(filepath.Join(t.TempDir(), "dock.yml"), "example", manifest); err == nil {
		t.Fatal("accepted HTTPS on the reserved plaintext redirect listener")
	}
}

func TestConfiguredDisabledRouteStaysHiddenAndUnmappedAfterReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dock.yml")
	manifest := Manifest{Version: 1, Services: map[string][]ServiceRoute{
		"mailpit": {{Protocol: "tcp", Port: "11025:1025"}},
	}}
	containers := []Container{{
		Project: "example", Service: "mailpit", WorkingDir: filepath.Dir(path),
		State: "running", Address: "172.19.0.2", Ports: []Port{{PrivatePort: 1025, Protocol: "tcp"}},
	}}
	dir := t.TempDir()
	m := &Manager{dataDir: dir, state: State{Version: 1, Routes: map[string]Route{}}, containers: containers}
	if err := m.ApplyManifest(path, "", manifest); err != nil {
		t.Fatal(err)
	}
	if got := m.Routes(); len(got) != 1 {
		t.Fatalf("expected one configured route before disabling: %#v", got)
	}
	manifest.Services["mailpit"][0].Disabled = true
	if err := m.ApplyManifest(path, "", manifest); err != nil {
		t.Fatal(err)
	}
	// A second manager represents the daemon reloading the CLI's persisted state.
	reloaded := &Manager{dataDir: dir, state: State{Version: 1, Routes: map[string]Route{}}, containers: containers}
	if got := reloaded.Routes(); len(got) != 0 {
		t.Fatalf("configured disabled route was listed or automatically rediscovered: %#v", got)
	}
	if reloaded.Status()["routeCount"] != 0 || manifest.RouteCount() != 0 {
		t.Fatal("configured disabled route was counted as a visible/applied route")
	}
	config, ports, _, err := reloaded.buildDynamicLocked()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := config["tcp"]; ok || len(ports) != 0 {
		t.Fatalf("configured disabled route still has a TCP mapping or published port: %#v, %#v", config, ports)
	}
	for key := range reloaded.state.Routes {
		if err := reloaded.SetEnabled(key, true); err == nil {
			t.Fatal("dashboard toggle was allowed to override configuration disabled")
		}
	}
	manifest.Services["mailpit"][0].Disabled = false
	if err := reloaded.ApplyManifest(path, "", manifest); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Routes(); len(got) != 1 || !got[0].Enabled {
		t.Fatalf("reenabled configuration did not restore the route: %#v", got)
	}
	// Dashboard-disabled routes are still visible; only config-disabled ones hide.
	for key := range reloaded.state.Routes {
		if err := reloaded.SetEnabled(key, false); err != nil {
			t.Fatal(err)
		}
	}
	if got := reloaded.Routes(); len(got) == 0 {
		t.Fatal("dashboard-disabled route unexpectedly disappeared")
	}
}
