package dock

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// POSIX shell builtins avoid a dependency on cat, ss, netstat or root inside
// the image. The exec runs with the container's configured user and privileges.
const socketTableCommand = `for table in tcp tcp6 udp udp6; do
  [ -r "/proc/net/$table" ] || continue
  printf 'DOCK_PORTS %s\n' "$table"
  while IFS= read -r line; do printf '%s\n' "$line"; done < "/proc/net/$table"
done`

func (e *Engine) listeningPorts(ctx context.Context, containerID string) ([]Port, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := e.request(ctx, http.MethodPost, "/containers/"+containerID+"/exec", map[string]any{
		"AttachStdout": true, "AttachStderr": true, "Tty": false,
		"Cmd": []string{"sh", "-c", socketTableCommand},
	})
	if err != nil {
		return nil, err
	}
	var created struct {
		ID string `json:"Id"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&created)
	resp.Body.Close()
	if err != nil || created.ID == "" {
		return nil, fmt.Errorf("create socket inspection exec: invalid runtime response")
	}
	resp, err = e.request(ctx, http.MethodPost, "/exec/"+created.ID+"/start", map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return nil, err
	}
	stdout, stderr, err := readExecOutput(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var status struct {
		Running  bool `json:"Running"`
		ExitCode int  `json:"ExitCode"`
	}
	if err := e.jsonRequest(ctx, http.MethodGet, "/exec/"+created.ID+"/json", &status); err != nil {
		return nil, err
	}
	if status.Running || status.ExitCode != 0 {
		message := strings.TrimSpace(string(stderr))
		if message == "" {
			message = "image must provide a POSIX shell and readable /proc/net socket tables"
		}
		return nil, fmt.Errorf("socket inspection failed: %s", message)
	}
	return parseSocketTables(stdout)
}

// Docker-compatible exec uses an 8-byte header for each stdout/stderr frame.
func readExecOutput(reader io.Reader) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	for {
		var header [8]byte
		_, err := io.ReadFull(reader, header[:])
		if err == io.EOF {
			return stdout.Bytes(), stderr.Bytes(), nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read socket inspection stream: %w", err)
		}
		size := int64(binary.BigEndian.Uint32(header[4:]))
		if size > 1<<20 || int64(stdout.Len()+stderr.Len())+size > 1<<20 {
			return nil, nil, fmt.Errorf("socket inspection output exceeds 1 MiB")
		}
		var output io.Writer
		switch header[0] {
		case 1:
			output = &stdout
		case 2:
			output = &stderr
		default:
			return nil, nil, fmt.Errorf("unexpected socket inspection stream %d", header[0])
		}
		if _, err := io.CopyN(output, reader, size); err != nil {
			return nil, nil, fmt.Errorf("read socket inspection frame: %w", err)
		}
	}
}

func parseSocketTables(data []byte) ([]Port, error) {
	seen := map[Port]bool{}
	table, tables := "", 0
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "DOCK_PORTS" {
			table = fields[1]
			tables++
			continue
		}
		if len(fields) < 4 || fields[1] == "local_address" || table == "" {
			continue
		}
		protocol := "tcp"
		if table == "udp" || table == "udp6" {
			protocol = "udp"
			if fields[3] != "07" { // Ignore connected UDP clients.
				continue
			}
		} else if fields[3] != "0A" { // TCP LISTEN, not established clients.
			continue
		}
		address := strings.Split(fields[1], ":")
		if len(address) != 2 {
			continue
		}
		ip, err := socketTableIP(address[0])
		if err != nil || ip.IsLoopback() {
			continue
		}
		port, err := strconv.ParseUint(address[1], 16, 16)
		if err == nil && port != 0 {
			seen[Port{PrivatePort: int(port), Protocol: protocol}] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if tables == 0 {
		return nil, fmt.Errorf("no readable socket tables; image must provide a POSIX shell and /proc/net")
	}
	ports := make([]Port, 0, len(seen))
	for port := range seen {
		ports = append(ports, port)
	}
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].PrivatePort == ports[j].PrivatePort {
			return ports[i].Protocol < ports[j].Protocol
		}
		return ports[i].PrivatePort < ports[j].PrivatePort
	})
	return ports, nil
}

func socketTableIP(value string) (net.IP, error) {
	data, err := hex.DecodeString(value)
	if err != nil || (len(data) != net.IPv4len && len(data) != net.IPv6len) {
		return nil, fmt.Errorf("invalid socket address")
	}
	// /proc encodes IPv4 and each IPv6 32-bit word in native byte order.
	for i := 0; i < len(data); i += 4 {
		word := binary.NativeEndian.Uint32(data[i : i+4])
		binary.BigEndian.PutUint32(data[i:i+4], word)
	}
	return net.IP(data), nil
}
