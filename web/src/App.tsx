import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Alert, AppBar, Avatar, Box, Button, Chip, CircularProgress, Container,
  Dialog, DialogActions, DialogContent, DialogTitle, Divider, FormControl,
  IconButton, InputLabel, LinearProgress, Link, MenuItem,
  Paper, Select, Snackbar, Stack, Switch, Tab, Table, TableBody, TableCell,
  TableContainer, TableHead, TableRow, Tabs, TextField, Toolbar, Tooltip,
  Typography,
} from '@mui/material'
import AddRounded from '@mui/icons-material/AddRounded'
import ArrowOutwardRounded from '@mui/icons-material/ArrowOutwardRounded'
import AutorenewRounded from '@mui/icons-material/AutorenewRounded'
import CodeRounded from '@mui/icons-material/CodeRounded'
import DeleteOutlineRounded from '@mui/icons-material/DeleteOutlineRounded'
import DnsRounded from '@mui/icons-material/DnsRounded'
import EditRounded from '@mui/icons-material/EditRounded'
import ExpandMoreRounded from '@mui/icons-material/ExpandMoreRounded'
import ChevronRightRounded from '@mui/icons-material/ChevronRightRounded'
import FolderOpenRounded from '@mui/icons-material/FolderOpenRounded'
import HubRounded from '@mui/icons-material/HubRounded'
import LanRounded from '@mui/icons-material/LanRounded'
import LockRounded from '@mui/icons-material/LockRounded'
import OpenInNewRounded from '@mui/icons-material/OpenInNewRounded'
import SettingsRounded from '@mui/icons-material/SettingsRounded'
import TerminalRounded from '@mui/icons-material/TerminalRounded'
import { ContainerInfo, RouteInfo, RuntimeStatus } from './types'
import './style.css'

type Routed = { key: string; route: RouteInfo }
type Toast = { message: string; severity: 'success' | 'error' | 'info' | 'warning' }

const api = async <T,>(path: string, init?: RequestInit): Promise<T> => {
  const response = await fetch(path, { ...init, headers: { 'Content-Type': 'application/json', ...init?.headers } })
  if (!response.ok) {
    let message = `${response.status} ${response.statusText}`
    try { message = (await response.json()).error || message } catch { /* keep status text */ }
    throw new Error(message)
  }
  if (response.status === 204) return undefined as T
  return response.json() as Promise<T>
}

const dot = (tone: string) => <Box component="span" className="status-dot" sx={{ bgcolor: tone }} />
const routeAddress = (route: Pick<RouteInfo, 'protocol' | 'tls' | 'hostname' | 'listenPort'>) => {
  if (route.protocol !== 'http') return `localhost:${route.listenPort || '—'}`
  const port = route.listenPort || (route.tls ? 443 : 80)
  return `${route.tls ? 'https' : 'http'}://${route.hostname || 'hostname.localhost'}${port === (route.tls ? 443 : 80) ? '' : `:${port}`}`
}
const routeDestination = (route: RouteInfo) => route.service ? route.service : route.container || 'Destination unavailable'

