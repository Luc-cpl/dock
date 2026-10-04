package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"dock/internal/dock"
	"gopkg.in/yaml.v3"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "dock:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if args[0] == "daemon" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		return dock.DaemonCommand(args[1:], executable)
	}
	m, err := dock.NewManager()
	if err != nil {
		return err
	}
	switch args[0] {
	case "help", "-h", "--help":
		usage()
		return nil
	case "trust":
		return dock.Trust()
	case "init":
		return initManifest(ctx, m, args[1:])
	case "serve":
		signalCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		fmt.Println("Dock is serving the dashboard at https://localhost")
		if err := dock.Serve(signalCtx, m); err != nil {
			return err
		}
		return nil
	case "status":
		if err := m.RefreshInventory(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "runtime: %s\n", err)
		}
		for key, value := range m.Status() {
			fmt.Printf("%-16s %v\n", key, value)
		}
		return nil
	case "stop":
		c, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		return m.Stop(c)
	case "routes":
		if len(args) < 2 {
			return errors.New("use routes list, routes apply, or routes delete")
		}
		switch args[1] {
		case "list":
			return listRoutes(m)
		case "apply":
			return applyRoutes(ctx, m, args[2:])
		case "delete":
			return deleteRoutes(ctx, m, args[2:])
		default:
			return fmt.Errorf("unknown subcommand: routes %s", args[1])
		}
	default:
		return fmt.Errorf("unknown command %q (use dock help)", args[0])
	}
}

func usage() {
	fmt.Println(`Dock - local container gateway

Usage:
  dock init [--file dock.yml] [--project name] [--force]
  dock serve
  dock daemon install|uninstall|enable|disable|start|stop|status
  dock status
  dock trust
  dock stop
  dock routes list
  dock routes apply [--file dock.yml] [--project name]
  dock routes delete --file dock.yml`)
}

func initManifest(ctx context.Context, m *dock.Manager, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	file := fs.String("file", "dock.yml", "output YAML manifest")
	project := fs.String("project", "", "Compose project (auto-detected from the current directory)")
	force := fs.Bool("force", false, "replace an existing manifest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := m.RefreshInventory(ctx); err != nil {
		return fmt.Errorf("discover running Compose project: %w", err)
	}
	projects, err := m.FindProject(".")
	if err != nil {
		return err
	}
	if *project == "" {
		switch len(projects) {
		case 0:
			cwd, _ := filepath.Abs(".")
			return fmt.Errorf("no running Compose project declares working_dir=%s", cwd)
		case 1:
			*project = projects[0]
		default:
			return fmt.Errorf("multiple running Compose projects match this directory (%s); use --project", strings.Join(projects, ", "))
		}
	} else {
		found := false
		for _, candidate := range projects {
			if candidate == *project {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("Compose project %q does not declare working_dir for the current directory", *project)
		}
	}
	routes := m.DiscoveredRoutes(*project)
	if len(routes) == 0 {
		return fmt.Errorf("no recognized HTTP or TCP ports found for running Compose project %q", *project)
	}
	data, err := yaml.Marshal(dock.StarterManifest(routes))
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	data = append([]byte("# Protocols: http, https, tcp, udp. HTTPS always redirects HTTP.\n# port: container port (HTTP defaults to 80, HTTPS to 443), or \"host:container\".\n# hostname is relative to the automatically detected Compose project.\n# disabled: true hides the route and removes its mapping.\n"), data...)
	path, err := filepath.Abs(*file)
	if err != nil {
		return err
	}
	if *force {
		err = os.WriteFile(path, data, 0644)
	} else {
		var output *os.File
		output, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err == nil {
			_, writeErr := output.Write(data)
			closeErr := output.Close()
			if writeErr != nil {
				err = writeErr
			} else {
				err = closeErr
			}
		}
	}
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists; use --force to replace it", path)
		}
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Printf("Created %s with %d routes for Compose project %q\n", path, len(routes), *project)
	return nil
}

