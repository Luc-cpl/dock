package dock

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestApplicationPort9000DoesNotUseDashboardListener(t *testing.T) {
	var created struct {
		Cmd        []string   `json:"Cmd"`
		HostConfig hostConfig `json:"HostConfig"`
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/containers/dock-traefik/json":
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/containers/create":
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Error(err)
			}
			fmt.Fprint(w, `{"Id":"new-traefik"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/containers/new-traefik/json":
			fmt.Fprint(w, `{"NetworkSettings":{"Networks":{}}}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	e := &Engine{Socket: "/test/docker.sock", Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result(), nil
	})}}
	if err := e.ensureTraefik(context.Background(), t.TempDir(), []Port{{PrivatePort: 9000, Protocol: "tcp"}}, nil); err != nil {
		t.Fatal(err)
	}
	cmd := strings.Join(created.Cmd, " ")
	if !strings.Contains(cmd, "--entryPoints.app9000.address=:9000") || !strings.Contains(cmd, "--entryPoints.traefik.address=:9180") {
		t.Fatalf("application and dashboard listeners overlap: %s", cmd)
	}
	for _, port := range []string{"9000", "9180"} {
		bindings := created.HostConfig.PortBindings[port+"/tcp"]
		if len(bindings) != 1 || bindings[0].HostPort != port || bindings[0].HostIP != "127.0.0.1" {
			t.Fatalf("wrong published port %s: %#v", port, bindings)
		}
	}
	if entrypoint("http", 9000) != "app9000" {
		t.Fatal("HTTP application was assigned to the dashboard entrypoint")
	}
}

func TestConnectNetworksSkipsExistingPodmanNetwork(t *testing.T) {
	connected := map[string]bool{"podman": true, "example_default": true}
	connects := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/containers/traefik-id/json":
			fmt.Fprint(w, `{"NetworkSettings":{"Networks":{`)
			first := true
			for name := range connected {
				if !first {
					fmt.Fprint(w, ",")
				}
				fmt.Fprintf(w, "%q:{}", name)
				first = false
			}
			fmt.Fprint(w, `}}}`)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/networks/"):
			connects++
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/networks/"), "/connect")
			if connected[name] {
				http.Error(w, "network is already connected", http.StatusForbidden)
				return
			}
			connected[name] = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	e := &Engine{Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result(), nil
	})}}
	for i := 0; i < 2; i++ {
		if err := e.connectNetworks(context.Background(), "traefik-id", []string{"example_default", "new_net"}); err != nil {
			t.Fatal(err)
		}
	}
	if connects != 1 {
		t.Fatalf("connect calls = %d, want one call for the new network", connects)
	}
}

func TestDiscoverDeclaredPortsAcrossImageTypes(t *testing.T) {
	cases := []struct {
		name, image     string
		listed, exposed string
		want            []Port
	}{
		{"http", "example/web", `[{"PrivatePort":8080,"Type":"tcp"}]`, `{"8080/tcp":{}}`, []Port{{8080, "tcp"}}},
		{"database", "example/database", `[]`, `{"5432/tcp":{}}`, []Port{{5432, "tcp"}}},
		{"shellless", "example/distroless", `[]`, `{"18791/tcp":{}}`, []Port{{18791, "tcp"}}},
		{"udp", "example/udp", `[{"PrivatePort":15353,"Type":"udp"}]`, `{"15353/udp":{}}`, []Port{{15353, "udp"}}},
		{"mixed", "example/mixed", `[]`, `{"18080/tcp":{},"15353/udp":{}}`, []Port{{15353, "udp"}, {18080, "tcp"}}},
		{"no-declarations", "example/worker", `[]`, `{}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/containers/json":
					fmt.Fprintf(w, `[{"Id":"fixture","Names":["/example-service-1"],"Image":%q,"State":"running","Labels":{"com.docker.compose.project":"example","com.docker.compose.service":"service"},"Ports":%s}]`, tc.image, tc.listed)
				case r.Method == http.MethodGet && r.URL.Path == "/containers/fixture/json":
					fmt.Fprintf(w, `{"Config":{"ExposedPorts":%s},"NetworkSettings":{"Networks":{"example_default":{"IPAddress":"172.18.0.2"}}}}`, tc.exposed)
				default:
					t.Errorf("unexpected runtime request (discovery must not exec): %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
				return w.Result(), nil
			})}}
			containers, err := e.Discover(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(containers) != 1 {
				t.Fatalf("containers=%d", len(containers))
			}
			c := containers[0]
			if len(c.Ports) != len(tc.want) {
				t.Fatalf("ports=%v, want %v", c.Ports, tc.want)
			}
			for i, p := range tc.want {
				if c.Ports[i] != p {
					t.Fatalf("ports=%v, want %v", c.Ports, tc.want)
				}
			}
			m := &Manager{containers: containers, state: State{Version: 1, Routes: map[string]Route{}}}
			for _, p := range tc.want {
				protocols := []string{p.Protocol}
				if p.Protocol == "tcp" {
					protocols = append(protocols, "http")
				}
				for _, protocol := range protocols {
					_, usable, found := m.resolve(Route{Project: "example", Service: "service", Port: p.PrivatePort, Protocol: protocol}, containers)
					if !found || len(usable) != 1 {
						t.Fatalf("declared %d/%s rejected", p.PrivatePort, protocol)
					}
				}
			}
			if tc.name == "shellless" && len(m.DiscoveredRoutes("example")) != 0 {
				t.Fatal("unknown protocol should require an explicit route")
			}
			_, usable, _ := m.resolve(Route{Project: "example", Service: "service", Port: 19999, Protocol: "tcp"}, containers)
			if len(usable) != 0 {
				t.Fatal("undeclared port accepted")
			}
		})
	}
}