function App() {
  const [tab, setTab] = useState(0)
  const [containers, setContainers] = useState<ContainerInfo[]>([])
  const [routes, setRoutes] = useState<Routed[]>([])
  const [status, setStatus] = useState<RuntimeStatus | null>(null)
  const [loading, setLoading] = useState(true)
  const [dialog, setDialog] = useState(false)
  const [editing, setEditing] = useState<Routed | null>(null)
  const [saving, setSaving] = useState(false)
  const [formError, setFormError] = useState<string | null>(null)
  const [busyRoute, setBusyRoute] = useState<string | null>(null)
  const [toast, setToast] = useState<Toast | null>(null)
  const [query, setQuery] = useState('')
  const [selectedProject, setSelectedProject] = useState<string | null>(null)
  const [collapsedGroups, setCollapsedGroups] = useState<Set<string>>(() => new Set())
  const [form, setForm] = useState({ id: '', hostname: '', target: '', protocol: 'http', port: '', listenPort: '', tls: true, redirectHttps: false })

  const refresh = useCallback(async (quiet = false) => {
    if (!quiet) setLoading(true)
    try {
      const [s, c, r] = await Promise.all([
        api<RuntimeStatus>('/api/status'),
        api<ContainerInfo[]>('/api/containers'),
        api<Routed[]>('/api/routes'),
      ])
      setStatus(s)
      setContainers(c.map(container => ({ ...container, ports: container.ports ?? [], networks: container.networks ?? [] })))
      setRoutes(r)
    } catch (error) {
      if (!quiet) setToast({ message: (error as Error).message, severity: 'error' })
    } finally { if (!quiet) setLoading(false) }
  }, [])

  useEffect(() => { void refresh(); const timer = window.setInterval(() => void refresh(true), 3000); return () => window.clearInterval(timer) }, [refresh])

  const targets = useMemo(() => {
    const found = new Map<string, ContainerInfo>()
    for (const c of containers) {
      const key = c.service ? `service:${c.project}/${c.service}` : `container:${c.name}`
      if (!found.has(key)) found.set(key, c)
    }
    return [...found.entries()].map(([key, c]) => ({ key, label: c.service ? `${c.service}  ·  ${c.project}` : c.name, c }))
  }, [containers])

  const selectedTarget = targets.find(t => t.key === form.target)
  const selectedPorts = selectedTarget?.c.ports || []
  const destinationLocked = !!editing && editing.route.owner !== 'ui'
  const formProtocol = form.protocol === 'http' && form.tls ? 'https' : form.protocol
  const formAddress = routeAddress({ protocol: form.protocol, tls: form.tls, hostname: form.hostname.trim(), listenPort: form.tls ? 443 : Number(form.listenPort) || undefined })
  const activeCount = containers.filter(c => c.state === 'running').length
  const projectGroups = useMemo(() => {
    const groups = new Map<string, ContainerInfo[]>()
    containers.filter(c => c.project).forEach(c => groups.set(c.project!, [...(groups.get(c.project!) || []), c]))
    return [...groups.entries()].sort(([a], [b]) => a.localeCompare(b))
  }, [containers])
  const looseContainers = containers.filter(c => !c.project)
  const filteredRoutes = routes.filter(({ route }) => `${route.hostname || ''} ${route.id} ${route.service || ''} ${route.project || ''} ${route.container || ''} ${route.protocol}`.toLowerCase().includes(query.toLowerCase()))
  const routeGroups = useMemo(() => {
    const groups = new Map<string, Routed[]>()
    filteredRoutes.forEach(item => {
      const project = item.route.project || (item.route.service ? 'Project not identified' : 'Standalone routes')
      groups.set(project, [...(groups.get(project) || []), item])
    })
    return [...groups.entries()].sort(([a], [b]) => a === 'Standalone routes' ? 1 : b === 'Standalone routes' ? -1 : a.localeCompare(b))
  }, [filteredRoutes])
  const toggleGroup = (group: string) => setCollapsedGroups(previous => {
    const next = new Set(previous)
    if (next.has(group)) next.delete(group)
    else next.add(group)
    return next
  })

  const openNewRoute = () => {
    const target = targets[0]
    const suggested = target?.c.ports.some(p => p.privatePort === 5432) ? 'tcp' : 'http'
    const port = target?.c.ports.find(p => p.protocol === 'tcp')?.privatePort
    setEditing(null)
    setFormError(null)
    setForm({ id: '', hostname: '', target: target?.key || '', protocol: suggested, port: port ? String(port) : '', listenPort: suggested === 'http' ? '443' : port ? String(port) : '', tls: suggested === 'http', redirectHttps: false })
    setDialog(true)
  }

  const openEditRoute = (item: Routed) => {
    const route = item.route
    const target = targets.find(t => route.service
      ? t.c.service === route.service && t.c.project === route.project
      : t.c.name === route.container)
    setEditing(item)
    setFormError(null)
    setForm({ id: route.id, hostname: route.hostname || '', target: target?.key || '', protocol: route.protocol, port: String(route.port), listenPort: route.tls ? '443' : route.listenPort ? String(route.listenPort) : '', tls: route.tls, redirectHttps: route.redirectHttps })
    setDialog(true)
  }

  const saveRoute = async () => {
    setSaving(true)
    setFormError(null)
    try {
      const selected = destinationLocked ? undefined : targets.find(t => t.key === form.target)
      const original = editing?.route
      if (!selected && !original) throw new Error('Select a discovered service or container.')
      if (!form.id.trim() || (form.protocol === 'http' && !form.hostname.trim())) throw new Error('Enter an ID and a hostname for HTTP routes.')
      const route: RouteInfo = {
        id: original?.id || form.id.trim(), hostname: form.protocol === 'http' ? form.hostname.trim() : '',
        project: selected ? (selected.c.project || undefined) : original?.project,
        service: selected ? (selected.c.service || undefined) : original?.service,
        container: selected ? (selected.c.service ? undefined : selected.c.name) : original?.container,
        protocol: form.protocol, port: destinationLocked ? original!.port : Number(form.port), listenPort: form.protocol === 'http' && form.tls ? 443 : form.listenPort ? Number(form.listenPort) : undefined,
        tls: form.protocol === 'http' && form.tls, redirectHttps: form.protocol === 'http' && form.tls, enabled: original?.enabled ?? true,
      }
      const result = editing
        ? await api<{ warning?: string }>(`/api/routes/${encodeURIComponent(editing.key)}`, { method: 'PUT', body: JSON.stringify({ route }) })
        : await api<{ warning?: string }>('/api/routes', { method: 'POST', body: JSON.stringify(route) })
      setDialog(false); setEditing(null); setToast({ message: result.warning ? `Route saved. ${result.warning}` : editing ? 'Route updated.' : 'Route saved and applied.', severity: result.warning ? 'warning' : 'success' }); await refresh(true)
    } catch (error) { setFormError((error as Error).message) } finally { setSaving(false) }
  }

  const toggleRoute = async (item: Routed, enabled: boolean) => {
    setBusyRoute(item.key)
    try {
      const result = await api<{ warning?: string }>(`/api/routes/${encodeURIComponent(item.key)}`, { method: 'PUT', body: JSON.stringify({ enabled }) })
      if (result.warning) setToast({ message: `Route updated. ${result.warning}`, severity: 'warning' })
      await refresh(true)
    }
    catch (error) { setToast({ message: (error as Error).message, severity: 'error' }) }
    finally { setBusyRoute(null) }
  }
  const removeRoute = async (item: Routed) => {
    try { await api(`/api/routes/${encodeURIComponent(item.key)}`, { method: 'DELETE' }); setToast({ message: 'Route removed.', severity: 'success' }); await refresh(true) }
    catch (error) { setToast({ message: (error as Error).message, severity: 'error' }) }
  }

  return <Box className="app-shell">
    <AppBar position="sticky" elevation={0} color="inherit" className="topbar">
      <Toolbar className="topbar-inner">
        <Stack direction="row" alignItems="center" spacing={1.25}>
          <Avatar className="brand-mark"><HubRounded fontSize="small" /></Avatar>
          <Typography fontWeight={750} letterSpacing="-.04em" fontSize={19}>dock</Typography>
          <Chip size="small" label="LOCAL GATEWAY" className="top-chip" />
        </Stack>
        <Box sx={{ flex: 1 }} />
        <Stack direction="row" spacing={1} alignItems="center" className="runtime-indicator">
          {dot(status?.ok ? '#10b981' : '#d97706')}
          <Typography variant="body2" color="text.secondary">{status?.ok ? 'Gateway online' : 'Checking runtime'}</Typography>
        </Stack>
        <Tooltip title="Refresh inventory"><IconButton onClick={() => void refresh()} sx={{ ml: 1 }}><AutorenewRounded /></IconButton></Tooltip>
        <Button variant="contained" startIcon={<AddRounded />} onClick={openNewRoute} sx={{ ml: 1.5 }} className="desktop-action">New route</Button>
      </Toolbar>
    </AppBar>

    <Container maxWidth="xl" className="page-content">
      <Box className="welcome-row">
        <Box>
          <Typography variant="overline" color="primary.main" fontWeight={700} letterSpacing=".1em">CONTAINER GATEWAY</Typography>
          <Typography variant="h1">Your services, in one place.</Typography>
          <Typography color="text.secondary" mt={0.65}>Discover, organize, and access your containers through local routes.</Typography>
        </Box>
        <Button variant="contained" startIcon={<AddRounded />} onClick={openNewRoute} className="mobile-action">New route</Button>
      </Box>

      {status?.error && <Alert severity="warning" sx={{ mb: 2 }}>{status.error}</Alert>}
      {status?.engine?.error && <Alert severity="error" sx={{ mb: 2 }}>Runtime disconnected: {status.engine.error}</Alert>}
      {loading && <LinearProgress sx={{ mb: 1.5, borderRadius: 3 }} />}

      <Box className="stats-grid">
        <StatCard icon={<DnsRounded />} label="Running containers" value={String(activeCount)} detail={`${containers.length} discovered`} tone="indigo" />
        <StatCard icon={<LanRounded />} label="Local routes" value={String(routes.length)} detail={`${routes.filter(x => x.route.status === 'active').length} ready`} tone="teal" />
        <StatCard icon={<LockRounded />} label="Trusted HTTPS" value={String(routes.filter(x => x.route.tls).length)} detail="Local certificates" tone="violet" />
        <StatCard icon={<HubRounded />} label="Runtime" value={status?.engine?.socket ? 'Connected' : 'Waiting'} detail={status?.engine?.socket?.includes('podman') ? 'Podman · local socket' : 'Docker API · local socket'} tone="amber" compact />
      </Box>

      <Paper className="workspace-card">
        <Box className="workspace-top">
          <Tabs value={tab} onChange={(_, v) => setTab(v)} className="workspace-tabs">
            <Tab icon={<LanRounded fontSize="small" />} iconPosition="start" label={`Routes ${routes.length || ''}`} />
            <Tab icon={<FolderOpenRounded fontSize="small" />} iconPosition="start" label={`Projects ${projectGroups.length + looseContainers.length || ''}`} />
            <Tab icon={<SettingsRounded fontSize="small" />} iconPosition="start" label="Settings" />
          </Tabs>
          <TextField size="small" placeholder={tab === 1 ? 'Search projects and containers...' : 'Search routes...'} value={query} onChange={e => setQuery(e.target.value)} className="search-field" />
        </Box>
        <Divider />
        {tab === 0 && <TableContainer className="table-scroll"><Table size="medium">
          <TableHead><TableRow><TableCell>Address</TableCell><TableCell>Destination</TableCell><TableCell>Source</TableCell><TableCell>Protocol</TableCell><TableCell>Status</TableCell><TableCell align="right">Enabled</TableCell><TableCell align="right" width={96}>Actions</TableCell></TableRow></TableHead>
          <TableBody>
            {routeGroups.flatMap(([group, items]) => [
              <TableRow key={`group:${group}`} className="route-group-row"><TableCell colSpan={7}><Button className="route-group-toggle" onClick={() => toggleGroup(group)} aria-expanded={!collapsedGroups.has(group)} aria-label={`${collapsedGroups.has(group) ? 'Expand' : 'Collapse'} ${group}`}><Stack direction="row" alignItems="center" spacing={1}>{collapsedGroups.has(group) ? <ChevronRightRounded fontSize="small" /> : <ExpandMoreRounded fontSize="small" />}<FolderOpenRounded fontSize="small" /><Typography fontWeight={700}>{group}</Typography><Chip size="small" label={items.length} /></Stack></Button></TableCell></TableRow>,
              ...(collapsedGroups.has(group) ? [] : items.map(item => <RouteRow key={item.key} item={item} busy={busyRoute === item.key} onEdit={() => openEditRoute(item)} onToggle={enabled => void toggleRoute(item, enabled)} onRemove={() => void removeRoute(item)} />)),
            ])}
            {!filteredRoutes.length&&<TableRow><TableCell colSpan={7}><EmptyState icon={<LanRounded />} title={routes.length?'No routes found':'No routes configured'} detail={routes.length?'Try another search term.':'Create a route or apply a dock.yml file from your project.'} action={!routes.length&&<Button startIcon={<AddRounded />} onClick={openNewRoute}>Create your first route</Button>} /></TableCell></TableRow>}
          </TableBody>
        </Table></TableContainer>}
        {tab === 1 && <Box sx={{p:2.5}}>
          {selectedProject ? <>
            <Button onClick={()=>setSelectedProject(null)} sx={{mb:1}}>← All projects</Button>
            <Typography variant="h2" sx={{fontSize:20,fontWeight:700,mb:1.5}}>{selectedProject}</Typography>
            <Box className="container-grid" sx={{p:0}}>{(projectGroups.find(([name])=>name===selectedProject)?.[1] || []).filter(c=>`${c.name} ${c.service} ${c.image}`.toLowerCase().includes(query.toLowerCase())).map(c=><ContainerCard key={c.id} container={c} />)}</Box>
          </> : <>
            {!!projectGroups.length && <><Typography variant="overline" color="text.secondary" fontWeight={700}>Projects</Typography><Box className="container-grid" sx={{p:0,mt:1,mb:2}}>
              {projectGroups.filter(([name, cs])=>`${name} ${cs.map(c=>`${c.service} ${c.name} ${c.image}`).join(' ')}`.toLowerCase().includes(query.toLowerCase())).map(([name, cs])=><Paper key={name} className="service-card project-card" onClick={()=>setSelectedProject(name)}><Stack direction="row" alignItems="center" spacing={1.5}><Avatar className="service-avatar"><FolderOpenRounded /></Avatar><Box flex={1}><Typography fontWeight={700}>{name}</Typography><Typography variant="caption" color="text.secondary">{cs.length} service{cs.length===1?'':'s'} · {cs.filter(c=>c.state==='running').length} running</Typography></Box><ArrowOutwardRounded color="action" /></Stack></Paper>)}
            </Box></>}
            {!!looseContainers.length && <><Typography variant="overline" color="text.secondary" fontWeight={700}>Standalone containers</Typography><Box className="container-grid" sx={{p:0,mt:1}}>{looseContainers.filter(c=>`${c.name} ${c.image}`.toLowerCase().includes(query.toLowerCase())).map(c=><ContainerCard key={c.id} container={c} />)}</Box></>}
            {!containers.length&&<EmptyState icon={<DnsRounded />} title="Waiting for containers" detail="Start a project in the connected runtime and it will appear here." />}
          </>}
        </Box>}
        {tab === 2 && <Box className="settings-grid">
          <Paper className="settings-panel"><Stack direction="row" spacing={1.3} alignItems="center"><Avatar className="settings-icon"><HubRounded /></Avatar><Box><Typography fontWeight={650}>Connected runtime</Typography><Typography variant="body2" color="text.secondary">{status?.engine?.socket || 'Local socket not found'}</Typography></Box></Stack><Divider sx={{ my: 2 }} /><SettingLine label="Containers discovered" value={String(containers.length)} /><SettingLine label="Last sync" value={status?.lastSync ? new Date(status.lastSync).toLocaleTimeString('en-US') : 'Waiting'} /><SettingLine label="Compose files" value="Read only" /></Paper>
          <Paper className="settings-panel"><Stack direction="row" spacing={1.3} alignItems="center"><Avatar className="settings-icon violet"><LockRounded /></Avatar><Box><Typography fontWeight={650}>Local TLS</Typography><Typography variant="body2" color="text.secondary">Certificates trusted by the operating system.</Typography></Box></Stack><Divider sx={{ my: 2 }} /><Alert severity="info" icon={<TerminalRounded />}>Run <code>dock trust</code> to trust the local certificate authority. HTTPS routes are issued automatically after setup.</Alert><Typography variant="caption" color="text.secondary" display="block" mt={1.5}>Certificates cover exact names and wildcards added in the dashboard or through dock.yml.</Typography></Paper>
          <Paper className="settings-panel"><Stack direction="row" spacing={1.3} alignItems="center"><Avatar className="settings-icon teal"><CodeRounded /></Avatar><Box><Typography fontWeight={650}>Versioned routes</Typography><Typography variant="body2" color="text.secondary">Keep the project's dashboard routes in dock.yml.</Typography></Box></Stack><Divider sx={{ my: 2 }} /><Box component="pre" className="code-sample">{`dock routes init\ndock routes apply\ndock routes sync`}</Box><Typography variant="caption" color="text.secondary">Init: Compose discovery → file. Apply: file → Dock. Sync: dashboard → file, replacing its contents. Run from the project directory or use --project.</Typography></Paper>
          <Paper className="settings-panel"><Stack direction="row" spacing={1.3} alignItems="center"><Avatar className="settings-icon amber"><FolderOpenRounded /></Avatar><Box><Typography fontWeight={650}>Storage</Typography><Typography variant="body2" color="text.secondary">Persistent state in local JSON files.</Typography></Box></Stack><Divider sx={{ my: 2 }} /><Typography variant="body2" className="path-value">{status?.dataDir || '~/.local/share/dock'}</Typography><Typography variant="caption" color="text.secondary" display="block" mt={.75}>The CA private key is not mounted in the Traefik container.</Typography></Paper>
        </Box>}
      </Paper>

      <Box className="footer-row"><Typography variant="caption" color="text.secondary">DOCK LOCAL GATEWAY <span className="footer-dot">·</span> Traefik Proxy</Typography><Typography variant="caption" color="text.secondary">Ports <b>80</b> · <b>443</b> <span className="footer-dot">·</span> loopback only</Typography></Box>
    </Container>

    <Dialog open={dialog} onClose={()=>!saving&&setDialog(false)} fullWidth maxWidth="sm">
      <DialogTitle><Typography fontWeight={700} fontSize={19}>{editing ? 'Edit route' : 'Create a route'}</Typography><Typography variant="body2" color="text.secondary" mt={.5}>{editing ? 'Update the local address and entry port for this route.' : 'Route a local address to your service.'}</Typography></DialogTitle>
      <DialogContent><Stack spacing={2} pt={1}>
        {editing && <Box className="route-current-address"><Typography variant="caption" color="text.secondary">Current address</Typography><Typography fontWeight={650} fontSize={14} sx={{ overflowWrap: 'anywhere' }}>{routeAddress(editing.route)}</Typography></Box>}
        {formError && <Alert severity="error">{formError}</Alert>}
        {!editing && <TextField label="Route ID" placeholder="web-https" size="small" value={form.id} onChange={e => setForm({ ...form, id: e.target.value })} helperText="Letters, numbers, dots, and hyphens." />}
        {form.protocol === 'http' && <TextField label="Hostname" autoFocus size="small" value={form.hostname} onChange={e => setForm({ ...form, hostname: e.target.value })} helperText="Use a .localhost address or a wildcard such as *.project.localhost." slotProps={{ inputLabel: { shrink: true } }} />}
        {destinationLocked
          ? <TextField label="Destination" size="small" value={routeDestination(editing!.route)} slotProps={{ input: { readOnly: true } }} helperText={editing!.route.owner === 'auto' ? 'Destination and container port are managed by automatic discovery.' : 'Destination and container port are managed by the configuration file.'} />
          : <FormControl size="small"><InputLabel>Destination</InputLabel><Select label="Destination" value={form.target} onChange={e => { const v = e.target.value; const item = targets.find(t => t.key === v); const p = item?.c.ports.find(x => x.protocol === (form.protocol === 'udp' ? 'udp' : 'tcp'))?.privatePort; setForm({ ...form, target: v, port: p ? String(p) : '' }) }}>{targets.map(t => <MenuItem key={t.key} value={t.key}>{t.label}</MenuItem>)}</Select></FormControl>}
        <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1.5}>
          <FormControl size="small" fullWidth><InputLabel>Protocol</InputLabel><Select label="Protocol" value={formProtocol} onChange={e => {
              const value = e.target.value
              const protocol = value === 'https' ? 'http' : value
              const tls = value === 'https'
              const port = destinationLocked || protocol === form.protocol ? form.port : String(selectedPorts.find(p => p.protocol === (protocol === 'udp' ? 'udp' : 'tcp'))?.privatePort || '')
              const listenPort = tls ? '443' : form.tls || protocol !== form.protocol ? protocol === 'http' ? '80' : Number(port) > 1023 ? port : '' : form.listenPort
              setForm({ ...form, protocol, tls, port, listenPort, redirectHttps: tls })
            }}><MenuItem value="http">HTTP</MenuItem><MenuItem value="https">HTTPS</MenuItem><MenuItem value="tcp">TCP</MenuItem><MenuItem value="udp">UDP</MenuItem></Select></FormControl>
          {destinationLocked
            ? <TextField label="Container port" size="small" fullWidth value={form.port} slotProps={{ input: { readOnly: true } }} />
            : selectedPorts.length ? <FormControl size="small" fullWidth><InputLabel>Container port</InputLabel><Select label="Container port" value={form.port} onChange={e => setForm({ ...form, port: e.target.value })}>{selectedPorts.filter(p => form.protocol === 'udp' ? p.protocol === 'udp' : p.protocol === 'tcp').map(p => <MenuItem key={`${p.privatePort}/${p.protocol}`} value={String(p.privatePort)}>{p.privatePort}/{p.protocol}</MenuItem>)}</Select></FormControl> : <TextField size="small" fullWidth type="number" label="Container port" value={form.port} onChange={e => setForm({ ...form, port: e.target.value })} />}
        </Stack>
        <TextField size="small" type={form.tls ? 'text' : 'number'} label="Entry port" required={form.protocol !== 'http'} value={form.tls ? '443' : form.listenPort} onChange={e => setForm({ ...form, listenPort: e.target.value })} slotProps={{ input: { readOnly: form.tls, endAdornment: form.tls ? <LockRounded sx={{ fontSize: 18, color: 'text.secondary' }} /> : undefined } }} helperText={form.tls ? 'HTTPS always uses port 443. HTTP on port 80 redirects automatically.' : form.protocol === 'http' ? 'Default: 80. Use another port to change the entry address.' : 'Use an available port above 1023.'} />
        <Box className="route-preview"><Typography variant="caption" color="text.secondary">{editing ? 'Address after saving' : 'Address'}</Typography><Typography fontWeight={650} fontSize={14} sx={{ overflowWrap: 'anywhere' }}>{formAddress}</Typography></Box>
      </Stack></DialogContent>
      <DialogActions sx={{px:3,pb:2.5}}><Button onClick={()=>{setDialog(false);setEditing(null)}} disabled={saving} color="inherit">Cancel</Button><Button variant="contained" onClick={()=>void saveRoute()} disabled={saving || !form.id.trim() || !form.port || (form.protocol === 'http' ? !form.hostname.trim() : !form.listenPort)} startIcon={saving ? <CircularProgress size={15} /> : undefined}>{editing ? 'Save changes' : 'Save route'}</Button></DialogActions>
    </Dialog>
    <Snackbar open={!!toast} autoHideDuration={5000} onClose={()=>setToast(null)} anchorOrigin={{vertical:'bottom',horizontal:'right'}}><Alert severity={toast?.severity||'info'} onClose={()=>setToast(null)} variant="filled">{toast?.message}</Alert></Snackbar>
  </Box>
}

