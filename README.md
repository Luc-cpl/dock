# Dock

Dock is a Linux-first local container gateway. A Go daemon serves the React/MUI dashboard, watches one local Docker-compatible runtime and manages a Traefik container. Existing Compose files are left alone.

## Build

Requirements: Go 1.27+, Node.js 22+, npm, a Docker Engine or rootless Podman API socket, and `mkcert` for trusted HTTPS.

```sh
make link-dev
dock serve
```

Use `make build` when you only need a workspace binary at `./dock`; use `make link-global` to connect that binary to the global command for local release testing.

`make link-dev` places a per-user command link at `~/.local/bin/dock`. Ensure `~/.local/bin` is on `PATH`. In this mode each invocation builds the current Go source before running it, and the launcher keeps the directory where you called `dock` as its working directory. Run `make web` after changing the React UI so the embedded dashboard is refreshed. The dashboard and its API are available through `https://localhost`; direct requests to the internal HTTP backend are rejected. Dock auto-detects `/var/run/docker.sock` and `$XDG_RUNTIME_DIR/podman/podman.sock`; set `DOCKER_HOST=unix:///path/to/socket` to choose another local socket. It uses `$XDG_DATA_HOME/dock` or `~/.local/share/dock` for configuration, certificates and Traefik's dynamic configuration.

## Run Dock as a user service

Dock can run as a systemd user service. Install the unit without enabling it, then enable it when you want Dock to start at login:

```sh
dock daemon install
dock daemon enable
```

Use `dock daemon start`, `stop`, `status`, or `disable` to manage it. `dock daemon uninstall` disables and stops the service, then removes the unit. Stopping the daemon also stops its managed Traefik container; starting it recreates or starts Traefik as needed. The user service starts with the user's systemd session.

For local testing of a standalone build, run `make link-global`. It rebuilds the frontend and binary, then points the same per-user `dock` command at the workspace binary. This is a local test link, not a package installation or publication. Switch back with `make link-dev`; remove the link with `make unlink-global`. These targets refuse to replace or remove an unrelated file or link.

## Trusted local HTTPS

Install `mkcert`, then run:

```sh
dock trust
```

This installs mkcert's local CA in the current user's system trust store and creates the dashboard certificate for `localhost`. If no Firefox or Chromium certificate database exists, mkcert may report that browser stores were skipped; Dock treats this as informational and continues with system trust. Dock issues the exact or wildcard certificates required by HTTPS routes and never mounts the CA private key into Traefik. HTTPS uses port 443 by default and forwards to the selected HTTP port inside the container. Every HTTPS route automatically redirects HTTP requests on port 80 to its configured HTTPS listener, including custom ports.

## Routes

Dock discovers Compose projects and standalone containers separately. Recognized HTTP ports receive host routes (`service.project.localhost` or `container.localhost`); a Compose service gets one route without an additional container-name alias. Recognized raw TCP ports, including PostgreSQL `5432` and Mailpit's SMTP/POP3 ports `1025` and `1110`, use TCP listeners. Dock leaves unrecognized ports for explicit configuration in the dashboard or `dock.yml`, where a route can belong to a project or target a standalone container. Existing `traefik.*` labels continue to be interpreted by Traefik's native Docker provider.

Generate `dock.yml` from the running Compose project in the current directory:

```sh
dock init
```

Dock discovers the project and service names from the runtime. Each service key contains its route array directly, with readable protocols, relative hostnames and port mappings. No route IDs or project name need to be entered. `--force` replaces an existing manifest; `--file` selects another output path.

For example, edit the generated file to enable HTTPS for `web.app`:

```yaml
version: 1
services:
  web.app:
    - hostname: app
      protocol: https
      port: 80
  mailpit:
    - hostname: mailpit
      protocol: http
      port: "8025:8025"
    - protocol: tcp
      port: "1025:1025"
  pgsql:
    - protocol: tcp
      port: "15432:5432"
```

With a project named `example`, this creates `https://app.example.localhost`, forwarding to port 80 inside the `web.app` container. Changing `protocol: http` to `protocol: https` enables TLS, certificates and automatic HTTP redirection. TLS terminates at Dock; the container port remains the application's HTTP port. There are no `tls` or redirect options in this format.

`port` accepts a container port or a quoted `"host:container"` mapping, using the same order as Compose:

| Route | Configuration | Entry port → container port |
| --- | --- | --- |
| HTTP with its default listener | `protocol: http`, `port: 8080` | 80 → 8080 |
| HTTPS with its default listener | `protocol: https`, `port: 80` | 443 → 80 |
| HTTPS on a custom entry port | `protocol: https`, `port: "8443:80"` | 8443 → 80 |
| PostgreSQL on a custom host port | `protocol: tcp`, `port: "15432:5432"` | 15432 → 5432 |
| UDP with the same host/container port | `protocol: udp`, `port: 5353` | 5353 → 5353 |

