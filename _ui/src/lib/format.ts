import type { RunStatus } from './api'

const pad = (n: number) => String(n).padStart(2, '0')

// localZone is the browser's time zone, the one fmtTime shows times in.
export const localZone = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'

// fmtTime is the time in the browser's zone, "2026-11-02 21:00:00".
export function fmtTime(iso: string | null | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (isNaN(d.getTime())) return iso
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

const zoneFormats = new Map<string, Intl.DateTimeFormat>()

// fmtZoned is the time in the given zone with its abbreviation,
// "2026-11-02 15:00 EST".
export function fmtZoned(iso: string | null | undefined, zone: string): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (isNaN(d.getTime())) return iso
  let f = zoneFormats.get(zone)
  if (!f) {
    try {
      f = new Intl.DateTimeFormat('en-CA', {
        timeZone: zone,
        year: 'numeric',
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
        hourCycle: 'h23',
        timeZoneName: 'short',
      })
    } catch {
      return fmtTime(iso)
    }
    zoneFormats.set(zone, f)
  }
  const p = Object.fromEntries(f.formatToParts(d).map((x) => [x.type, x.value]))
  return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute} ${p.timeZoneName}`
}

// parseBuildDate reads a release date (RFC 3339) or the Makefile's
// "2026-01-02_15:04:05", which is UTC without a zone.
export function parseBuildDate(s: string | undefined): string {
  if (!s || s === '-') return ''
  let v = s.replace('_', 'T')
  if (/T\d\d:\d\d(:\d\d)?$/.test(v)) v += 'Z'
  const t = new Date(v)
  return isNaN(t.getTime()) ? '' : t.toISOString()
}

// relative is "in 5m", "3h ago".
export function relative(iso: string | null | undefined, now = Date.now()): string {
  if (!iso) return '—'
  const t = new Date(iso).getTime()
  if (isNaN(t)) return '—'
  const diff = t - now
  const abs = Math.abs(diff)
  const s = Math.round(abs / 1000)

  let text: string
  if (s < 5) return diff >= 0 ? 'now' : 'just now'
  if (s < 60) text = `${s}s`
  else if (s < 3600) text = `${Math.floor(s / 60)}m`
  else if (s < 86400) text = `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`
  else text = `${Math.floor(s / 86400)}d ${Math.floor((s % 86400) / 3600)}h`

  return diff >= 0 ? `in ${text}` : `${text} ago`
}

// nextRun is relative() for a next run time, "due" once it has passed and
// the scheduler has not picked it up yet.
export function nextRun(iso: string | null | undefined, now = Date.now()): string {
  if (iso && new Date(iso).getTime() <= now) return 'due'
  return relative(iso, now)
}

export function duration(start: string | null, end: string | null, now = Date.now()): string {
  if (!start) return '—'
  const ms = (end ? new Date(end).getTime() : now) - new Date(start).getTime()
  if (ms < 1000) return `${Math.max(ms, 0)}ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`
  const s = Math.floor(ms / 1000)
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`
  return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`
}

export function bytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1 << 20) return `${(n / 1024).toFixed(n % 1024 ? 1 : 0)} KiB`
  return `${(n / (1 << 20)).toFixed(n % (1 << 20) ? 1 : 0)} MiB`
}

export const statusTone: Record<RunStatus, string> = {
  queued: '',
  running: 'info',
  succeeded: 'ok',
  failed: 'danger',
  timed_out: 'danger',
  lost: 'danger',
  canceled: 'warn',
  skipped: '',
}

export const statusLabel: Record<RunStatus, string> = {
  queued: 'queued',
  running: 'running',
  succeeded: 'succeeded',
  failed: 'failed',
  timed_out: 'timed out',
  lost: 'lost',
  canceled: 'canceled',
  skipped: 'skipped',
}

export const allStatuses: RunStatus[] = ['queued', 'running', 'succeeded', 'failed', 'timed_out', 'canceled', 'skipped', 'lost']
