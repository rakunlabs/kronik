<script lang="ts">
  import { Search, User, X } from '@lucide/svelte'
  import { api, type Job, type RunQuery } from '../lib/api'
  import { app } from '../lib/app.svelte'
  import { router, href, replace } from '../lib/router.svelte'
  import { allStatuses, statusLabel } from '../lib/format'
  import { debounced } from '../lib/live.svelte'
  import PageHeader from '../lib/PageHeader.svelte'
  import RunTable from '../lib/RunTable.svelte'

  let jobs = $state<Job[]>([])
  api.jobs().then((j) => (jobs = j)).catch(() => {})

  // Every filter lives in the address, so a search can be shared.
  const q = $derived(router.route.query)
  const jobId = $derived(q.get('job') ?? '')
  const status = $derived(q.get('status') ?? '')
  const mine = $derived(q.get('mine') === 'true')
  const range = $derived(q.get('range') ?? '')
  const fromDate = $derived(q.get('from') ?? '')
  const toDate = $derived(q.get('to') ?? '')

  let text = $state(router.route.query.get('q') ?? '')
  const typed = debounced(() => text.trim(), 300)

  function set(changes: Record<string, string>) {
    const next: Record<string, string> = {
      job: jobId,
      status,
      q: typed.value,
      mine: mine ? 'true' : '',
      range,
      from: fromDate,
      to: toDate,
      ...changes,
    }
    replace('runs', '', next)
  }

  // The search box writes the address after a pause in typing.
  let written = router.route.query.get('q') ?? ''
  $effect(() => {
    const t = typed.value
    if (t === written) return
    written = t
    set({ q: t })
  })

  // A link (sidebar, a job page) sets the box from the address.
  $effect(() => {
    const fromURL = q.get('q') ?? ''
    if (fromURL !== written) {
      written = fromURL
      text = fromURL
    }
  })

  const ranges = [
    { id: '', label: 'Any time' },
    { id: '1h', label: 'Last hour' },
    { id: '24h', label: 'Last 24 hours' },
    { id: '7d', label: 'Last 7 days' },
    { id: '30d', label: 'Last 30 days' },
    { id: 'custom', label: 'Between dates…' },
  ]
  const rangeMs: Record<string, number> = { '1h': 3600e3, '24h': 86400e3, '7d': 7 * 86400e3, '30d': 30 * 86400e3 }

  // Dates are in the browser's zone: "to" includes that whole day.
  const localDay = (d: string, addDays = 0) => {
    const [y, m, day] = d.split('-').map(Number)
    return new Date(y, m - 1, day + addDays).toISOString()
  }

  // A relative range is turned into times when the filter changes, not on
  // every poll, so the list stays still.
  const query = $derived.by<RunQuery>(() => {
    const out: RunQuery = { job_id: jobId, status, q: typed.value, mine: mine ? 'true' : '' }
    if (range in rangeMs) out.from = new Date(Date.now() - rangeMs[range]).toISOString()
    if (range === 'custom') {
      if (fromDate) out.from = localDay(fromDate)
      if (toDate) out.to = localDay(toDate, 1)
    }
    return out
  })

  const statusOptions = [
    { value: '', label: 'All statuses' },
    { value: 'failed,timed_out,lost', label: 'All failures' },
    ...allStatuses.map((s) => ({ value: s, label: statusLabel[s] })),
  ]

  const filtered = $derived(!!(jobId || status || typed.value || mine || range))
  const user = $derived(app.info?.user ?? '')
  const today = new Date().toISOString().slice(0, 10)
  const keep = $derived(app.info?.retention?.runs)
</script>

<PageHeader title="Runs" />

<div class="mb-3 flex flex-wrap items-center gap-2">
  <div class="relative flex-1 min-w-56 max-w-[380px]">
    <Search size={14} class="absolute left-2.5 top-1/2 -translate-y-1/2 faint pointer-events-none" />
    <input type="search" class="w-full pl-8!" aria-label="Search runs" placeholder="Job name, error, who started it, run id" bind:value={text} />
  </div>

  <select value={jobId} aria-label="Job" onchange={(e) => set({ job: e.currentTarget.value })}>
    <option value="">All jobs</option>
    {#each jobs as j}<option value={j.id}>{j.name}</option>{/each}
    {#if jobId && !jobs.some((j) => j.id === jobId)}<option value={jobId}>this job</option>{/if}
  </select>

  <select value={status} aria-label="Status" onchange={(e) => set({ status: e.currentTarget.value })}>
    {#each statusOptions as o}<option value={o.value}>{o.label}</option>{/each}
    {#if status && !statusOptions.some((o) => o.value === status)}<option value={status}>{status}</option>{/if}
  </select>

  <select value={range} aria-label="Time" onchange={(e) => set({ range: e.currentTarget.value, from: '', to: '' })}>
    {#each ranges as r}<option value={r.id}>{r.label}</option>{/each}
  </select>

  {#if range === 'custom'}
    <div class="flex items-center gap-1.5">
      <input type="date" aria-label="From" value={fromDate} max={toDate || today} onchange={(e) => set({ from: e.currentTarget.value })} />
      <span class="faint small">to</span>
      <input type="date" aria-label="To" value={toDate} min={fromDate} max={today} onchange={(e) => set({ to: e.currentTarget.value })} />
    </div>
  {/if}

  <button
    type="button"
    class:primary={mine}
    aria-pressed={mine}
    disabled={!user}
    title={user ? 'Runs of the jobs you created or changed, and the runs you started' : 'No user header from the proxy'}
    onclick={() => set({ mine: mine ? '' : 'true' })}
  >
    <User size={13} /> Mine
  </button>

  {#if filtered}
    <a class="btn ghost" href={href('runs')}><X size={13} /> Clear</a>
  {/if}
</div>

{#if keep && keep !== '0'}
  <p class="hint mb-3">Runs are kept for {keep}; older ones are deleted by the daily cleanup.</p>
{/if}

<RunTable {query} />

