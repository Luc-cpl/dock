package dock

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Manager struct {
	mu             sync.RWMutex
	engine         *Engine
	engineErr      string
	dataDir        string
	state          State
	containers     []Container
	lastSync       time.Time
	lastError      string
	traefikRunning bool
	proxyToken     string
}

func DataDir() string {
	if base := os.Getenv("XDG_DATA_HOME"); base != "" {
		return filepath.Join(base, "dock")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "dock")
	}
	return filepath.Join(os.TempDir(), "dock")
}

func NewManager() (*Manager, error) {
	dir := DataDir()
	if err := os.MkdirAll(filepath.Join(dir, "dynamic"), 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "certs"), 0700); err != nil {
		return nil, err
	}
	token, err := loadOrCreateProxyToken(dir)
	if err != nil {
		return nil, err
	}
	m := &Manager{dataDir: dir, proxyToken: token, state: State{Version: 1, Routes: map[string]Route{}, Overrides: map[string]Route{}}, containers: []Container{}}
	if err := m.reloadStateLocked(); err != nil {
		return nil, err
	}
	e, err := NewEngine()
	if err != nil {
		m.engineErr = err.Error()
	} else {
		m.engine = e
	}
	return m, nil
}

func loadOrCreateProxyToken(dir string) (string, error) {
	path := filepath.Join(dir, "proxy-token")
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		return "", err
	}
	return token, nil
}

func (m *Manager) DataDir() string { return m.dataDir }

func (m *Manager) Routes() []Route {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.reloadStateLocked()
	out := m.effectiveRoutesLocked()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hostname == out[j].Hostname {
			return out[i].ID < out[j].ID
		}
		return out[i].Hostname < out[j].Hostname
	})
	return out
}

func (m *Manager) effectiveRoutesLocked() []Route {
	byKey := map[string]Route{}
	for _, r := range m.state.Routes {
		byKey[routeKey(r.Owner, r.ID)] = r
	}
	for _, r := range m.autoRoutesLocked() {
		byKey[routeKey(r.Owner, r.ID)] = r
	}
	for key, r := range m.state.Overrides {
		byKey[key] = r
	}
	out := make([]Route, 0, len(byKey))
	for _, r := range byKey {
		// Manifest-disabled routes are hidden. Dashboard-disabled routes remain
		// visible so the user can enable them again.
		if r.Disabled {
			continue
		}
		if !r.Enabled {
			r.Status, r.Message = "disabled", "Disabled"
		}
		out = append(out, r)
	}
	return out
}

func (m *Manager) reloadStateLocked() error {
	path := filepath.Join(m.dataDir, "state.json")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var state State
	if err := json.Unmarshal(b, &state); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if state.Routes == nil {
		state.Routes = map[string]Route{}
	}
	if state.Overrides == nil {
		state.Overrides = map[string]Route{}
	}
	// Applied manifests are snapshots, but a deleted source must not leave
	// stale listeners or suppress automatic discovery indefinitely.
	changed := false
	for _, routes := range []map[string]Route{state.Routes, state.Overrides} {
		for key, route := range routes {
			if !filepath.IsAbs(route.Owner) {
				continue
			}
			if _, err := os.Stat(route.Owner); errors.Is(err, os.ErrNotExist) {
				delete(routes, key)
				changed = true
			}
		}
	}
	m.state = state
	if changed {
		return atomicJSON(path, m.state)
	}
	return nil
}

// DiscoveredRoutes returns automatic routes for a running Compose project.
func (m *Manager) DiscoveredRoutes(project string) []Route {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Route
	for _, route := range m.discoverRoutesLocked(false) {
		if route.Owner == "auto" && route.Project == project && route.Service != "" {
			out = append(out, route)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service == out[j].Service {
			if out[i].Protocol == out[j].Protocol {
				return out[i].Port < out[j].Port
			}
			return out[i].Protocol < out[j].Protocol
		}
		return out[i].Service < out[j].Service
	})
	return out
}

func (m *Manager) autoRoutesLocked() []Route {
	return m.discoverRoutesLocked(true)
}

