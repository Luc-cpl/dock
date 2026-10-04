package dock

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSourceRouteEditsPreserveDestinationAndPublishHTTPS(t *testing.T) {
	for _, source := range []string{"auto", "manifest"} {
		t.Run(source, func(t *testing.T) {
			m := &Manager{dataDir: t.TempDir(), state: State{Version: 1, Routes: map[string]Route{}}, containers: []Container{
				{Name: "example-web-1", Project: "example", Service: "web", State: "running", Address: "172.19.0.2", Ports: []Port{{PrivatePort: 80, Protocol: "tcp"}}},
			}}
			if source == "manifest" {
				path := filepath.Join(t.TempDir(), "dock.yml")
				if err := os.WriteFile(path, []byte("version: 1\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := m.ApplyManifest(path, "example", Manifest{Version: 1, Routes: []Route{{ID: "web", Project: "example", Service: "web", Hostname: "web.example.localhost", Port: 80}}}); err != nil {
					t.Fatal(err)
				}
			}
			original := m.Routes()[0]
			key := routeKey(original.Owner, original.ID)
			for _, change := range []func(*Route){
				func(r *Route) { r.Project = "other" },
				func(r *Route) { r.Service = "other" },
				func(r *Route) { r.Service, r.Container = "", "other" },
				func(r *Route) { r.Port = 8080 },
			} {
				changed := original
				change(&changed)
				if err := m.UpdatePanelRoute(key, changed); err == nil {
					t.Fatalf("source destination edit accepted: %#v", changed)
				}
			}
			changed := original
			changed.Hostname, changed.TLS, changed.ListenPort = "secure.example.localhost", true, 8443
			if err := m.UpdatePanelRoute(key, changed); err != nil {
				t.Fatal(err)
			}
			routes := m.Routes()
			if len(routes) != 1 || routes[0].Hostname != changed.Hostname || routes[0].ListenPort != 443 || routes[0].Port != 80 || !routes[0].TLS || !routes[0].RedirectTLS || routes[0].Owner != original.Owner {
				t.Fatalf("edit was not persisted or duplicated discovery: %#v", routes)
			}
			if err := m.DeleteRoute(key); err == nil {
				t.Fatal("source route could be deleted")
			}
			config, _, _, err := m.buildDynamicLocked()
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(config)
			for _, want := range []string{"secure.example.localhost", "websecure", "172.19.0.2:80", "\"port\":\"443\""} {
				if !strings.Contains(string(data), want) {
					t.Fatalf("HTTPS configuration lacks %q: %s", want, data)
				}
			}
			if err := m.SetEnabled(key, false); err != nil {
				t.Fatal(err)
			}
			if routes := m.Routes(); len(routes) != 1 || routes[0].Enabled || routes[0].Status != "disabled" {
				t.Fatalf("disabled route rediscovered: %#v", routes)
			}
			exported, err := m.ExportRoutes("example")
			if err != nil {
				t.Fatal(err)
			}
			data, err = yaml.Marshal(exported)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "export.yml")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			decoded, _, err := ReadManifest(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.ApplyManifest(path, "example", decoded); err != nil {
				t.Fatal(err)
			}
			if got := m.Routes(); len(got) != 1 || got[0].Owner != path {
				t.Fatalf("applying an export duplicated its source routes: %#v", got)
			}
			other := &Manager{dataDir: t.TempDir(), state: State{Version: 1, Routes: map[string]Route{}}}
			if err := other.ApplyManifest(path, "example", decoded); err != nil {
				t.Fatal(err)
			}
			if got := other.Routes(); len(got) != 1 || got[0].Enabled || got[0].Hostname != changed.Hostname || got[0].ListenPort != 443 || got[0].Port != 80 {
				t.Fatalf("export lost edited/disabled configuration: %#v", got)
			}
		})
	}
}

func TestExistingSourceRoutesCanCorrectTheirProtocol(t *testing.T) {
	for _, source := range []string{"auto", "manifest"} {
		for _, correction := range []struct {
			name, from, to string
			port, listener int
		}{
			{"tcp-to-http", "tcp", "http", 5432, 80},
			{"tcp-to-https", "tcp", "https", 5432, 8443},
			{"http-to-tcp", "http", "tcp", 8080, 18080},
			{"http-to-udp", "http", "udp", 8080, 18080},
		} {
			t.Run(source+"/"+correction.name, func(t *testing.T) {
				containers := []Container{{Name: "example-app-1", Project: "example", Service: "app", State: "running", Address: "172.19.0.2", Ports: []Port{{PrivatePort: correction.port, Protocol: "tcp"}, {PrivatePort: correction.port, Protocol: "udp"}}}}
				m := &Manager{dataDir: t.TempDir(), state: State{Version: 1, Routes: map[string]Route{}}, containers: containers}
				if source == "manifest" {
					path := filepath.Join(t.TempDir(), "dock.yml")
					if err := os.WriteFile(path, []byte("version: 1\n"), 0600); err != nil {
						t.Fatal(err)
					}
					route := Route{ID: "app", Project: "example", Service: "app", Protocol: correction.from, Port: correction.port, ListenPort: correction.port}
					if correction.from == "http" {
						route.Hostname = "app.example.localhost"
					}
					if err := m.ApplyManifest(path, "example", Manifest{Version: 1, Routes: []Route{route}}); err != nil {
						t.Fatal(err)
					}
				}
				original := m.Routes()[0]
				key := routeKey(original.Owner, original.ID)
				changed := original
				changed.Protocol, changed.ListenPort, changed.Hostname = correction.to, correction.listener, ""
				if correction.to == "http" || correction.to == "https" {
					changed.Hostname = "corrected.example.localhost"
				}
				if err := m.UpdatePanelRoute(key, changed); err != nil {
					t.Fatal(err)
				}
				// Reload the persisted state as the daemon does on inventory refresh.
				reloaded := &Manager{dataDir: m.dataDir, containers: containers}
				routes := reloaded.Routes()
				if len(routes) != 1 {
					t.Fatalf("protocol correction duplicated automatic discovery: %#v", routes)
				}
				got := routes[0]
				protocol, listener := correction.to, correction.listener
				if protocol == "https" {
					protocol, listener = "http", 443
				}
				if got.Protocol != protocol || got.ListenPort != listener || got.Port != original.Port || got.Project != original.Project || got.Service != original.Service || got.Container != original.Container || got.Owner != original.Owner {
					t.Fatalf("protocol correction changed the destination or was not persisted: %#v", got)
				}
				config, _, _, err := reloaded.buildDynamicLocked()
				if err != nil {
					t.Fatal(err)
				}
				section, ok := config[protocol].(map[string]any)
				wantRouters := 1
				if got.TLS {
					wantRouters++
				}
				if !ok || len(section["routers"].(map[string]any)) != wantRouters || len(section["services"].(map[string]any)) != 1 {
					t.Fatalf("corrected protocol was not routed: %#v", config)
				}
				if err := reloaded.SetEnabled(key, false); err != nil {
					t.Fatal(err)
				}
				if got := reloaded.Routes(); len(got) != 1 || got[0].Enabled {
					t.Fatalf("disabled correction rediscovered the old protocol: %#v", got)
				}
				exported, err := reloaded.ExportRoutes("example")
				if err != nil {
					t.Fatal(err)
				}
				if len(exported.Services) != 1 || len(exported.Services["app"]) != 1 || exported.Services["app"][0].Protocol != correction.to || !exported.Services["app"][0].Disabled {
					t.Fatalf("export lost protocol correction: %#v", exported)
				}
			})
		}
	}
}

func TestDeletedManifestReleasesDiscoveryWithoutRemovingDashboardRoutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dock.yml")
	if err := os.WriteFile(path, []byte("version: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{dataDir: t.TempDir(), state: State{Version: 1, Routes: map[string]Route{}}, containers: []Container{
		{Name: "example-web-1", Project: "example", Service: "web", State: "running", Address: "172.19.0.2", Ports: []Port{{PrivatePort: 80, Protocol: "tcp"}}},
	}}
	if err := m.ApplyManifest(path, "example", Manifest{Version: 1, Routes: []Route{{ID: "configured", Project: "example", Service: "web", Hostname: "custom.example.localhost", Port: 80, ListenPort: 2019}}}); err != nil {
		t.Fatal(err)
	}
	if err := m.SaveRoute(Route{ID: "keep", Container: "other", Hostname: "other.localhost", Port: 80}, "ui", ""); err != nil {
		t.Fatal(err)
	}
	if got := m.Routes(); len(got) != 2 {
		t.Fatalf("configured route duplicated discovery: %#v", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	got := m.Routes()
	if len(got) != 2 {
		t.Fatalf("deleted manifest affected unrelated routes: %#v", got)
	}
	var discovered, dashboard bool
	for _, r := range got {
		if r.Owner == path {
			t.Fatalf("deleted source still owns route: %#v", r)
		}
		discovered = discovered || r.Owner == "auto" && r.Hostname == "web.example.localhost"
		dashboard = dashboard || r.Owner == "ui" && r.ID == "keep"
	}
	if !discovered || !dashboard {
		t.Fatalf("discovery/dashboard preservation failed: %#v", got)
	}
	config, ports, _, err := m.buildDynamicLocked()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(config)
	if strings.Contains(string(data), "custom.example.localhost") || len(ports) != 0 {
		t.Fatalf("deleted manifest listener is still published: %s, %#v", data, ports)
	}
	other := &Manager{dataDir: m.dataDir}
	if err := other.reloadStateLocked(); err != nil {
		t.Fatal(err)
	}
	if len(other.state.Routes) != 1 {
		t.Fatalf("deleted manifest was not removed from disk: %#v", other.state.Routes)
	}
}

func TestAutomaticHTTPSOverrideGetsCertificate(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "certs"), 0700); err != nil {
		t.Fatal(err)
	}
	ca := t.TempDir()
	if err := os.WriteFile(filepath.Join(ca, "rootCA.pem"), []byte("test CA"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = '-CAROOT' ]; then printf '%s\\n' \"$DOCK_TEST_CA\"; exit 0; fi\n: > \"$2\"\n: > \"$4\"\n"
	if err := os.WriteFile(filepath.Join(bin, "mkcert"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("DOCK_TEST_CA", ca)
	m := &Manager{dataDir: dir, state: State{Overrides: map[string]Route{
		"auto/web": {ID: "web", Owner: "auto", Hostname: "secure.localhost", Protocol: "http", TLS: true, Enabled: true, Container: "web", Port: 80, ListenPort: 443},
	}}}
	if err := m.ensureCertificatesLocked(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".pem", "-key.pem"} {
		if _, err := os.Stat(filepath.Join(dir, "certs", certID("secure.localhost")+suffix)); err != nil {
			t.Fatalf("automatic HTTPS override certificate missing: %v", err)
		}
	}
}
