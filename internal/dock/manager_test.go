package dock

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestManifestApplyIsIdempotentAndIsolatedByFile(t *testing.T) {
	m := &Manager{dataDir: t.TempDir(), state: State{Version: 1, Routes: map[string]Route{}}}
	first := filepath.Join(t.TempDir(), "dock.yml")
	second := filepath.Join(t.TempDir(), "dock.yml")
	a := Manifest{Version: 1, Routes: []Route{{ID: "one", Hostname: "app.alpha.localhost", Project: "alpha", Service: "web", Port: 8080}}}
	b := Manifest{Version: 1, Routes: []Route{{ID: "one", Hostname: "api.beta.localhost", Project: "beta", Service: "api", Port: 9000}}}
	if err := m.ApplyManifest(first, "", a); err != nil {
		t.Fatal(err)
	}
	if err := m.ApplyManifest(second, "", b); err != nil {
		t.Fatal(err)
	}
	if err := m.ApplyManifest(first, "", a); err != nil {
		t.Fatal(err)
	}
	if got := len(m.state.Routes); got != 2 {
		t.Fatalf("got %d routes, want 2", got)
	}
	a.Routes = nil
	if err := m.ApplyManifest(first, "", a); err != nil {
		t.Fatal(err)
	}
	if len(m.state.Routes) != 1 {
		t.Fatalf("removing one manifest route removed another manifest's route")
	}
	if _, ok := m.state.Routes[routeKey(second, "one")]; !ok {
		t.Fatal("route owned by second manifest was not preserved")
	}
	if _, err := os.Stat(filepath.Join(m.dataDir, "state.json")); err != nil {
		t.Fatal(err)
	}
}

func TestManifestExpandsHostnamesRelativeToComposeProject(t *testing.T) {
	m := &Manager{dataDir: t.TempDir(), state: State{Version: 1, Routes: map[string]Route{}}}
	path := filepath.Join(t.TempDir(), "dock.yml")
	manifest := Manifest{Version: 1, Routes: []Route{
		{ID: "service", Hostname: "name", Service: "web", Port: 8080},
		{ID: "wildcard", Hostname: "*", Service: "web", Port: 8081},
		{ID: "nested-wildcard", Hostname: "*.name", Service: "web", Port: 8082},
		{ID: "full", Hostname: "app.project.localhost", Service: "web", Port: 8083},
	}}
	if err := m.ApplyManifest(path, "project", manifest); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"service":         "name.project.localhost",
		"wildcard":        "*.project.localhost",
		"nested-wildcard": "*.name.project.localhost",
		"full":            "app.project.localhost",
	}
	for id, hostname := range want {
		route, ok := m.state.Routes[routeKey(path, id)]
		if !ok {
			t.Fatalf("route %q was not stored", id)
		}
		if route.Hostname != hostname {
			t.Errorf("route %q hostname = %q, want %q", id, route.Hostname, hostname)
		}
		if route.Project != "project" {
			t.Errorf("route %q project = %q, want project", id, route.Project)
		}
	}
}

func TestPanelRouteSaveRejectsHostnameConflict(t *testing.T) {
	m := &Manager{dataDir: t.TempDir(), state: State{Version: 1, Routes: map[string]Route{}}}
	first := Route{ID: "first", Hostname: "app.project.localhost", Project: "project", Service: "web", Protocol: "http", Port: 8080, ListenPort: 443, TLS: true}
	if err := m.SaveRoute(first, "ui", ""); err != nil {
		t.Fatal(err)
	}
	second := Route{ID: "second", Hostname: "app.project.localhost", Project: "project", Service: "web", Protocol: "http", Port: 8081, ListenPort: 443, TLS: true}
	if err := m.SaveRoute(second, "ui", ""); err == nil {
		t.Fatal("saving a second route with the same hostname and entrypoint should fail")
	}
}