function StatCard({icon,label,value,detail,tone,compact=false}:{icon:React.ReactNode;label:string;value:string;detail:string;tone:string;compact?:boolean}){
  return <Paper className="stat-card"><Avatar className={`stat-icon ${tone}`}>{icon}</Avatar><Box sx={{minWidth:0}}><Typography variant="caption" color="text.secondary" fontWeight={550}>{label}</Typography><Typography className="stat-value" fontSize={compact?18:25} fontWeight={720}>{value}</Typography><Typography variant="caption" color="text.secondary" className="stat-detail">{detail}</Typography></Box></Paper>
}

function RouteRow({ item, busy, onEdit, onToggle, onRemove }: { item: Routed; busy: boolean; onEdit: () => void; onToggle: (enabled: boolean) => void; onRemove: () => void }) {
  const r = item.route
  const address = routeAddress(r)
  const canOpen = r.protocol === 'http' && !!r.hostname && !r.hostname.startsWith('*.') && r.enabled
  const source = r.owner === 'auto' ? 'Automatic discovery' : r.owner === 'ui' ? 'Created in dashboard' : r.owner || 'Configuration'
  return <TableRow hover>
    <TableCell><Stack direction="row" alignItems="center" spacing={1}><Avatar className="table-icon"><LanRounded fontSize="small" /></Avatar><Box>
      {canOpen
        ? <Tooltip title="Open in a new tab"><Link href={address} target="_blank" rel="noreferrer" underline="hover" className="route-address" aria-label={`Open ${address}`}><span>{address}</span><OpenInNewRounded sx={{ fontSize: 15 }} /></Link></Tooltip>
        : <Typography fontWeight={650} fontSize={13.5} sx={{ overflowWrap: 'anywhere' }}>{address}</Typography>}
    </Box></Stack></TableCell>
    <TableCell><Typography fontSize={13}>{routeDestination(r)}</Typography><Typography variant="caption" color="text.secondary">Container port {r.port}</Typography></TableCell>
    <TableCell><Tooltip title={source}><Chip size="small" label={r.owner === 'auto' ? 'Automatic' : r.owner === 'ui' ? 'Dashboard' : r.owner?.split('/').pop() || 'Configuration'} className={r.owner === 'auto' ? 'auto-chip' : 'protocol-chip'} /></Tooltip></TableCell>
    <TableCell>{r.protocol === 'http' && r.tls ? <Chip icon={<LockRounded />} size="small" label="HTTPS" className="tls-chip" /> : <Chip size="small" label={r.protocol.toUpperCase()} className="protocol-chip" />}</TableCell>
    <TableCell><Tooltip title={r.message || ''}><Stack direction="row" alignItems="center" spacing={.75}>{dot(r.status === 'active' ? '#10b981' : r.status === 'disabled' ? '#98a2b3' : '#d97706')}<Typography fontSize={12.5} color={r.status === 'active' ? 'success.main' : 'text.secondary'}>{r.status === 'active' ? 'Active' : r.status === 'disabled' ? 'Disabled' : 'Pending'}</Typography></Stack></Tooltip></TableCell>
    <TableCell align="right"><Switch size="small" checked={r.enabled} disabled={busy} onChange={e => onToggle(e.target.checked)} slotProps={{ input: { 'aria-label': `${r.enabled ? 'Disable' : 'Enable'} ${r.hostname || r.id}` } }} /></TableCell>
    <TableCell align="right"><Stack direction="row" justifyContent="flex-end" spacing={.25}>
      <Tooltip title="Edit route"><span><IconButton size="small" disabled={busy} aria-label={`Edit ${r.hostname || r.id}`} onClick={onEdit}><EditRounded fontSize="small" /></IconButton></span></Tooltip>
      {r.owner === 'ui' && <Tooltip title="Delete route"><span><IconButton size="small" disabled={busy} aria-label={`Delete ${r.hostname || r.id}`} onClick={onRemove}><DeleteOutlineRounded fontSize="small" /></IconButton></span></Tooltip>}
    </Stack></TableCell>
  </TableRow>
}

