export type PortInfo = { privatePort: number; protocol: string }
export type ContainerInfo = {
  id: string; name: string; image: string; state: string; project?: string; service?: string
  networks: string[]; ports: PortInfo[]; hasTraefikRules: boolean; discoveryError?: string; portDiscoveryError?: string
}
export type RouteInfo = {
  id: string; hostname?: string; project?: string; service?: string; container?: string
  protocol: string; port: number; listenPort?: number; tls: boolean; redirectHttps: boolean
  enabled: boolean; owner?: string; status?: string; message?: string
}
export type RuntimeStatus = {
  ok: boolean; engine: { socket?: string; error?: string }; traefik: { running: boolean }
  lastSync?: string; error?: string; containerCount: number; routeCount: number; dataDir: string
}
