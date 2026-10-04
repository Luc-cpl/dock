package dock

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Port accepts a container port or a Compose-style "host:container" pair.
type ServiceRoute struct {
	Hostname string `yaml:"hostname,omitempty"`
	Protocol string `yaml:"protocol"`
	Port     string `yaml:"port"`
	Disabled bool   `yaml:"disabled,omitempty"`
}

func (m Manifest) RouteCount() int {
	n := 0
	for _, route := range m.Routes {
		if !route.Disabled {
			n++
		}
	}
	for _, service := range m.Services {
		for _, route := range service {
			if !route.Disabled {
				n++
			}
		}
	}
	return n
}

func (m Manifest) expandRoutes(project string) ([]Route, error) {
	if len(m.Services) == 0 {
		return m.Routes, nil
	}
	if len(m.Routes) != 0 {
		return nil, fmt.Errorf("use services or legacy routes in a manifest, not both")
	}
	if project == "" {
		return nil, fmt.Errorf("no running Compose project matches the manifest directory; use --project")
	}
	names := make([]string, 0, len(m.Services))
	for name := range m.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	var routes []Route
	for _, service := range names {
		if strings.TrimSpace(service) == "" {
			return nil, fmt.Errorf("service name must not be empty")
		}
		for i, spec := range m.Services[service] {
			protocol := strings.ToLower(strings.TrimSpace(spec.Protocol))
			if protocol == "" {
				protocol = "http"
			}
			if protocol != "http" && protocol != "https" && protocol != "tcp" && protocol != "udp" {
				return nil, fmt.Errorf("service %q route %d: protocol must be http, https, tcp, or udp", service, i+1)
			}
			hostPort, containerPort, err := parseManifestPort(spec.Port, protocol)
			if err != nil {
				return nil, fmt.Errorf("service %q route %d: %w", service, i+1, err)
			}
			hostname := strings.ToLower(strings.TrimSpace(spec.Hostname))
			if hostname == "" && (protocol == "http" || protocol == "https") {
				hostname = dnsLabel(service)
			}
			identity := fmt.Sprintf("%s/%s/%s/%d/%d", service, hostname, protocol, hostPort, containerPort)
			routes = append(routes, Route{
				ID: "route-" + certID(identity), Project: project, Service: service,
				Hostname: hostname, Protocol: protocol, Port: containerPort,
				ListenPort: hostPort, Disabled: spec.Disabled,
			})
		}
	}
	return routes, nil
}

func parseManifestPort(value, protocol string) (int, int, error) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) < 1 || len(parts) > 2 {
		return 0, 0, fmt.Errorf("port must be a container port or a host:container pair")
	}
	parse := func(s string) (int, error) {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 || n > 65535 {
			return 0, fmt.Errorf("port %q must be between 1 and 65535", s)
		}
		return n, nil
	}
	containerPort, err := parse(parts[len(parts)-1])
	if err != nil {
		return 0, 0, err
	}
	hostPort := containerPort
	if len(parts) == 2 {
		hostPort, err = parse(parts[0])
	} else if protocol == "http" {
		hostPort = 80
	} else if protocol == "https" {
		hostPort = 443
	}
	return hostPort, containerPort, err
}

// StarterManifest keeps the runtime's service names and explicit port choices,
// while using the HTTP/HTTPS default listener when it matches the discovery.
func StarterManifest(routes []Route) Manifest {
	manifest := Manifest{Version: 1, Services: map[string][]ServiceRoute{}}
	for _, route := range routes {
		protocol := route.Protocol
		defaultPort := 0
		if protocol == "http" {
			defaultPort = 80
			if route.TLS {
				protocol, defaultPort = "https", 443
			}
		}
		port := fmt.Sprintf("%d:%d", route.ListenPort, route.Port)
		if route.ListenPort == defaultPort {
			port = strconv.Itoa(route.Port)
		}
		hostname := ""
		if protocol == "http" || protocol == "https" {
			hostname = relativeManifestHostname(route.Hostname, route.Project, route.Service)
		}
		manifest.Services[route.Service] = append(manifest.Services[route.Service], ServiceRoute{Hostname: hostname, Protocol: protocol, Port: port, Disabled: route.Disabled})
	}
	return manifest
}

func relativeManifestHostname(hostname, project, service string) string {
	if hostname == "" {
		return dnsLabel(service)
	}
	if project != "" {
		suffix := "." + dnsLabel(project) + ".localhost"
		if strings.HasSuffix(hostname, suffix) {
			relative := strings.TrimSuffix(hostname, suffix)
			if !strings.Contains(relative, ".") || strings.HasPrefix(relative, "*.") {
				return relative
			}
		}
	}
	return hostname
}

// EncodeManifest gives initialization and export the same editable YAML format.
func EncodeManifest(manifest Manifest) ([]byte, error) {
	data, err := yaml.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	header := "# Protocols: http, https, tcp, udp. HTTPS always redirects HTTP.\n" +
		"# port: container port (HTTP defaults to 80, HTTPS to 443), or \"host:container\".\n" +
		"# hostname is relative to the Compose project; .localhost names are absolute.\n" +
		"# disabled: true disables the route while keeping it visible in the dashboard.\n"
	return append([]byte(header), data...), nil
}