function ContainerCard({container:c}:{container:ContainerInfo}){
  return <Paper className="service-card"><Stack direction="row" alignItems="flex-start" spacing={1.5}><Avatar className="service-avatar"><DnsRounded /></Avatar><Box minWidth={0} flex={1}><Stack direction="row" alignItems="center" spacing={.7}><Typography fontWeight={650} noWrap>{c.service||c.name}</Typography>{dot(c.state==='running'?'#10b981':'#98a2b3')}</Stack><Typography variant="caption" color="text.secondary" noWrap display="block">{c.project?`${c.project} · `:''}{c.image}</Typography></Box><Tooltip title={c.hasTraefikRules?'Routes declared by Traefik labels':'Automatic Dock routes'}><Chip size="small" label={c.hasTraefikRules?'Labels':'Auto'} className={c.hasTraefikRules?'label-chip':'auto-chip'} /></Tooltip></Stack><Divider sx={{my:1.5}}/><Stack direction="row" justifyContent="space-between" alignItems="center"><Stack direction="row" spacing={.6} alignItems="center" color="text.secondary"><LanRounded sx={{fontSize:15}}/><Typography variant="caption">{c.ports.length?c.ports.map(p=>`${p.privatePort}/${p.protocol}`).join(', '):'No ports declared'}</Typography></Stack><Typography variant="caption" color="text.secondary">{c.networks.length} network{c.networks.length===1?'':'s'}</Typography></Stack>{c.discoveryError&&<Alert severity="warning" sx={{mt:1}}>{c.discoveryError}</Alert>}</Paper>
}

function EmptyState({icon,title,detail,action}:{icon:React.ReactNode;title:string;detail:string;action?:React.ReactNode}){
  return <Stack alignItems="center" textAlign="center" spacing={1} py={7} className="empty-state"><Avatar>{icon}</Avatar><Typography fontWeight={650}>{title}</Typography><Typography variant="body2" color="text.secondary" maxWidth={360}>{detail}</Typography>{action}</Stack>
}
function SettingLine({label,value}:{label:string;value:string}){return <Stack direction="row" justifyContent="space-between" py={.65}><Typography variant="body2" color="text.secondary">{label}</Typography><Typography variant="body2" fontWeight={550}>{value}</Typography></Stack>}

export default App
