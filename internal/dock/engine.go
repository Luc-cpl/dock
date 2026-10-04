package dock

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Engine struct {
	Socket string
	Client *http.Client
}

func NewEngine() (*Engine, error) {
	endpoint := strings.TrimSpace(os.Getenv("DOCKER_HOST"))
	if endpoint == "" {
		for _, p := range []string{"/var/run/docker.sock", filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "podman/podman.sock"), filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "docker.sock")} {
			if p == "" || strings.Contains(p, "//") {
				continue
			}
			if info, err := os.Stat(p); err == nil && info.Mode()&os.ModeSocket != 0 {
				endpoint = "unix://" + p
				break
			}
		}
	}
	if !strings.HasPrefix(endpoint, "unix://") {
		return nil, fmt.Errorf("Dock supports local Unix sockets only; set DOCKER_HOST=unix:///path/to/socket")
	}
	socket := strings.TrimPrefix(endpoint, "unix://")
	if !filepath.IsAbs(socket) {
		return nil, fmt.Errorf("socket path must be absolute: %s", socket)
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
	}}
	return &Engine{Socket: socket, Client: &http.Client{Transport: tr}}, nil
}

func (e *Engine) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &msg)
		if msg.Message == "" {
			msg.Message = strings.TrimSpace(string(b))
		}
		return nil, fmt.Errorf("runtime API %s %s: %s (HTTP %d)", method, path, msg.Message, resp.StatusCode)
	}
	return resp, nil
}

func (e *Engine) jsonRequest(ctx context.Context, method, path string, out any) error {
	r, err := e.request(ctx, method, path, nil)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(out)
}

type listContainer struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
	Ports  []struct {
		Private int    `json:"PrivatePort"`
		Type    string `json:"Type"`
	} `json:"Ports"`
}

