<script lang="ts">
  import { api, errorMessage, type Run, type RunQuery } from './api'
  import { href, go } from './router.svelte'
  import { fmtTime, relative, duration } from './format'
  import { clock, poll } from './live.svelte'
  import { rowClick } from './rows'
  import StatusBadge from './StatusBadge.svelte'
  import ResultView from './ResultView.svelte'

  let {
    jobId = '',
    status = '',
    query,
    showJob = true,
  }: { jobId?: string; status?: string; query?: RunQuery; showJob?: boolean } = $props()

  // The filter: a full query (Runs page), or a job and status (job page).
  const filter = $derived<RunQuery>(query ?? { job_id: jobId, status })
  const filterKey = $derived(JSON.stringify(filter))

  const pageSize = 50
  const maxShown = 500

  // The poll asks for as many of the newest runs as are shown, so a run
  // never drops out between the live first part and the older pages, and
  // runs still going keep updating wherever they are in the list.
  let runs = $state<Run[] | null>(null)
  let want = $state(pageSize)
  let hasOlder = $state(false)
  let loadingOlder = $state(false)
  let error = $state('')

  // gen changes with the filter; replies for an older filter are dropped.
  let gen = 0

  $effect(() => {
    void filterKey
    const f = $state.snapshot(filter) as RunQuery
    const g = ++gen
    runs = null
    want = pageSize
    hasOlder = false
    error = ''

    return poll(async (signal) => {
      try {
        const limit = Math.min(want, maxShown)
        const r = await api.runs({ ...f, limit }, signal)
        if (g !== gen || signal.aborted) return
        // Pages loaded past maxShown are not polled; they are kept as they are.
        const ids = new Set(r.map((x) => x.id))
        const kept = want > maxShown && runs ? runs.slice(maxShown).filter((x) => !ids.has(x.id)) : []
        runs = [...r, ...kept]
        if (want === pageSize) hasOlder = r.length === pageSize
        error = ''
      } catch (e) {
        if (g === gen && !signal.aborted) error = errorMessage(e)
      }
    }, 3000)
  })

  async function loadOlder() {
    const last = runs?.[runs.length - 1]
    if (!last) return
    const g = gen
    loadingOlder = true
    try {
      const r = await api.runs({ ...($state.snapshot(filter) as RunQuery), limit: pageSize, before: last.id })
      if (g !== gen || !runs) return
      const ids = new Set(runs.map((x) => x.id))
      runs = [...runs, ...r.filter((x) => !ids.has(x.id))]
      want = runs.length
      hasOlder = r.length === pageSize
    } catch (e) {
      if (g === gen) error = errorMessage(e)
    } finally {
      if (g === gen) loadingOlder = false
    }
  }

  const triggerText: Record<Run['trigger'], string> = { schedule: 'schedule', manual: 'manual', retry: 'retry', webhook: 'webhook', after: 'after', ping: 'reported' }
  const cols = $derived(showJob ? 8 : 7)
</script>

{#if error && runs}<div class="alert mb-3">{error}</div>{/if}

<div class="panel table-wrap">
  <table>
    <thead>
      <tr>
        <th>Status</th>
        {#if showJob}<th>Job</th>{/if}
        <th>Trigger</th>
        <th>Scheduled</th>
        <th>Started</th>
        <th>Duration</th>
        <th>Replica</th>
        <th>Result</th>
      </tr>
    </thead>
    <tbody>
      {#if runs === null}
        <tr><td colspan={cols} class="empty">{#if error}<span class="text-danger">Could not load runs: {error}</span>{:else}Loading…{/if}</td></tr>
      {:else if runs.length === 0}
        <tr>
          <td colspan={cols} class="empty">
            {filter.q || filter.mine || filter.from || filter.to || filter.status ? 'No run matches.' : 'No runs yet.'}
          </td>
        </tr>
      {:else}
        {#each runs as r (r.id)}
          <tr class="link" onclick={rowClick(() => go('run', r.id))}>
            <td class="shrink"><a href={href('run', r.id)} aria-label="Open run {r.id}"><StatusBadge status={r.status} /></a></td>
            {#if showJob}
              <td class="max-w-56 truncate"><a href={href('job', r.job_id)}>{r.job_name}</a></td>
            {/if}
            <td class="small" title={r.triggered_by ? `by ${r.triggered_by}` : undefined}>
              {triggerText[r.trigger]}{#if r.attempt > 1}<span class="faint"> · attempt {r.attempt}</span>{/if}
              {#if r.triggered_by}<span class="faint"> · {r.triggered_by}</span>{/if}
            </td>
            <td class="small mono">{fmtTime(r.scheduled_at)}</td>
            <td class="small" title={fmtTime(r.started_at)}>{r.started_at ? relative(r.started_at, clock.now) : '—'}</td>
            <td class="small">{r.started_at ? duration(r.started_at, r.finished_at, clock.now) : '—'}</td>
            <td class="small faint max-w-48 truncate" title={r.node}>{r.node || '—'}</td>
            <td class="grow small">
              {#if r.error}
                <span class={r.status === 'skipped' || r.status === 'canceled' ? 'faint' : 'text-danger'} title={r.error}>{r.error}</span>
              {:else if r.result}
                <ResultView value={r.result} compact />
              {:else if r.result_code !== null}
                <span class="faint mono">{r.result_code}</span>
              {/if}
            </td>
          </tr>
        {/each}
      {/if}
    </tbody>
  </table>
</div>

{#if hasOlder}
  <div class="mt-3 flex items-center gap-3">
    <button disabled={loadingOlder} onclick={loadOlder}>{loadingOlder ? 'Loading…' : 'Load older runs'}</button>
    <span class="small faint">{runs?.length} shown</span>
  </div>
{/if}
