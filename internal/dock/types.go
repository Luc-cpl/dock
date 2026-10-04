package dock

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Port struct {
	PrivatePort int    `json:"privatePort"`
	Protocol    string `json:"protocol"`
}

type Container struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Image            string            `json:"image"`
	State            string            `json:"state"`
	Project          string            `json:"project,omitempty"`
	Service          string            `json:"service,omitempty"`
	WorkingDir       string            `json:"workingDir,omitempty"`
	Networks         []string          `json:"networks"`
	NetworkAddresses map[string]string `json:"-"`
	Ports            []Port            `json:"ports"`
	Labels           map[string]string `json:"labels"`
	Address          string            `json:"address,omitempty"`
	HasTraefik       bool              `json:"hasTraefikRules"`
	DiscoverErr      string            `json:"discoveryError,omitempty"`
	PortDiscoveryErr string            `json:"portDiscoveryError,omitempty"`
}

type Route struct {
	ID          string    `json:"id" yaml:"id"`
	Hostname    string    `json:"hostname,omitempty" yaml:"hostname"`
	Project     string    `json:"project,omitempty" yaml:"project"`
	Service     string    `json:"service,omitempty" yaml:"service"`
	Container   string    `json:"container,omitempty" yaml:"container"`
	Protocol    string    `json:"protocol" yaml:"protocol"`
	Port        int       `json:"port" yaml:"port"`
	ListenPort  int       `json:"listenPort,omitempty" yaml:"listenPort"`
	TLS         bool      `json:"tls" yaml:"tls"`
	RedirectTLS bool      `json:"redirectHttps" yaml:"redirectHttps"`
	Enabled     bool      `json:"enabled" yaml:"enabled"`
	Disabled    bool      `json:"disabled,omitempty" yaml:"disabled"`
	Owner       string    `json:"owner,omitempty" yaml:"-"`
	CreatedAt   time.Time `json:"createdAt" yaml:"-"`
	Status      string    `json:"status,omitempty" yaml:"-"`
	Message     string    `json:"message,omitempty" yaml:"-"`
}

type Manifest struct {
	Version  int                       `yaml:"version"`
	Project  string                    `yaml:"project,omitempty"`
	Services map[string][]ServiceRoute `yaml:"services,omitempty"`
	Routes   []Route                   `yaml:"routes,omitempty"`
}

type State struct {
	Version int              `json:"version"`
	Routes  map[string]Route `json:"routes"`
}

var routeIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func (r *Route) Normalize(source string) error {
	r.ID = strings.TrimSpace(r.ID)
	if !routeIDPattern.MatchString(r.ID) {
		return fmt.Errorf("invalid ID %q (use letters, numbers, dots, hyphens, or underscores)", r.ID)
	}
	r.Hostname = strings.ToLower(strings.TrimSpace(r.Hostname))
	r.Project = strings.TrimSpace(r.Project)
	r.Service = strings.TrimSpace(r.Service)
	r.Container = strings.TrimSpace(r.Container)
	r.Protocol = strings.ToLower(strings.TrimSpace(r.Protocol))
	if r.Protocol == "" {
		r.Protocol = "http"
	}
	if r.Protocol == "https" {
		r.Protocol = "http"
		r.TLS = true
	}
	if r.Protocol != "http" && r.Protocol != "tcp" && r.Protocol != "udp" {
		return fmt.Errorf("protocol for %q must be http, https, tcp, or udp", r.ID)
	}
	if (r.Service == "") == (r.Container == "") {
		return fmt.Errorf("route %q must define either service or container", r.ID)
	}
	if r.Service != "" && r.Project == "" {
		return fmt.Errorf("route %q must define project when using service", r.ID)
	}
	if r.Port < 1 || r.Port > 65535 {
		return fmt.Errorf("invalid destination port on route %q", r.ID)
	}
	if r.ListenPort == 0 && r.Protocol == "http" {
		if r.TLS {
			r.ListenPort = 443
		} else {
			r.ListenPort = 80
		}
	}
	if r.ListenPort < 1 || r.ListenPort > 65535 {
		return fmt.Errorf("listenPort is required and must be between 1 and 65535 on route %q", r.ID)
	}
	if r.ListenPort == 9080 || r.ListenPort == 9180 {
		return fmt.Errorf("listener port %d is reserved for Dock", r.ListenPort)
	}
	if r.ListenPort < 1024 && !(r.Protocol == "http" && (r.ListenPort == 80 || r.ListenPort == 443)) {
		return fmt.Errorf("use a listener port above 1023; only HTTP supports ports 80 and 443")
	}
	if r.TLS && r.Protocol != "http" {
		return fmt.Errorf("Dock-managed TLS is available only for HTTP on route %q", r.ID)
	}
	if r.TLS {
		if r.ListenPort == 80 {
			return fmt.Errorf("HTTPS route %q cannot listen on port 80; use 443 or a custom port above 1023", r.ID)
		}
		r.RedirectTLS = true
	}
	if r.RedirectTLS && (!r.TLS || r.Protocol != "http") {
		return fmt.Errorf("redirectHttps requires an HTTP route with tls: true (%q)", r.ID)
	}
	if r.Hostname != "" && !validHostnamePattern(r.Hostname) {
		return fmt.Errorf("invalid hostname on route %q", r.ID)
	}
	if r.Hostname == "localhost" {
		return errors.New("localhost is reserved for the Dock dashboard")
	}
	if r.Hostname == "" && r.Protocol == "http" {
		return fmt.Errorf("HTTP route %q requires a hostname", r.ID)
	}
	if r.Hostname != "" && r.Protocol != "http" {
		return fmt.Errorf("TCP and UDP routes use ports; hostnames apply only to HTTP")
	}
	if r.Hostname == "" && r.Protocol == "tcp" && !r.TLS {
		// Raw TCP is intentionally configured as a one-route listener per port.
	}
	if source != "" {
		r.Owner = source
	}
	return nil
}

func validHostnamePattern(host string) bool {
	if strings.ContainsAny(host, "/:@ `") || strings.Contains(host, "..") || len(host) > 253 {
		return false
	}
	if strings.HasPrefix(host, "*.") {
		host = host[2:]
	}
	if strings.Contains(host, "*") {
		return false
	}
	if net.ParseIP(host) != nil {
		return false
	}
	for _, part := range strings.Split(host, ".") {
		if len(part) == 0 || len(part) > 63 || part[0] == '-' || part[len(part)-1] == '-' {
			return false
		}
		for _, c := range part {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return strings.HasSuffix(host, ".localhost")
}

func routeKey(owner, id string) string {
	if owner == "" || owner == "ui" {
		return "ui/" + id
	}
	return filepath.Clean(owner) + "/" + id
}