func (m *Manager) discoverRoutesLocked(suppressManual bool) []Route {
	manual := map[string]bool{}
	manualListeners := map[int]bool{}
	manualTargets := map[string]bool{}
	if suppressManual {
		for _, r := range m.state.Routes {
			manualTargets[discoveryTargetKey(r.Project, r.Service, r.Container, r.Port)] = true
			if r.Disabled {
				continue
			}
			if r.Enabled && r.Protocol == "http" {
				manual[r.Hostname+"/"+strconv.Itoa(r.ListenPort)] = true
				if r.TLS {
					manual[r.Hostname+"/80"] = true
				}
			}
			if r.Enabled {
				manualListeners[r.ListenPort] = true
			}
		}
		for _, r := range m.state.Overrides {
			manualTargets[discoveryTargetKey(r.Project, r.Service, r.Container, r.Port)] = true
			manual[r.Hostname+"/"+strconv.Itoa(r.ListenPort)] = true
			if r.TLS {
				manual[r.Hostname+"/80"] = true
			}
		}
	}
	seen := map[string]bool{}
	var out []Route
	status, message := "active", "Automatic discovery"
	if !m.traefikRunning {
		status, message = "pending", "Waiting for Traefik"
	}
	add := func(host string, c Container, port int, project, service string) {
		key := host + "/" + strconv.Itoa(port)
		if seen[key] || manual[key] {
			return
		}
		seen[key] = true
		id := "auto-" + certID(key)
		container := c.Name
		if project != "" && service != "" {
			container = ""
		}
		out = append(out, Route{ID: id, Hostname: host, Project: project, Service: service, Container: container, Protocol: "http", Port: port, ListenPort: port, Enabled: true, Owner: "auto", Status: status, Message: message})
	}
	tcpCandidates := map[int]Container{}
	ambiguousTCP := map[int]bool{}
	for _, c := range m.containers {
		if c.Labels["traefik.enable"] == "false" || c.HasTraefik || c.State != "running" || net.ParseIP(c.Address) == nil {
			continue
		}
		for _, p := range c.Ports {
			protocol := knownRouteProtocol(p)
			if protocol == "" {
				continue
			}
			container := c.Name
			if c.Project != "" && c.Service != "" {
				container = ""
			}
			if manualTargets[discoveryTargetKey(c.Project, c.Service, container, p.PrivatePort)] {
				continue
			}
			if protocol == "tcp" {
				old, exists := tcpCandidates[p.PrivatePort]
				if exists && (old.Project != c.Project || old.Service != c.Service || (c.Service == "" && old.Name != c.Name)) {
					ambiguousTCP[p.PrivatePort] = true
				}
				tcpCandidates[p.PrivatePort] = c
				continue
			}
			if c.Project != "" && c.Service != "" {
				add(dnsLabel(c.Service)+"."+dnsLabel(c.Project)+".localhost", c, p.PrivatePort, c.Project, c.Service)
			} else {
				add(dnsLabel(c.Name)+".localhost", c, p.PrivatePort, "", "")
			}
		}
	}
	for port, c := range tcpCandidates {
		if manualListeners[port] || ambiguousTCP[port] {
			continue
		}
		container := c.Name
		if c.Project != "" && c.Service != "" {
			container = ""
		}
		out = append(out, Route{ID: "auto-tcp-" + certID(strconv.Itoa(port)+"/tcp"), Project: c.Project, Service: c.Service, Container: container, Protocol: "tcp", Port: port, ListenPort: port, Enabled: true, Owner: "auto", Status: status, Message: "Automatic TCP discovery"})
	}
	return out
}

func discoveryTargetKey(project, service, container string, port int) string {
	// A configured protocol corrects the discovery hint for this destination.
	return fmt.Sprintf("%s/%s/%s/%d", project, service, container, port)
}

func (m *Manager) Containers() []Container {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Container, len(m.containers))
	copy(out, m.containers)
	for i := range out {
		if out[i].Ports == nil {
			out[i].Ports = []Port{}
		}
		if out[i].Networks == nil {
			out[i].Networks = []string{}
		}
	}
	return out
}

func (m *Manager) Status() map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	routeCount := 0
	for _, route := range m.effectiveRoutesLocked() {
		if route.Enabled && !route.Disabled {
			routeCount++
		}
	}
	return map[string]any{"ok": m.lastError == "" && m.engineErr == "", "engine": map[string]any{"socket": func() string {
		if m.engine != nil {
			return m.engine.Socket
		}
		return ""
	}(), "error": m.engineErr}, "traefik": map[string]any{"running": m.traefikRunning}, "lastSync": m.lastSync, "error": m.lastError, "containerCount": len(m.containers), "routeCount": routeCount, "dataDir": m.dataDir}
}

func (m *Manager) Start(ctx context.Context) error {
	if m.engine == nil {
		return errors.New(m.engineErr)
	}
	if err := m.Sync(ctx); err != nil {
		return fmt.Errorf("start Traefik: %w", err)
	}
	go m.watch(ctx)
	go m.periodic(ctx)
	return nil
}

func (m *Manager) Stop(ctx context.Context) error {
	if m.engine == nil {
		return errors.New(m.engineErr)
	}
	return m.engine.stopTraefik(ctx)
}

