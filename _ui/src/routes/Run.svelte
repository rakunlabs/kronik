<script lang="ts">
  import { Square, Copy, Check, RotateCcw, ArrowDownToLine } from '@lucide/svelte'
  import { api, errorMessage, isNotFound, type Run } from '../lib/api'
  import { href, go } from '../lib/router.svelte'
  import { fmtTime, duration, relative } from '../lib/format'
  import { clock, poll } from '../lib/live.svelte'
  import { toast } from '../lib/toast.svelte'
  import PageHeader from '../lib/PageHeader.svelte'
  import StatusBadge from '../lib/StatusBadge.svelte'
  import ResultView from '../lib/ResultView.svelte'

  let { id }: { id: string } = $props()

  let run = $state<Run | null>(null)
  let error = $state('')
  let notFound = $state(false)
  let follow = $state(true)
  let copied = $state(false)
  let busy = $state(false)
  let outputEl = $state<HTMLPreElement>()

  const live = $derived(!run || run.status === 'queued' || run.status === 'running')
  let gen = 0

  async function load(signal?: AbortSignal) {
    const g = ++gen
    try {
      const r = await api.run(id, signal)
      if (g !== gen || signal?.aborted) return
      run = r
      error = ''
    } catch (e) {
      if (g !== gen || signal?.aborted) return
      if (isNotFound(e)) notFound = true
      else error = errorMessage(e)
    }
  }

  // Poll fast while the run goes on; stop once it has finished.
  $effect(() => {
    if (!live || notFound) return
    return poll(load, 1500)
  })

  // Keep the end of the output in view while following, including the last
  // part that arrives with the finished status.
  $effect(() => {
    void run?.output
    if (follow && outputEl) outputEl.scrollTop = outputEl.scrollHeight
  })

  // Scrolling up stops following; back at the bottom starts it again.
  function onScroll() {
    if (!outputEl || !live) return
    follow = outputEl.scrollHeight - outputEl.scrollTop - outputEl.clientHeight < 24
  }

  async function cancel() {
    busy = true
    try {
      await api.cancelRun(id)
      await load()
    } catch (e) {
      toast(errorMessage(e), { tone: 'danger' })
    } finally {
      busy = false
    }
  }

  async function rerun() {
    if (!run) return
    busy = true
    try {
      const { run_id } = await api.trigger(run.job_id)
      go('run', run_id)
    } catch (e) {
      toast(errorMessage(e), { tone: 'danger' })
    } finally {
      busy = false
    }
  }

  async function copy() {
    try {
      await navigator.clipboard.writeText(run?.output ?? '')
      copied = true
      setTimeout(() => (copied = false), 1200)
    } catch {
      toast('Copying needs a secure (https) page', { tone: 'danger' })
    }
  }

  const triggerText = { schedule: 'Schedule', manual: 'Run now', retry: 'Retry' }
</script>

{#if notFound}
  <PageHeader title="Run not found" back={href('runs')} backLabel="Runs" />
  <p class="muted">This run does not exist anymore. Old runs are deleted after a while.</p>
{:else if !run}
  <PageHeader title="Run" back={href('runs')} backLabel="Runs" />
  {#if error}<div class="alert">{error}</div>{:else}<p class="faint">Loading…</p>{/if}
{:else}
  <PageHeader title={run.job_name} back={href('job', run.job_id)} backLabel={run.job_name}>
    {#snippet badges()}
      <StatusBadge status={run!.status} />
      <span class="mono small faint">{run!.id}</span>
    {/snippet}
    {#snippet actions()}
      {#if live}
        <button class="danger" onclick={cancel} disabled={busy || run!.cancel_requested}>
          <Square size={13} /> {run!.cancel_requested ? 'Canceling…' : 'Cancel run'}
        </button>
      {:else}
        <button disabled={busy} onclick={rerun}><RotateCcw size={14} /> Run job again</button>
      {/if}
    {/snippet}
  </PageHeader>

  {#if error}<div class="alert mb-3">{error}</div>{/if}
  {#if run.error}
    <div class={run.status === 'skipped' || run.status === 'canceled' ? 'note mb-3' : 'alert mb-3'}>{run.error}</div>
  {/if}

  <section class="panel p-4 mb-5">
    <dl class="kv sm:grid-cols-[max-content_minmax(0,1fr)_max-content_minmax(0,1fr)]!">
      <dt>Trigger</dt>
      <dd>
        {triggerText[run.trigger]}{run.attempt > 1 ? ` · attempt ${run.attempt}` : ''}
        {#if run.triggered_by}{' '}<span class="faint">by {run.triggered_by}</span>{/if}
      </dd>
      <dt>Replica</dt><dd class="mono">{run.node || '—'}</dd>
      <dt>Scheduled</dt><dd>{fmtTime(run.scheduled_at)}</dd>
      <dt>Result code</dt><dd class="mono">{run.result_code ?? '—'}</dd>
      <dt>Started</dt><dd>{fmtTime(run.started_at)}</dd>
      <dt>Finished</dt><dd>{fmtTime(run.finished_at)}</dd>
      <dt>Duration</dt><dd>{duration(run.started_at, run.finished_at, clock.now)}</dd>
      {#if run.canceled_by}<dt>Canceled by</dt><dd>{run.canceled_by}</dd>{/if}
      {#if run.status === 'queued'}
        <dt>Starts</dt>
        <dd>{new Date(run.not_before).getTime() > clock.now ? relative(run.not_before, clock.now) : 'when a replica is free'}</dd>
      {/if}
    </dl>
  </section>

  {#if run.result}
    <h2 class="mb-2">Result</h2>
    <div class="mb-5"><ResultView value={run.result} /></div>
  {/if}

  <div class="flex flex-wrap items-center justify-between gap-3 mb-2">
    <h2 class="flex items-center gap-2">
      Output
      {#if run.output_truncated}<span class="badge warn">only the end is kept</span>{/if}
    </h2>
    <div class="flex items-center gap-1">
      {#if live}
        <button class="ghost sm {follow ? 'text-ok' : ''}" aria-pressed={follow} onclick={() => (follow = !follow)}>
          <ArrowDownToLine size={13} /> {follow ? 'Following' : 'Follow'}
        </button>
      {/if}
      <button class="ghost sm" onclick={copy} disabled={!run.output}>
        {#if copied}<Check size={13} /> Copied{:else}<Copy size={13} /> Copy{/if}
      </button>
    </div>
  </div>
  <pre bind:this={outputEl} onscroll={onScroll} class="term min-h-48 max-h-[65vh]" class:empty={!run.output}>{run.output ||
      (run.status === 'queued' ? 'Waiting for a replica…' : live ? 'Waiting for output…' : 'No output.')}</pre>
{/if}

