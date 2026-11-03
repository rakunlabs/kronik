<script lang="ts">
  import { api, errorMessage, type AuditEntry } from './api'
  import { href } from './router.svelte'
  import { fmtTime, relative } from './format'
  import { clock, poll } from './live.svelte'

  let {
    jobId = '',
    user = '',
    limit = 100,
    showJob = true,
  }: { jobId?: string; user?: string; limit?: number; showJob?: boolean } = $props()

  let entries = $state<AuditEntry[] | null>(null)
  let error = $state('')
  let gen = 0

  $effect(() => {
    const filter = { job_id: jobId, user, limit }
    const g = ++gen
    entries = null
    return poll(async (signal) => {
      try {
        const e = await api.audit(filter, signal)
        if (g !== gen || signal.aborted) return
        entries = e
        error = ''
      } catch (e) {
        if (g === gen && !signal.aborted) error = errorMessage(e)
      }
    }, 10000)
  })

  const actionText: Record<string, string> = {
    'job.create': 'created',
    'job.update': 'changed',
    'job.delete': 'deleted',
    'job.pause': 'paused',
    'job.resume': 'resumed',
    'job.trigger': 'ran now',
    'run.cancel': 'canceled a run',
    'script.test': 'test run',
  }

  function detailText(e: AuditEntry): string {
    const d = e.detail ?? {}
    if (Array.isArray(d.changed)) return d.changed.length ? d.changed.join(', ') : 'nothing changed'
    if (typeof d.schedule === 'string') return `${d.schedule} · ${d.executor}`
    if (typeof d.executor === 'string') return String(d.executor)
    return ''
  }

  const cols = $derived(showJob ? 6 : 5)
</script>

<div class="panel table-wrap">
  <table>
    <thead>
      <tr>
        <th>When</th>
        <th>User</th>
        <th>Action</th>
        {#if showJob}<th>Job</th>{/if}
        <th>Via</th>
        <th>Detail</th>
      </tr>
    </thead>
    <tbody>
      {#if entries === null}
        <tr><td colspan={cols} class="empty">{#if error}<span class="text-danger">Could not load: {error}</span>{:else}Loading…{/if}</td></tr>
      {:else if entries.length === 0}
        <tr><td colspan={cols} class="empty">Nothing recorded yet.</td></tr>
      {:else}
        {#each entries as e (e.id)}
          <tr>
            <td class="small" title={fmtTime(e.at)}>{relative(e.at, clock.now)}</td>
            <td class="small">{#if e.actor}<span class="font-medium">{e.actor}</span>{:else}<span class="faint">unknown</span>{/if}</td>
            <td class="small">
              {actionText[e.action] ?? e.action}
              {#if e.run_id && e.run_id !== 'test'} · <a href={href('run', e.run_id)}>run</a>{/if}
            </td>
            {#if showJob}
              <td class="small max-w-56 truncate">
                {#if e.job_id && e.action !== 'job.delete'}<a href={href('job', e.job_id)}>{e.job_name}</a>{:else}{e.job_name}{/if}
              </td>
            {/if}
            <td class="small faint">{e.via}</td>
            <td class="grow small faint" title={detailText(e)}>{detailText(e)}</td>
          </tr>
        {/each}
      {/if}
    </tbody>
  </table>
</div>