func (m *Manager) RefreshInventory(ctx context.Context) error {
	if m.engine == nil {
		return errors.New(m.engineErr)
	}
	containers, err := m.engine.Discover(ctx)
	if err != nil {
		return err
	}
	traefik, _ := m.engine.IsContainerRunning(ctx, "dock-traefik")
	m.mu.Lock()
	m.containers = containers
	m.traefikRunning = traefik
	m.lastSync = time.Now()
	m.mu.Unlock()
	return nil
}

func (m *Manager) periodic(ctx context.Context) {
	t := time.NewTicker(4 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = m.Sync(ctx)
		}
	}
}

func (m *Manager) watch(ctx context.Context) {
	for ctx.Err() == nil {
		stream, err := m.engine.Events(ctx)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		dec := json.NewDecoder(stream)
		for ctx.Err() == nil {
			var ev map[string]any
			if err := dec.Decode(&ev); err != nil {
				break
			}
			_ = m.Sync(ctx)
		}
		stream.Close()
		time.Sleep(time.Second)
	}
}

func (m *Manager) Sync(ctx context.Context) error {
	if m.engine == nil {
		return errors.New(m.engineErr)
	}
	containers, err := m.engine.Discover(ctx)
	if err != nil {
		m.mu.Lock()
		m.lastError = err.Error()
		m.traefikRunning = false
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.reloadStateLocked(); err != nil {
		m.lastError = err.Error()
		return err
	}
	m.containers = containers
	for _, stored := range []map[string]Route{m.state.Routes, m.state.Overrides} {
		for key, route := range stored {
			if !route.Enabled {
				route.Status = "disabled"
				route.Message = "Disabled"
				stored[key] = route
				continue
			}
			_, usable, found := m.resolve(route, containers)
			if !found {
				route.Status = "pending"
				route.Message = "Destination not found in the runtime"
			} else if len(usable) == 0 {
				route.Status = "pending"
				route.Message = fmt.Sprintf("Destination port %d is no longer advertised in the runtime inventory", route.Port)
			} else {
				reachable := false
				message := "Route ready"
				for _, c := range usable {
					if net.ParseIP(c.Address) != nil {
						reachable = true
						break
					}
					if c.DiscoverErr != "" {
						message = c.DiscoverErr
					}
				}
				if reachable {
					route.Status = "active"
					if route.Owner != "auto" && !routePortAdvertised(route, usable) {
						message = fmt.Sprintf("Destination port %d is configured explicitly but is not advertised by the container; Dock cannot verify that a process is listening there", route.Port)
					}
				} else {
					route.Status = "pending"
				}
				route.Message = message
			}
			stored[key] = route
		}
	}
	if err := m.ensureCertificatesLocked(); err != nil {
		m.lastError = err.Error()
		m.markRoutesPendingLocked(err.Error())
		return err
	}
	dynamic, ports, networks, err := m.buildDynamicLocked()
	if err != nil {
		m.lastError = err.Error()
		return err
	}
	if err := m.engine.ensureTraefik(ctx, m.dataDir, ports, networks); err != nil {
		m.lastError = err.Error()
		m.traefikRunning = false
		m.markRoutesPendingLocked(err.Error())
		return err
	}
	if err := atomicJSON(filepath.Join(m.dataDir, "dynamic", "routes.yml"), dynamic); err != nil {
		m.lastError = err.Error()
		return err
	}
	_ = atomicJSON(filepath.Join(m.dataDir, "state.json"), m.state)
	m.lastError = ""
	m.traefikRunning = true
	m.lastSync = time.Now()
	return nil
}

func (m *Manager) markRoutesPendingLocked(message string) {
	for _, stored := range []map[string]Route{m.state.Routes, m.state.Overrides} {
		for key, route := range stored {
			if route.Enabled {
				route.Status = "pending"
				route.Message = message
				stored[key] = route
			}
		}
	}
	_ = atomicJSON(filepath.Join(m.dataDir, "state.json"), m.state)
}

func atomicJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if existing, readErr := os.ReadFile(path); readErr == nil && string(existing) == string(b) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (m *Manager) resolve(route Route, containers []Container) ([]Container, []Container, bool) {
	var targets []Container
	for _, c := range containers {
		if route.Container != "" {
			if c.Name == route.Container || c.ID == route.Container || strings.HasPrefix(c.ID, route.Container) {
				targets = append(targets, c)
			}
		} else if c.Project == route.Project && c.Service == route.Service {
			targets = append(targets, c)
		}
	}
	if len(targets) == 0 {
		return nil, nil, false
	}
	// A configured route declares its destination port explicitly. Container
	// exposed-port metadata is optional in Compose, so only require that
	// metadata for routes created by automatic discovery.
	if route.Owner != "auto" {
		return targets, targets, true
	}
	var usable []Container
	for _, c := range targets {
		for _, p := range c.Ports {
			if p.PrivatePort == route.Port && (route.Protocol == "http" && p.Protocol == "tcp" || p.Protocol == route.Protocol) {
				usable = append(usable, c)
				break
			}
		}
	}
	if len(usable) == 0 {
		return targets, nil, true
	}
	return targets, usable, true
}

func routePortAdvertised(route Route, containers []Container) bool {
	for _, container := range containers {
		for _, port := range container.Ports {
			if port.PrivatePort == route.Port && (route.Protocol == "http" && port.Protocol == "tcp" || port.Protocol == route.Protocol) {
				return true
			}
		}
	}
	return false
}

func (m *Manager) buildDynamicLocked() (map[string]any, []Port, []string, error) {
	containers := append([]Container(nil), m.containers...)
	routes := m.effectiveRoutesLocked()
	portsByKey := map[string]Port{}
	for _, r := range routes {
		if r.Disabled || !r.Enabled {
			continue
		}
		if r.Protocol == "tcp" || r.Protocol == "udp" {
			p := Port{PrivatePort: r.ListenPort, Protocol: r.Protocol}
			portsByKey[fmt.Sprintf("%d/%s", p.PrivatePort, p.Protocol)] = p
		} else if r.ListenPort > 0 && r.ListenPort != 80 && r.ListenPort != 443 {
			p := Port{PrivatePort: r.ListenPort, Protocol: "tcp"}
			portsByKey[fmt.Sprintf("%d/tcp", p.PrivatePort)] = p
		}
	}
	ports := make([]Port, 0, len(portsByKey))
	for _, p := range portsByKey {
		ports = append(ports, p)
	}
	ports = uniquePorts(ports)
	var networks []string
	for _, r := range routes {
		if r.Disabled || !r.Enabled {
			continue
		}
		targets, usable, found := m.resolve(r, containers)
		if !found || len(usable) == 0 {
			continue
		}
		for _, c := range targets {
			if c.Labels["traefik.docker.network"] != "" {
				networks = append(networks, c.Labels["traefik.docker.network"])
			} else {
				networks = append(networks, c.Networks...)
			}
		}
	}
	networks = uniqueSorted(networks)
	services := map[string]any{}
	routers := map[string]any{}
	tcpServices := map[string]any{}
	tcpRouters := map[string]any{}
	udpServices := map[string]any{}
	udpRouters := map[string]any{}
	middlewares := map[string]any{}
	tlsCerts := []any{}
	panelCert := filepath.Join(m.dataDir, "certs", certID("localhost")+".pem")
	if _, err := os.Stat(panelCert); err == nil {
		services["dock-panel"] = map[string]any{"loadBalancer": map[string]any{"servers": []map[string]string{{"url": "http://host.docker.internal:9080"}}}}
		routers["dock-panel"] = map[string]any{"entryPoints": []string{"websecure"}, "rule": "Host(`localhost`)", "service": "dock-panel", "priority": 100000, "tls": map[string]any{}, "middlewares": []string{"dock-panel-auth"}}
		middlewares["dock-panel-redirect"] = map[string]any{"redirectScheme": map[string]any{"scheme": "https", "port": "443", "permanent": true}}
		routers["dock-panel-redirect"] = map[string]any{"entryPoints": []string{"web"}, "rule": "Host(`localhost`)", "service": "noop@internal", "priority": 100000, "middlewares": []string{"dock-panel-redirect"}}
		tlsCerts = append(tlsCerts, map[string]string{"certFile": "/etc/traefik/certs/" + certID("localhost") + ".pem", "keyFile": "/etc/traefik/certs/" + certID("localhost") + "-key.pem"})
	}
	for _, route := range routes {
		if route.Disabled || !route.Enabled {
			continue
		}
		candidates, usable, found := m.resolve(route, containers)
		if !found || len(usable) == 0 {
			continue
		}
		for _, c := range candidates {
			if c.Labels["traefik.enable"] == "false" {
				route.Status = "pending"
				route.Message = "Destination excluded by traefik.enable=false"
				m.state.Routes[routeKey(route.Owner, route.ID)] = route
				usable = nil
				break
			}
		}
		if len(usable) == 0 {
			continue
		}
		serviceName := "manual-" + safeName(route.Owner+"-"+route.ID)
		entrypoint := entrypoint(route.Protocol, route.ListenPort)
		if route.Protocol == "http" {
			servers := []map[string]string{}
			for _, c := range usable {
				ip := c.Address
				if net.ParseIP(ip) == nil {
					continue
				}
				scheme := "http"
				servers = append(servers, map[string]string{"url": fmt.Sprintf("%s://%s:%d", scheme, ip, route.Port)})
			}
			if len(servers) == 0 {
				continue
			}
			services[serviceName] = map[string]any{"loadBalancer": map[string]any{"servers": servers}}
			priority := 30000
			if strings.HasPrefix(route.Hostname, "*.") {
				priority = 1000
			}
			router := map[string]any{"entryPoints": []string{entrypoint}, "rule": hostRule(route.Hostname), "service": serviceName, "priority": priority}
			if route.TLS {
				router["tls"] = map[string]any{}
			}
			routers[serviceName] = router
			if route.TLS {
				redirectName := serviceName + "-redirect"
				middlewares[redirectName] = map[string]any{"redirectScheme": map[string]any{
					"scheme": "https", "permanent": true, "port": strconv.Itoa(route.ListenPort),
				}}
				routers[redirectName] = map[string]any{
					"entryPoints": []string{"web"}, "rule": hostRule(route.Hostname),
					"service": "noop@internal", "priority": priority + 1,
					"middlewares": []string{redirectName},
				}
				certID := certID(route.Hostname)
				tlsCerts = append(tlsCerts, map[string]string{"certFile": "/etc/traefik/certs/" + certID + ".pem", "keyFile": "/etc/traefik/certs/" + certID + "-key.pem"})
			}
		} else if route.Protocol == "tcp" {
			servers := []map[string]string{}
			for _, c := range usable {
				if net.ParseIP(c.Address) != nil {
					servers = append(servers, map[string]string{"address": fmt.Sprintf("%s:%d", c.Address, route.Port)})
				}
			}
			if len(servers) == 0 {
				continue
			}
			tcpServices[serviceName] = map[string]any{"loadBalancer": map[string]any{"servers": servers}}
			rule := "HostSNI(`*`)"
			if route.Hostname != "" {
				rule = "HostSNI(`" + route.Hostname + "`)"
			}
			tcpRouters[serviceName] = map[string]any{"entryPoints": []string{entrypoint}, "rule": rule, "service": serviceName}
		} else {
			servers := []map[string]string{}
			for _, c := range usable {
				if net.ParseIP(c.Address) != nil {
					servers = append(servers, map[string]string{"address": fmt.Sprintf("%s:%d", c.Address, route.Port)})
				}
			}
			if len(servers) == 0 {
				continue
			}
			udpServices[serviceName] = map[string]any{"loadBalancer": map[string]any{"servers": servers}}
			udpRouters[serviceName] = map[string]any{"entryPoints": []string{entrypoint}, "service": serviceName}
		}
	}
	httpConfig := map[string]any{"routers": routers, "services": services}
	if _, ok := routers["dock-panel"]; ok {
		middlewares["dock-panel-auth"] = map[string]any{"headers": map[string]any{"customRequestHeaders": map[string]string{"X-Dock-Internal-Token": m.proxyToken}}}
	}
	if len(middlewares) > 0 {
		httpConfig["middlewares"] = middlewares
	}
	root := map[string]any{"http": httpConfig}
	if len(tcpRouters) > 0 {
		root["tcp"] = map[string]any{"routers": tcpRouters, "services": tcpServices}
	}
	if len(udpRouters) > 0 {
		root["udp"] = map[string]any{"routers": udpRouters, "services": udpServices}
	}
	if len(tlsCerts) > 0 {
		root["tls"] = map[string]any{"certificates": tlsCerts}
	}
	return root, ports, networks, nil
}

func entrypoint(protocol string, port int) string {
	if protocol == "http" {
		if port == 80 {
			return "web"
		}
		if port == 443 {
			return "websecure"
		}
		return "app" + strconv.Itoa(port)
	}
	if protocol == "udp" {
		return "udp" + strconv.Itoa(port)
	}
	return "app" + strconv.Itoa(port)
}

func hostRule(host string) string {
	if !strings.HasPrefix(host, "*.") {
		return "Host(`" + host + "`)"
	}
	pattern := strings.ReplaceAll(host[2:], ".", `\.`)
	return "HostRegexp(`^[a-z0-9-]+\\." + pattern + "$`)"
}

func dnsLabel(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	v := strings.Trim(b.String(), "-")
	if v == "" {
		return "container"
	}
	if len(v) > 63 {
		v = v[:63]
	}
	return v
}

func safeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 45 {
		out = out[:45]
	}
	if out == "" {
		out = "route"
	}
	return out
}