type containerInspect struct {
	Config struct {
		Labels       map[string]string `json:"Labels"`
		ExposedPorts map[string]any    `json:"ExposedPorts"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
			NetworkID string `json:"NetworkID"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func (e *Engine) Ping(ctx context.Context) error {
	r, err := e.request(ctx, http.MethodGet, "/_ping", nil)
	if err != nil {
		return err
	}
	r.Body.Close()
	return nil
}

func (e *Engine) IsContainerRunning(ctx context.Context, name string) (bool, error) {
	var info struct {
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
	}
	if err := e.jsonRequest(ctx, http.MethodGet, "/containers/"+name+"/json", &info); err != nil {
		return false, err
	}
	return info.State.Running, nil
}

func (e *Engine) Discover(ctx context.Context) ([]Container, error) {
	var listed []listContainer
	if err := e.jsonRequest(ctx, http.MethodGet, "/containers/json?all=false", &listed); err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(listed))
	for _, item := range listed {
		c := Container{ID: item.ID, Image: item.Image, State: item.State, Labels: item.Labels}
		if len(item.Names) != 0 {
			c.Name = strings.TrimPrefix(item.Names[0], "/")
		}
		c.Project = item.Labels["com.docker.compose.project"]
		c.Service = item.Labels["com.docker.compose.service"]
		c.WorkingDir = item.Labels["com.docker.compose.project.working_dir"]
		for _, p := range item.Ports {
			proto := strings.ToLower(p.Type)
			if proto != "tcp" && proto != "udp" {
				proto = "tcp"
			}
			if p.Private > 0 {
				c.Ports = append(c.Ports, Port{PrivatePort: p.Private, Protocol: proto})
			}
		}
		var detail containerInspect
		if err := e.jsonRequest(ctx, http.MethodGet, "/containers/"+c.ID+"/json", &detail); err != nil {
			c.DiscoverErr = err.Error()
			out = append(out, c)
			continue
		}
		if len(c.Labels) == 0 {
			c.Labels = detail.Config.Labels
		}
		if c.Project == "" {
			c.Project = c.Labels["com.docker.compose.project"]
		}
		if c.Service == "" {
			c.Service = c.Labels["com.docker.compose.service"]
		}
		if c.WorkingDir == "" {
			c.WorkingDir = c.Labels["com.docker.compose.project.working_dir"]
		}
		c.NetworkAddresses = map[string]string{}
		for name, n := range detail.NetworkSettings.Networks {
			c.Networks = append(c.Networks, name)
			c.NetworkAddresses[name] = n.IPAddress
		}
		c.Address = preferredAddress(c)
		if preferred := c.Labels["traefik.docker.network"]; preferred != "" && net.ParseIP(c.Address) == nil {
			c.DiscoverErr = fmt.Sprintf("traefik.docker.network=%q is not connected to the container", preferred)
		} else if net.ParseIP(c.Address) == nil {
			c.DiscoverErr = "container has no IP address on a network reachable by Traefik"
		}
		for key := range c.Labels {
			if strings.HasPrefix(key, "traefik.http.routers.") || strings.HasPrefix(key, "traefik.tcp.routers.") || strings.HasPrefix(key, "traefik.udp.routers.") {
				c.HasTraefik = true
				break
			}
		}
		// Docker's list response includes exposed ports. Include inspect values too for engines
		// whose list endpoint omits an unbound private port.
		seen := map[string]bool{}
		for _, p := range c.Ports {
			seen[strconv.Itoa(p.PrivatePort)+"/"+p.Protocol] = true
		}
		for spec := range detail.Config.ExposedPorts {
			parts := strings.Split(spec, "/")
			port, _ := strconv.Atoi(parts[0])
			proto := "tcp"
			if len(parts) > 1 {
				proto = strings.ToLower(parts[1])
			}
			key := strconv.Itoa(port) + "/" + proto
			if port > 0 && !seen[key] {
				c.Ports = append(c.Ports, Port{PrivatePort: port, Protocol: proto})
				seen[key] = true
			}
		}
		// Image metadata is incomplete for applications which open additional
		// listeners at runtime. Read the live socket tables in their network namespace.
		if c.State == "running" && c.Name != "dock-traefik" {
			ports, err := e.listeningPorts(ctx, c.ID)
			if err != nil {
				c.PortDiscoveryErr = fmt.Sprintf("live port discovery unavailable; using runtime metadata: %s", err)
			} else {
				for _, p := range ports {
					key := strconv.Itoa(p.PrivatePort) + "/" + p.Protocol
					if !seen[key] {
						c.Ports = append(c.Ports, p)
						seen[key] = true
					}
				}
			}
		}
		sort.Slice(c.Ports, func(i, j int) bool {
			if c.Ports[i].PrivatePort == c.Ports[j].PrivatePort {
				return c.Ports[i].Protocol < c.Ports[j].Protocol
			}
			return c.Ports[i].PrivatePort < c.Ports[j].PrivatePort
		})
		c.Networks = uniqueSorted(c.Networks)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func preferredAddress(c Container) string {
	if c.NetworkAddresses == nil {
		return c.Address
	}
	if preferred := c.Labels["traefik.docker.network"]; preferred != "" {
		if address := c.NetworkAddresses[preferred]; net.ParseIP(address) != nil {
			return address
		}
		return ""
	}
	keys := make([]string, 0, len(c.NetworkAddresses))
	for name := range c.NetworkAddresses {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		if net.ParseIP(c.NetworkAddresses[name]) != nil {
			return c.NetworkAddresses[name]
		}
	}
	return c.Address
}

func (e *Engine) Events(ctx context.Context) (io.ReadCloser, error) {
	r, err := e.request(ctx, http.MethodGet, "/events?filters=%7B%22type%22%3A%5B%22container%22%2C%22network%22%5D%7D", nil)
	if err != nil {
		return nil, err
	}
	return r.Body, nil
}

type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}
type containerConfig struct {
	Image        string                    `json:"Image"`
	Cmd          []string                  `json:"Cmd"`
	Labels       map[string]string         `json:"Labels"`
	ExposedPorts map[string]map[string]any `json:"ExposedPorts"`
}
type hostConfig struct {
	Binds         []string                 `json:"Binds"`
	ExtraHosts    []string                 `json:"ExtraHosts,omitempty"`
	PortBindings  map[string][]PortBinding `json:"PortBindings"`
	RestartPolicy map[string]any           `json:"RestartPolicy"`
	SecurityOpt   []string                 `json:"SecurityOpt,omitempty"`
}
type createContainerResponse struct {
	ID string `json:"Id"`
}

func (e *Engine) ensureTraefik(ctx context.Context, dataDir string, ports []Port, traefikNetworks []string) error {
	name := "dock-traefik"
	ports = uniquePorts(ports)
	networks := uniqueSorted(traefikNetworks)
	signatureBytes, _ := json.Marshal(struct {
		Ports      []Port
		Networks   []string
		Socket     string
		PanelProxy string
	}{ports, networks, e.Socket, "host-gateway-dashboard9180"})
	signature := string(signatureBytes)
	signaturePath := filepath.Join(dataDir, "traefik-signature.json")
	previousSignature, _ := os.ReadFile(signaturePath)
	var old struct {
		ID string `json:"Id"`
	}
	if err := e.jsonRequest(ctx, http.MethodGet, "/containers/"+name+"/json", &old); err == nil {
		var state struct {
			State struct {
				Running bool `json:"Running"`
			} `json:"State"`
		}
		if err := e.jsonRequest(ctx, http.MethodGet, "/containers/"+name+"/json", &state); err == nil && state.State.Running && string(previousSignature) == signature {
			return e.connectNetworks(ctx, old.ID, networks)
		}
	}
	// Remove the previous managed instance so new listeners can be applied deterministically.
	if r, err := e.request(ctx, http.MethodPost, "/containers/"+name+"/stop?t=5", nil); err == nil {
		r.Body.Close()
	}
	if r, err := e.request(ctx, http.MethodDelete, "/containers/"+name+"?force=true&v=true", nil); err == nil {
		r.Body.Close()
	}
	if err := e.pullImage(ctx, "traefik", "v3.7.13"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "dynamic"), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "certs"), 0700); err != nil {
		return err
	}
	cmd := []string{
		"--api.dashboard=true", "--api.insecure=true", "--providers.docker=true",
		"--providers.docker.endpoint=unix:///var/run/docker.sock", "--providers.docker.exposedByDefault=true",
		"--providers.docker.defaultRule=Host(`{{ if index .Labels \"com.docker.compose.service\" }}{{ index .Labels \"com.docker.compose.service\" }}.{{ index .Labels \"com.docker.compose.project\" }}{{ else }}{{ normalize .Name }}{{ end }}.localhost`)",
		"--providers.file.directory=/etc/traefik/dynamic", "--providers.file.watch=true",
		"--entryPoints.web.address=:80", "--entryPoints.websecure.address=:443", "--entryPoints.traefik.address=:9180",
		"--ping=true", "--log.level=INFO",
	}
	exposed := map[string]map[string]any{"80/tcp": {}, "443/tcp": {}, "9180/tcp": {}}
	bindings := map[string][]PortBinding{
		"80/tcp":   {{HostIP: "127.0.0.1", HostPort: "80"}},
		"443/tcp":  {{HostIP: "127.0.0.1", HostPort: "443"}},
		"9180/tcp": {{HostIP: "127.0.0.1", HostPort: "9180"}},
	}
	for _, p := range ports {
		if p.PrivatePort < 1 || p.PrivatePort > 65535 {
			continue
		}
		if p.Protocol == "udp" {
			key := fmt.Sprintf("%d/udp", p.PrivatePort)
			if _, ok := bindings[key]; !ok {
				name := "udp" + strconv.Itoa(p.PrivatePort)
				cmd = append(cmd, "--entryPoints."+name+".address=:"+strconv.Itoa(p.PrivatePort)+"/udp")
				exposed[key] = map[string]any{}
				bindings[key] = []PortBinding{{HostIP: "127.0.0.1", HostPort: strconv.Itoa(p.PrivatePort)}}
			}
			continue
		}
		if p.PrivatePort < 1024 || p.PrivatePort == 9180 {
			continue
		}
		key := fmt.Sprintf("%d/tcp", p.PrivatePort)
		if _, ok := bindings[key]; !ok {
			name := fmt.Sprintf("app%d", p.PrivatePort)
			cmd = append(cmd, "--entryPoints."+name+".address=:"+strconv.Itoa(p.PrivatePort))
			exposed[key] = map[string]any{}
			bindings[key] = []PortBinding{{HostIP: "127.0.0.1", HostPort: strconv.Itoa(p.PrivatePort)}}
		}
	}
	labels := map[string]string{"traefik.enable": "false", "org.opencontainers.image.title": "Dock managed Traefik"}
	body := map[string]any{
		"Image": "traefik:v3.7.13", "Cmd": cmd, "Labels": labels, "ExposedPorts": exposed,
		"HostConfig": hostConfig{Binds: []string{e.Socket + ":/var/run/docker.sock:ro,Z", filepath.Join(dataDir, "dynamic") + ":/etc/traefik/dynamic:ro,Z", filepath.Join(dataDir, "certs") + ":/etc/traefik/certs:ro,Z"}, ExtraHosts: []string{"host.docker.internal:host-gateway"}, PortBindings: bindings, RestartPolicy: map[string]any{"Name": "unless-stopped"}, SecurityOpt: []string{"label=disable"}},
	}
	var created createContainerResponse
	resp, err := e.request(ctx, http.MethodPost, "/containers/create?name="+name, body)
	if err != nil {
		return err
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&created)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("decode Traefik container response: %w", err)
	}
	r, err := e.request(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil)
	if err != nil {
		return err
	}
	r.Body.Close()
	connectErr := e.connectNetworks(ctx, created.ID, networks)
	if err := os.WriteFile(signaturePath, signatureBytes, 0600); err != nil {
		return err
	}
	return connectErr
}