func listRoutes(m *dock.Manager) error {
	routes := m.Routes()
	if len(routes) == 0 {
		fmt.Println("No routes configured.")
		return nil
	}
	fmt.Printf("%-24s %-38s %-10s %-8s %-9s %s\n", "ID", "HOSTNAME", "DESTINATION", "PROTO", "PORT", "STATUS")
	for _, r := range routes {
		dst := r.Container
		if dst == "" {
			dst = r.Service + "." + r.Project
		}
		host := r.Hostname
		if r.Protocol != "http" {
			host = fmt.Sprintf("localhost:%d (dynamic)", r.ListenPort)
		} else if r.TLS {
			host = "https://" + host
		} else if host != "" {
			host = "http://" + host
		}
		port := fmt.Sprintf("%d -> %d", r.ListenPort, r.Port)
		if r.Protocol == "http" && r.ListenPort == (80) {
			port = fmt.Sprintf("%d", r.Port)
		}
		fmt.Printf("%-24s %-38s %-10s %-8s %-9s %s\n", r.ID, host, dst, r.Protocol, port, r.Status)
	}
	return nil
}

func applyRoutes(ctx context.Context, m *dock.Manager, args []string) error {
	fs := flag.NewFlagSet("routes apply", flag.ContinueOnError)
	file := fs.String("file", "dock.yml", "YAML manifest")
	project := fs.String("project", "", "Compose project")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manifest, path, err := dock.ReadManifest(*file)
	if err != nil {
		return err
	}
	if *project == "" {
		*project = manifest.Project
	}
	needsProject := len(manifest.Services) > 0
	needsInventory := len(manifest.Services) > 0
	for _, r := range manifest.Routes {
		hostname := strings.ToLower(strings.TrimSpace(r.Hostname))
		if r.Service == "" && r.Container == "" {
			needsInventory = true
		}
		if r.Project == "" && (r.Service != "" || (hostname != "" && !strings.HasSuffix(hostname, ".localhost"))) {
			needsProject = true
		}
	}
	if needsProject && *project == "" {
		needsInventory = true
	}
	if needsInventory {
		if err := m.RefreshInventory(ctx); err != nil {
			return fmt.Errorf("discover Compose project and services: %w", err)
		}
	}
	if needsProject && *project == "" {
		projects, err := m.FindProject(filepath.Dir(path))
		if err != nil {
			return err
		}
		if len(projects) == 1 {
			*project = projects[0]
		} else if len(projects) > 1 {
			return fmt.Errorf("multiple Compose projects match %s (%s); use --project", filepath.Dir(path), strings.Join(projects, ", "))
		} else {
			return fmt.Errorf("no running Compose project declares working_dir=%s; set project in the YAML or use --project", filepath.Dir(path))
		}
	}
	if err := m.ApplyManifest(path, *project, manifest); err != nil {
		return err
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := m.Sync(c); err != nil {
		return err
	}
	fmt.Printf("Applied %d routes from %s\n", manifest.RouteCount(), path)
	return nil
}

func deleteRoutes(ctx context.Context, m *dock.Manager, args []string) error {
	fs := flag.NewFlagSet("routes delete", flag.ContinueOnError)
	file := fs.String("file", "", "YAML manifest")
	id := fs.String("id", "", "dashboard route ID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file != "" {
		if err := m.DeleteManifest(*file); err != nil {
			return err
		}
	} else if *id != "" {
		found := false
		for _, r := range m.Routes() {
			if r.ID == *id && r.Owner == "ui" {
				if err := m.DeleteRoute("ui/" + r.ID); err != nil {
					return err
				}
				found = true
			}
		}
		if !found {
			return fmt.Errorf("dashboard route %q not found", *id)
		}
	} else {
		return errors.New("provide --file or --id")
	}
	c, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := m.Sync(c); err != nil {
		return err
	}
	fmt.Println("Routes removed.")
	return nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