func certID(host string) string { h := sha256.Sum256([]byte(host)); return hex.EncodeToString(h[:8]) }

func (m *Manager) ensureCertificatesLocked() error {
	for _, r := range m.effectiveRoutesLocked() {
		if r.Disabled || !r.Enabled || !r.TLS || r.Protocol != "http" {
			continue
		}
		if err := generateLocalCertificate(m.dataDir, r.Hostname); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) SaveRoute(route Route, owner string, existingKey string) error {
	if owner == "ui" && (route.TLS || strings.EqualFold(route.Protocol, "https")) {
		route.ListenPort = 443
	}
	if err := route.Normalize(owner); err != nil {
		return err
	}
	key := routeKey(owner, route.ID)
	if existingKey != "" && existingKey != key {
		return errors.New("route ID and owner cannot be changed")
	}
	if !route.Enabled {
		route.Enabled = !route.Disabled
	}
	if err := m.validateConflicts([]Route{route}, ""); err != nil {
		return err
	}
	route.CreatedAt = time.Now()
	m.mu.Lock()
	m.state.Routes[key] = route
	err := atomicJSON(filepath.Join(m.dataDir, "state.json"), m.state)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	return nil
}

func (m *Manager) UpdatePanelRoute(key string, route Route) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.reloadStateLocked(); err != nil {
		return err
	}
	var existing Route
	found := false
	for _, current := range m.effectiveRoutesLocked() {
		if routeKey(current.Owner, current.ID) == key {
			existing, found = current, true
			break
		}
	}
	if !found {
		if current, ok := m.state.Overrides[key]; ok {
			existing, found = current, true
		}
	}
	if !found {
		return errors.New("route not found")
	}
	if route.ID != existing.ID {
		return errors.New("route ID cannot be changed")
	}
	if route.TLS || strings.EqualFold(route.Protocol, "https") {
		route.ListenPort = 443
	}
	if err := route.Normalize(existing.Owner); err != nil {
		return err
	}
	if existing.Owner != "ui" && (route.Project != existing.Project || route.Service != existing.Service || route.Container != existing.Container || route.Port != existing.Port) {
		return errors.New("the destination and container port are managed by the route source")
	}
	route.Enabled = existing.Enabled
	route.Disabled = existing.Disabled
	route.CreatedAt = existing.CreatedAt
	if err := m.validateEffectiveRoute(route, key); err != nil {
		return err
	}
	if m.state.Overrides == nil {
		m.state.Overrides = map[string]Route{}
	}
	if existing.Owner == "ui" || m.state.Routes[key].ID != "" {
		m.state.Routes[key] = route
	} else {
		m.state.Overrides[key] = route
	}
	return atomicJSON(filepath.Join(m.dataDir, "state.json"), m.state)
}

