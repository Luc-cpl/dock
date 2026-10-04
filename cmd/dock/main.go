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
		return errors.New("use dock routes init to create dock.yml")
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
			routesUsage()
			return nil
		}
		switch args[1] {
		case "init":
			return initManifest(ctx, m, args[2:])
		case "help", "-h", "--help":
			routesUsage()
			return nil
		case "list":
			return listRoutes(m)
		case "apply":
			return applyRoutes(ctx, m, args[2:])
		case "delete":
			return deleteRoutes(ctx, m, args[2:])
		case "sync":
			return syncRoutes(ctx, m, args[2:])
		case "export":
			return errors.New("use dock routes sync to save dashboard routes to dock.yml")
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
  dock serve
  dock daemon install|uninstall|enable|disable|start|stop|status
  dock status
  dock trust
  dock stop
  dock routes init [--file dock.yml] [--project name] [--force]
  dock routes apply [--file dock.yml] [--project name]
  dock routes sync [--file dock.yml] [--project name]
  dock routes list
  dock routes delete --file dock.yml | --id dashboard-route-id

Route configuration flow:
  init   Compose discovery -> dock.yml (use --force to replace an existing file)
  apply  dock.yml -> Dock
  sync   Dock dashboard -> dock.yml (replaces the file with current routes)`)
}

func routesUsage() {
	fmt.Println(`Usage:
  dock routes init [--file dock.yml] [--project name] [--force]
  dock routes apply [--file dock.yml] [--project name]
  dock routes sync [--file dock.yml] [--project name]
  dock routes list
  dock routes delete --file dock.yml | --id dashboard-route-id

  init    Create dock.yml from the running Compose project's discovered routes.
  apply   Load dock.yml into Dock, replacing routes owned by that file.
  sync    Save the project's current dashboard routes to dock.yml, replacing the file.
  list    Show current routes and their status.
  delete  Remove file-managed routes or a dashboard-created route (--id) from Dock.

init and sync detect the Compose project from the current directory.
apply detects it from the configuration file's directory.
Use --project to select a project explicitly. sync is one-way: Dock -> file.`)
}

func manifestFlags(name, description string) *flag.FlagSet {
	fs := flag.NewFlagSet("routes "+name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), description)
		fmt.Fprintf(fs.Output(), "Usage: dock routes %s [options]\n", name)
		fs.PrintDefaults()
	}
	return fs
}

func syncRoutes(ctx context.Context, m *dock.Manager, args []string) error {
	fs := manifestFlags("sync", "Save the current dashboard routes to dock.yml (Dock -> file), replacing the file.")
	file := fs.String("file", "dock.yml", "output YAML manifest")
	project := fs.String("project", "", "Compose project (auto-detected from the current directory)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if err := m.RefreshInventory(ctx); err != nil {
		return fmt.Errorf("discover current dashboard routes: %w", err)
	}
	selected, err := resolveComposeProject(m, *project)
	if err != nil {
		return err
	}
	manifest, err := m.ExportRoutes(selected)
	if err != nil {
		return err
	}
	data, err := dock.EncodeManifest(manifest)
	if err != nil {
		return err
	}
	path, err := filepath.Abs(*file)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return err
	}
	count := 0
	for _, routes := range manifest.Services {
		count += len(routes)
	}
	fmt.Printf("Synced %d routes for Compose project %q to %s\n", count, selected, path)
	return nil
}

func initManifest(ctx context.Context, m *dock.Manager, args []string) error {
	fs := manifestFlags("init", "Create dock.yml from Compose discovery. An existing file requires --force.")
	file := fs.String("file", "dock.yml", "output YAML manifest")
	project := fs.String("project", "", "Compose project (auto-detected from the current directory)")
	force := fs.Bool("force", false, "replace an existing manifest")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if err := m.RefreshInventory(ctx); err != nil {
		return fmt.Errorf("discover running Compose project: %w", err)
	}
	selected, err := resolveComposeProject(m, *project)
	if err != nil {
		return err
	}
	*project = selected
	routes := m.DiscoveredRoutes(*project)
	if len(routes) == 0 {
		return fmt.Errorf("no recognized HTTP or TCP ports found for running Compose project %q", *project)
	}
	data, err := dock.EncodeManifest(dock.StarterManifest(routes))
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
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

func resolveComposeProject(m *dock.Manager, project string) (string, error) {
	if project != "" {
		return project, nil
	}
	projects, err := m.FindProject(".")
	if err != nil {
		return "", err
	}
	switch len(projects) {
	case 0:
		cwd, _ := filepath.Abs(".")
		return "", fmt.Errorf("no running Compose project declares working_dir=%s; use --project", cwd)
	case 1:
		return projects[0], nil
	default:
		return "", fmt.Errorf("multiple running Compose projects match this directory (%s); use --project", strings.Join(projects, ", "))
	}
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
	fs := manifestFlags("apply", "Load dock.yml into Dock (file -> Dock), replacing routes owned by that file.")
	file := fs.String("file", "dock.yml", "YAML manifest")
	project := fs.String("project", "", "Compose project")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
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