For `https`, HTTP on port 80 always redirects to the selected HTTPS port: the custom example redirects to `https://app.example.localhost:8443`. An HTTPS route reserves HTTP port 80 for that hostname, so a separate HTTP route for the same hostname on port 80 cannot coexist with it. `tcp` and `udp` use raw port forwarding without hostnames or Dock-managed TLS. In the dashboard their entry address appears as `localhost:<host-port>` with a Dynamic badge, and the next line shows the host-to-container mapping; Dynamic identifies a listener managed by Dock, not a randomly selected port. Container-only TCP/UDP ports use the same port on the host. Custom listener ports must be above 1023; HTTP/HTTPS also support 80 and 443, and Dock reserves 9080 and 9180.

Hostnames in the file are relative to the automatically detected project. `hostname: app` becomes `app.example.localhost`; `"*"` becomes `*.example.localhost`, and `"*.api"` becomes `*.api.example.localhost`. If an HTTP/HTTPS hostname is omitted, Dock uses the service name with punctuation replaced by hyphens. A hostname ending in `.localhost` is treated as an explicit full address. A service can contain multiple routes; use distinct hostnames or entry ports for different HTTP destinations. Add `disabled: true` to a route to remove its listener, proxy mapping and dashboard/CLI listing. Dock also suppresses automatic rediscovery of that destination. The route stays hidden until the configuration is enabled again; it is not shown with a Disabled status. Disabling a route through the dashboard remains a visible status toggle.

Apply the file from the project directory:

```sh
dock routes apply
dock routes list
```

Dock discovers the running project from the Compose working-directory labels beside `dock.yml`. Use `--project` when multiple projects share that directory, or when the manifest is stored in a different directory:

```sh
dock routes apply --file ./ops/dock.yml --project example
dock routes delete --file ./dock.yml
```

Applying a manifest is atomic and idempotent. Internal IDs are derived automatically, and routes removed from the file are removed only from that manifest's ownership set. The daemon reloads CLI changes for both the dashboard and proxy configuration. Dock does not run Compose or edit its files. A configured route replaces automatic discovery for the same service and container port, including when the host port or hostname changes.

Existing flat `routes:` manifests remain supported for compatibility and standalone containers. The new generated `services:` format replaces IDs and explicit project/service fields on every route. Do not mix `services:` and legacy `routes:` in the same file.

## Runtime and networking

Containers listen on internal ports in their own network namespaces. Several projects can all use the same internal port without a host collision. Docker/Compose `ports:` publishes a port directly on the host; services routed through Dock should omit those publications. Dock connects Traefik to the application's bridge network and forwards to the container IP and internal port instead. Existing published bindings belong to the runtime and cannot be removed from a running container by a gateway; changing those bindings requires recreating that container.

For example, two separate Compose projects can both run this service without competing for a host port:

```yaml
services:
  web:
    image: nginx:alpine
    # No ports: publication is needed for Dock.
```

The application still listens on port 80 inside each container. Dock discovers that internal port and exposes each project through its own `web.<project>.localhost` hostname. `expose:` describes internal ports and is optional for live socket discovery; it does not publish them on the host. Applications must bind to a container network interface (usually `0.0.0.0` or `::`), rather than only to container loopback. Keep the default bridge network isolation; `network_mode: host` shares the host network and defeats this separation.

HTTP/HTTPS routes can share one host listener because the hostname selects the destination. Ordinary TCP/UDP streams do not carry an HTTP hostname, so routes to different applications must use different host listener ports. Dock avoids ambiguous automatic TCP mappings and validates explicitly configured listeners.

Dock starts Traefik Proxy v3 on loopback ports 80, 443 and 9180 (dashboard). For discovered application ports it publishes the matching port on loopback. Traefik joins the bridge networks used by routes and honors `traefik.docker.network` when selecting an upstream. A newly needed port or network can require Traefik recreation. The dashboard reports unavailable destinations and collisions. Keep the runtime socket private: access to the Docker-compatible API is equivalent to control of that runtime.

Automatic discovery combines runtime port metadata with live TCP/UDP socket tables inside each container. This discovers listeners missing from image `EXPOSE` metadata, without scanning or publishing host ports. The live inspection includes listening TCP sockets and bound UDP server sockets accessible from the container network; loopback-only listeners and client connections are excluded from its results. Socket inspection uses a short read-only command with the container's configured user and requires a POSIX shell and readable `/proc/net`. If inspection is unavailable, Dock retains metadata discovery and reports the limitation in the dashboard. Known HTTP ports include 9000 and 9001; unrecognized application protocols remain available for explicit routes.

## Remove Dock resources

Run `dock stop` to remove Dock's Traefik container while retaining routes and certificates. To remove all Dock data as well, stop the daemon and remove `$XDG_DATA_HOME/dock` (or `~/.local/share/dock`). Run `make unlink-global` to remove the workspace command link. `mkcert -uninstall` removes mkcert's local CA from the OS trust store; only do that if no other local development tool relies on it.