func (e *Engine) connectNetworks(ctx context.Context, containerID string, networks []string) error {
	connected, err := e.containerNetworks(ctx, containerID)
	if err != nil {
		return fmt.Errorf("inspect Traefik networks: %w", err)
	}
	for _, network := range networks {
		if network == "bridge" || network == "host" || network == "none" || connected[network] {
			continue
		}
		path := "/networks/" + url.PathEscape(network) + "/connect"
		resp, err := e.request(ctx, http.MethodPost, path, map[string]any{"Container": containerID})
		if err != nil {
			// A concurrent sync may have connected the network after our inspection.
			if current, inspectErr := e.containerNetworks(ctx, containerID); inspectErr == nil && current[network] {
				connected[network] = true
				continue
			}
			return fmt.Errorf("connect Traefik to network %s: %w", network, err)
		}
		resp.Body.Close()
		connected[network] = true
	}
	return nil
}

func (e *Engine) containerNetworks(ctx context.Context, containerID string) (map[string]bool, error) {
	var container struct {
		NetworkSettings struct {
			Networks map[string]json.RawMessage `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := e.jsonRequest(ctx, http.MethodGet, "/containers/"+url.PathEscape(containerID)+"/json", &container); err != nil {
		return nil, err
	}
	connected := make(map[string]bool, len(container.NetworkSettings.Networks))
	for network := range container.NetworkSettings.Networks {
		connected[network] = true
	}
	return connected, nil
}

func uniquePorts(in []Port) []Port {
	seen := map[string]bool{}
	out := make([]Port, 0, len(in))
	for _, p := range in {
		key := fmt.Sprintf("%d/%s", p.PrivatePort, p.Protocol)
		if p.PrivatePort > 0 && !seen[key] {
			seen[key] = true
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PrivatePort == out[j].PrivatePort {
			return out[i].Protocol < out[j].Protocol
		}
		return out[i].PrivatePort < out[j].PrivatePort
	})
	return out
}

func (e *Engine) pullImage(ctx context.Context, image, tag string) error {
	resp, err := e.request(ctx, http.MethodPost, "/images/create?fromImage="+image+"&tag="+tag, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Consume the stream to completion; progress JSON may be newline-delimited.
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 64<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var status struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(scanner.Bytes(), &status)
		if status.Error != "" {
			return fmt.Errorf("pull %s:%s: %s", image, tag, status.Error)
		}
	}
	return scanner.Err()
}

func (e *Engine) stopTraefik(ctx context.Context) error {
	if r, err := e.request(ctx, http.MethodPost, "/containers/dock-traefik/stop?t=5", nil); err == nil {
		r.Body.Close()
	}
	r, err := e.request(ctx, http.MethodDelete, "/containers/dock-traefik?force=true&v=true", nil)
	if err != nil && !strings.Contains(err.Error(), "No such container") {
		return err
	}
	if r != nil {
		r.Body.Close()
	}
	return nil
}