func (m *Manager) validateEffectiveRoute(route Route, excludingKey string) error {
	if !route.Enabled || route.Disabled {
		return nil
	}
	for _, other := range m.effectiveRoutesLocked() {
		if routeKey(other.Owner, other.ID) == excludingKey || !other.Enabled || other.Disabled {
			continue
		}
		if route.Protocol == "http" && other.Protocol == "http" && route.Hostname == other.Hostname && (route.ListenPort == other.ListenPort || route.TLS && other.ListenPort == 80 || other.TLS && route.ListenPort == 80) {
			return fmt.Errorf("hostname %s already has a route listener", route.Hostname)
		}
		if route.Protocol != "http" && route.Protocol == other.Protocol && route.ListenPort == other.ListenPort {
			return fmt.Errorf("port conflict %s/%d", route.Protocol, route.ListenPort)
		}
	}
	return nil
}

func (m *Manager) SetEnabled(key string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.reloadStateLocked(); err != nil {
		return err
	}
	var r Route
	found := false
	for _, current := range m.effectiveRoutesLocked() {
		if routeKey(current.Owner, current.ID) == key {
			r, found = current, true
			break
		}
	}
	if !found {
		return errors.New("route not found")
	}
	r.Disabled = false
	r.Enabled = enabled
	if err := m.validateEffectiveRoute(r, key); err != nil {
		return err
	}
	if m.state.Overrides == nil {
		m.state.Overrides = map[string]Route{}
	}
	if r.Owner == "ui" || m.state.Routes[key].ID != "" {
		m.state.Routes[key] = r
	} else {
		m.state.Overrides[key] = r
	}
	return atomicJSON(filepath.Join(m.dataDir, "state.json"), m.state)
}

