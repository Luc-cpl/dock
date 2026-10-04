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