func TestPanelRouteCanBeUpdatedWithoutChangingItsIdentity(t *testing.T) {
	m := &Manager{dataDir: t.TempDir(), state: State{Version: 1, Routes: map[string]Route{}}}
	first := Route{ID: "web", Hostname: "app.project.localhost", Project: "project", Service: "web", Protocol: "http", Port: 8080, ListenPort: 443, TLS: true}
	if err := m.SaveRoute(first, "ui", ""); err != nil {
		t.Fatal(err)
	}
	created := m.state.Routes["ui/web"].CreatedAt
	updated := Route{ID: "web", Hostname: "api.project.localhost", Project: "project", Service: "web", Protocol: "http", Port: 8081, ListenPort: 443, TLS: true}
	if err := m.UpdatePanelRoute("ui/web", updated); err != nil {
		t.Fatal(err)
	}
	got := m.state.Routes["ui/web"]
	if got.Hostname != "api.project.localhost" || got.Port != 8081 || got.Owner != "ui" || !got.CreatedAt.Equal(created) {
		t.Fatalf("unexpected updated route: %#v", got)
	}
	updated.ID = "changed"
	if err := m.UpdatePanelRoute("ui/web", updated); err == nil {
		t.Fatal("editing a route ID should fail")
	}
}

func TestGeneratedHTTPRoutesSharePortAndTargetTheSelectedContainer(t *testing.T) {
	m := &Manager{dataDir: t.TempDir(), state: State{Version: 1, Routes: map[string]Route{}}, containers: []Container{
		{ID: "one", Name: "alpha-web-1", Project: "alpha", Service: "web", State: "running", Address: "172.19.0.2", Networks: []string{"alpha_default"}, Ports: []Port{{PrivatePort: 8080, Protocol: "tcp"}}},
		{ID: "two", Name: "beta-web-1", Project: "beta", Service: "web", State: "running", Address: "172.20.0.2", Networks: []string{"beta_default"}, Ports: []Port{{PrivatePort: 8080, Protocol: "tcp"}}},
	}}
	raw, ports, networks, err := m.buildDynamicLocked()
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	config := string(bytes)
	for _, want := range []string{"web.alpha.localhost", "web.beta.localhost"} {
		if !contains(config, want) {
			t.Fatalf("generated config misses %q: %s", want, config)
		}
	}
	for _, unwanted := range []string{"alpha-web-1.localhost", "beta-web-1.localhost"} {
		if contains(config, unwanted) {
			t.Fatalf("generated config duplicates a Compose service as %q: %s", unwanted, config)
		}
	}
	if len(ports) != 1 || ports[0].PrivatePort != 8080 || ports[0].Protocol != "tcp" {
		t.Fatalf("unexpected listeners: %#v", ports)
	}
	if len(networks) != 2 {
		t.Fatalf("expected only routed service networks, got %#v", networks)
	}
}

func TestDiscoveredComposeRoutesDoNotDuplicateContainersOrMislabelMailPorts(t *testing.T) {
	m := &Manager{state: State{Version: 1, Routes: map[string]Route{}}, containers: []Container{
		{Name: "example-mailpit-1", Project: "example", Service: "mailpit", State: "running", Address: "172.19.0.2", Ports: []Port{{PrivatePort: 8025, Protocol: "tcp"}, {PrivatePort: 1025, Protocol: "tcp"}, {PrivatePort: 1110, Protocol: "tcp"}}},
		{Name: "example-pgsql-1", Project: "example", Service: "pgsql", State: "running", Address: "172.19.0.3", Ports: []Port{{PrivatePort: 5432, Protocol: "tcp"}}},
	}}
	routes := m.Routes()
	if len(routes) != 4 {
		t.Fatalf("got %d automatic routes, want one per recognized port: %#v", len(routes), routes)
	}
	for _, route := range routes {
		if route.Project != "example" || route.Container != "" {
			t.Errorf("Compose route should belong to the project without a container alias: %#v", route)
		}
		if route.Port == 8025 && (route.Protocol != "http" || route.Hostname != "mailpit.example.localhost") {
			t.Errorf("Mailpit web route is wrong: %#v", route)
		}
		if route.Port == 1025 || route.Port == 1110 || route.Port == 5432 {
			if route.Protocol != "tcp" || route.Hostname != "" {
				t.Errorf("Mail or database port was not classified as raw TCP: %#v", route)
			}
		}
	}
}