// ExportRoutes returns a project's effective routes in the same format as routes init.
func (m *Manager) ExportRoutes(project string) (Manifest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.reloadStateLocked(); err != nil {
		return Manifest{}, err
	}
	var routes []Route
	for _, route := range m.effectiveRoutesLocked() {
		if route.Project != project {
			continue
		}
		if route.Service == "" {
			return Manifest{}, fmt.Errorf("route %q targets a container directly; Compose project manifests use services", route.ID)
		}
		route.Disabled = route.Disabled || !route.Enabled
		routes = append(routes, route)
	}
	if len(routes) == 0 {
		return Manifest{}, fmt.Errorf("no dashboard routes found for Compose project %q", project)
	}
	sort.Slice(routes, func(i, j int) bool {
		a, b := routes[i], routes[j]
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		if a.Protocol != b.Protocol {
			return a.Protocol < b.Protocol
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		if a.Hostname != b.Hostname {
			return a.Hostname < b.Hostname
		}
		return a.ListenPort < b.ListenPort
	})
	return StarterManifest(routes), nil
}

func (m *Manager) DeleteRoute(key string) error {
	m.mu.Lock()
	r, ok := m.state.Routes[key]
	if !ok {
		m.mu.Unlock()
		return errors.New("route not found")
	}
	if r.Owner != "ui" {
		m.mu.Unlock()
		return errors.New("discovered and dock.yml routes cannot be deleted; disable them or export your route settings")
	}
	delete(m.state.Routes, key)
	err := atomicJSON(filepath.Join(m.dataDir, "state.json"), m.state)
	m.mu.Unlock()
	return err
}

func (m *Manager) ApplyManifest(path, projectOverride string, manifest Manifest) error {
	if manifest.Version != 1 {
		return fmt.Errorf("manifest version must be 1")
	}
	if projectOverride == "" {
		projectOverride = manifest.Project
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	owner := filepath.Clean(abs)
	if projectOverride == "" {
		projects, err := m.FindProject(filepath.Dir(abs))
		if err != nil {
			return err
		}
		if len(projects) == 1 {
			projectOverride = projects[0]
		} else if len(projects) > 1 {
			return fmt.Errorf("multiple Compose projects match %s (%s); use --project", filepath.Dir(abs), strings.Join(projects, ", "))
		}
	}
	m.mu.RLock()
	discovered := m.discoverRoutesLocked(false)
	m.mu.RUnlock()
	manifestRoutes, err := manifest.expandRoutes(projectOverride)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	normalized := make([]Route, 0, len(manifestRoutes))
	for _, route := range manifestRoutes {
		if route.Service == "" && route.Container == "" {
			var match *Route
			for i := range discovered {
				candidate := &discovered[i]
				if candidate.ID == route.ID && (projectOverride == "" || candidate.Project == projectOverride) {
					match = candidate
					break
				}
			}
			if match == nil {
				return fmt.Errorf("route %q has no service or container and does not match a discovered route; regenerate dock.yml or specify a destination", route.ID)
			}
			route.Project = match.Project
			route.Service = match.Service
			route.Container = match.Container
			if route.Protocol == "" {
				route.Protocol = match.Protocol
			}
			if route.Port == 0 {
				route.Port = match.Port
			}
			if route.ListenPort == 0 {
				route.ListenPort = match.ListenPort
			}
		}
		if route.Project == "" && (route.Service != "" || isRelativeManifestHostname(route.Hostname)) {
			route.Project = projectOverride
		}
		if route.Project == "" && (route.Service != "" || isRelativeManifestHostname(route.Hostname)) {
			return fmt.Errorf("route %q needs a Compose project to resolve its relative hostname; use --project", route.ID)
		}
		protocol := strings.ToLower(strings.TrimSpace(route.Protocol))
		if protocol == "" || protocol == "http" || protocol == "https" {
			route.Hostname = expandManifestHostname(route.Hostname, route.Project)
		}
		if err := route.Normalize(owner); err != nil {
			return err
		}
		if seen[route.ID] {
			return fmt.Errorf("duplicate ID %q", route.ID)
		}
		seen[route.ID] = true
		route.Enabled = !route.Disabled
		normalized = append(normalized, route)
	}
	if err := m.validateConflicts(normalized, owner); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, r := range m.state.Routes {
		if r.Owner == owner && !seen[r.ID] || r.Owner != owner && containsRouteSettings(normalized, r) {
			delete(m.state.Routes, key)
		}
	}
	// Applying a published snapshot transfers its automatic overrides to the file.
	for key, r := range m.state.Overrides {
		for _, configured := range normalized {
			if discoveryTargetKey(r.Project, r.Service, r.Container, r.Port) == discoveryTargetKey(configured.Project, configured.Service, configured.Container, configured.Port) {
				delete(m.state.Overrides, key)
				break
			}
		}
	}
	for _, r := range normalized {
		r.Enabled = !r.Disabled
		if r.CreatedAt.IsZero() {
			r.CreatedAt = time.Now()
		}
		m.state.Routes[routeKey(owner, r.ID)] = r
	}
	return atomicJSON(filepath.Join(m.dataDir, "state.json"), m.state)
}

func isRelativeManifestHostname(hostname string) bool {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	return hostname != "" && !strings.HasSuffix(hostname, ".localhost")
}

// expandManifestHostname lets a service route use a hostname relative to its
// Compose project: "app", "*", and "*.app" become local project names;
// an empty hostname resolves to the project's root hostname.
func expandManifestHostname(hostname, project string) string {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	if hostname == "" {
		if strings.TrimSpace(project) != "" {
			return dnsLabel(project) + ".localhost"
		}
		return hostname
	}
	if strings.HasSuffix(hostname, ".localhost") || strings.TrimSpace(project) == "" {
		return hostname
	}
	project = dnsLabel(project)
	switch {
	case hostname == "*":
		return "*." + project + ".localhost"
	case strings.HasPrefix(hostname, "*."):
		name := strings.TrimPrefix(hostname, "*.")
		return "*." + name + "." + project + ".localhost"
	case !strings.Contains(hostname, "."):
		return hostname + "." + project + ".localhost"
	default:
		return hostname
	}
}

func (m *Manager) DeleteManifest(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	owner := filepath.Clean(abs)
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, r := range m.state.Routes {
		if r.Owner == owner {
			delete(m.state.Routes, key)
		}
	}
	return atomicJSON(filepath.Join(m.dataDir, "state.json"), m.state)
}

func (m *Manager) validateConflicts(candidate []Route, owner string) error {
	all := map[string]Route{}
	m.mu.RLock()
	for k, r := range m.state.Routes {
		if r.Owner != owner && !(filepath.IsAbs(owner) && containsRouteSettings(candidate, r)) {
			all[k] = r
		}
	}
	m.mu.RUnlock()
	for _, r := range candidate {
		all[routeKey(owner, r.ID)] = r
	}
	ports := map[string]string{}
	hosts := map[string]string{}
	for _, r := range all {
		if r.Disabled || !r.Enabled {
			continue
		}
		if r.Protocol == "tcp" || r.Protocol == "udp" {
			k := r.Protocol + "/" + strconv.Itoa(r.ListenPort)
			if old := ports[k]; old != "" {
				return fmt.Errorf("port conflict %s: routes %s and %s", k, old, r.ID)
			}
			ports[k] = r.ID
		}
		if r.Protocol == "http" && r.Hostname != "" {
			listeners := []int{r.ListenPort}
			if r.TLS {
				listeners = append(listeners, 80)
			}
			for _, listener := range listeners {
				k := r.Hostname + "/" + strconv.Itoa(listener)
				if old := hosts[k]; old != "" && old != r.ID {
					return fmt.Errorf("hostname %s on port %d is already used by routes %s and %s (HTTPS also reserves HTTP port 80 for redirect)", r.Hostname, listener, old, r.ID)
				}
				hosts[k] = r.ID
			}
		}
	}
	return nil
}

func containsRouteSettings(routes []Route, other Route) bool {
	for _, route := range routes {
		if route.Project == other.Project && route.Service == other.Service && route.Container == other.Container && route.Protocol == other.Protocol && route.Hostname == other.Hostname && route.Port == other.Port && route.ListenPort == other.ListenPort && route.TLS == other.TLS && route.RedirectTLS == other.RedirectTLS && (route.Enabled && !route.Disabled) == (other.Enabled && !other.Disabled) {
			return true
		}
	}
	return false
}

func generateLocalCertificate(dataDir string, hosts ...string) error {
	if _, err := exec.LookPath("mkcert"); err != nil {
		return errors.New("mkcert is not installed; install it and run dock trust")
	}
	caroot, err := exec.Command("mkcert", "-CAROOT").Output()
	if err != nil {
		return fmt.Errorf("get mkcert CA path: %w", err)
	}
	if _, err := os.Stat(filepath.Join(strings.TrimSpace(string(caroot)), "rootCA.pem")); err != nil {
		return errors.New("local CA is not installed; run dock trust")
	}
	cert := filepath.Join(dataDir, "certs", certID(hosts[0])+".pem")
	key := filepath.Join(dataDir, "certs", certID(hosts[0])+"-key.pem")
	if _, err := os.Stat(cert); err == nil {
		if _, err = os.Stat(key); err == nil {
			return nil
		}
	}
	args := []string{"-cert-file", cert, "-key-file", key}
	args = append(args, hosts...)
	cmd := exec.Command("mkcert", args...)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("generate local certificate: %w: %s", err, strings.TrimSpace(string(b)))
	}
	_ = os.Chmod(key, 0600)
	return nil
}

func (m *Manager) FindProject(workingDir string) ([]string, error) {
	abs, err := filepath.Abs(workingDir)
	if err != nil {
		return nil, err
	}
	abs = filepath.Clean(abs)
	projects := map[string]bool{}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, c := range m.containers {
		if c.WorkingDir != "" {
			dir, e := filepath.Abs(c.WorkingDir)
			if e == nil && filepath.Clean(dir) == abs && c.Project != "" {
				projects[c.Project] = true
			}
		}
	}
	out := make([]string, 0, len(projects))
	for p := range projects {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}