func TestRouteValidationRequiresExplicitTCPListenerAndSafeLocalHostname(t *testing.T) {
	bad := Route{ID: "raw-db", Container: "postgres", Protocol: "tcp", Port: 5432}
	if err := bad.Normalize("ui"); err == nil {
		t.Fatal("TCP route without listenPort should fail")
	}
	bad = Route{ID: "outside", Hostname: "service.example.com", Container: "app", Protocol: "http", Port: 80}
	if err := bad.Normalize("ui"); err == nil {
		t.Fatal("non-local hostname should fail")
	}
	good := Route{ID: "wild", Hostname: "*.project.localhost", Project: "project", Service: "web", Protocol: "http", Port: 8080, TLS: true}
	if err := good.Normalize("ui"); err != nil {
		t.Fatal(err)
	}
	if good.ListenPort != 443 {
		t.Fatalf("TLS default port=%d, want 443", good.ListenPort)
	}
	if got := hostRule("*.project.localhost"); got != "HostRegexp(`^[a-z0-9-]+\\.project\\.localhost$`)" {
		t.Fatalf("wildcard rule=%q", got)
	}
}

func TestManifestRouteUsesDiscoveredContainerPort(t *testing.T) {
	m := &Manager{dataDir: t.TempDir(), state: State{Version: 1, Routes: map[string]Route{}}, containers: []Container{{
		Name: "example-storage-1", Project: "example", Service: "storage", State: "running",
		Address: "172.19.0.2", Networks: []string{"example_default"}, Ports: []Port{{PrivatePort: 9000, Protocol: "tcp"}, {PrivatePort: 9001, Protocol: "tcp"}},
	}}}
	manifest := Manifest{Version: 1, Services: map[string][]ServiceRoute{
		"storage": {{Hostname: "storage", Protocol: "http", Port: "9001:9001"}},
	}}
	if err := m.ApplyManifest(filepath.Join(t.TempDir(), "dock.yml"), "example", manifest); err != nil {
		t.Fatal(err)
	}
	var manual Route
	for _, route := range m.state.Routes {
		manual = route
	}
	_, usable, found := m.resolve(manual, m.containers)
	if !found || len(usable) != 1 {
		t.Fatalf("configured port 9001 was rejected despite being discovered: %#v", manual)
	}
	config, ports, networks, err := m.buildDynamicLocked()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(data), "http://172.19.0.2:9001") || !contains(string(data), "app9001") {
		t.Fatalf("manual storage console route is missing from proxy config: %s", data)
	}
	var published bool
	for _, port := range ports {
		if port.PrivatePort == 9001 && port.Protocol == "tcp" {
			published = true
		}
	}
	if !published || len(networks) != 1 || networks[0] != "example_default" {
		t.Fatalf("manual route listener or network is missing: %#v, %#v", ports, networks)
	}
	// Configuration selects a destination; it does not invent listening ports.
	manual.Port = 9002
	_, usable, found = m.resolve(manual, m.containers)
	if !found || len(usable) != 0 {
		t.Fatal("configured route incorrectly accepted an undiscovered port")
	}
}

func TestAdditionalHTTPPortIsRecognizedWhenDiscovered(t *testing.T) {
	m := &Manager{state: State{Version: 1, Routes: map[string]Route{}}, containers: []Container{{
		Name: "example-storage-1", Project: "example", Service: "storage", State: "running",
		Address: "172.19.0.2", Ports: []Port{{PrivatePort: 9000, Protocol: "tcp"}, {PrivatePort: 9001, Protocol: "tcp"}},
	}}}
	routes := m.DiscoveredRoutes("example")
	if len(routes) != 2 {
		t.Fatalf("expected both discovered HTTP routes, got %#v", routes)
	}
	for _, route := range routes {
		if route.Protocol != "http" || route.Hostname != "storage.example.localhost" || route.Service != "storage" {
			t.Fatalf("wrong storage automatic route: %#v", route)
		}
	}
}

func TestPreferredAddressHonorsTraefikNetworkLabel(t *testing.T) {
	c := Container{
		Address: "172.18.0.3",
		Labels:  map[string]string{"traefik.docker.network": "backend"},
		NetworkAddresses: map[string]string{
			"frontend": "172.18.0.3",
			"backend":  "172.19.0.3",
		},
	}
	if got := preferredAddress(c); got != "172.19.0.3" {
		t.Fatalf("preferred address = %q, want backend address", got)
	}
	delete(c.NetworkAddresses, "backend")
	if got := preferredAddress(c); got != "" {
		t.Fatalf("missing labeled network fell back to address %q", got)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
