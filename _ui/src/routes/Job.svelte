<script lang="ts">
  import { Play, Pause, CalendarCheck, Pencil, Trash2, Copy, ChevronDown, Webhook, Bell, Link2 } from '@lucide/svelte'
  import { api, errorMessage, isNotFound, type Job, type Channel } from '../lib/api'
  import { href, go } from '../lib/router.svelte'
  import { fmtTime, fmtZoned, nextRun, localZone } from '../lib/format'
  import { clock, poll } from '../lib/live.svelte'
  import { toast } from '../lib/toast.svelte'
  import { confirm } from '../lib/confirm.svelte'
  import PageHeader from '../lib/PageHeader.svelte'
  import SchedulePreview from '../lib/SchedulePreview.svelte'
  import RunTable from '../lib/RunTable.svelte'
  import AuditTable from '../lib/AuditTable.svelte'
  import RunChart from '../lib/RunChart.svelte'
  import JobVersions from '../lib/JobVersions.svelte'
  import KeyValues from '../lib/KeyValues.svelte'
  import { app } from '../lib/app.svelte'

  let { id }: { id: string } = $props()

  let job = $state<Job | null>(null)
  let error = $state('')
  let notFound = $state(false)
  let busy = $state(false)
  let gen = 0

  $effect(() => {
    if (notFound) return
    return poll(async (signal) => {
      const g = ++gen
      try {
        const j = await api.job(id, signal)
        if (g !== gen || signal.aborted) return
        job = j
        error = ''
      } catch (e) {
        if (g !== gen || signal.aborted) return
        if (isNotFound(e)) notFound = true
        else error = errorMessage(e)
      }
    }, 5000)
  })

  async function act(fn: () => Promise<unknown>) {
    busy = true
    gen++
    try {
      await fn()
    } catch (e) {
      toast(errorMessage(e), { tone: 'danger' })
    } finally {
      busy = false
    }
  }

  const trigger = () =>
    act(async () => {
      const { run_id } = await api.trigger(id)
      toast('Run queued', { href: href('run', run_id), link: 'View run' })
    })

  // Run with params: values for this run only.
  let withParams = $state(false)
  let params = $state<Record<string, string>>({})
  const triggerWith = () =>
    act(async () => {
      const { run_id } = await api.trigger(id, $state.snapshot(params))
      withParams = false
      toast('Run queued', { href: href('run', run_id), link: 'View run' })
    })

  // The webhook token is shown once, right after it is made.
  let token = $state('')
  const hookBase = $derived(new URL('hook/', location.href.split('#')[0]).href)

  const newHook = () =>
    act(async () => {
      if (job!.has_hook) {
        const ok = await confirm({
          title: 'Make a new webhook URL?',
          body: 'The current URL stops working at once. Whatever calls it must get the new one.',
          action: 'Make a new one',
          danger: true,
        })
        if (!ok) return
      }
      token = (await api.newHook(id)).token
      job = await api.job(id)
    })

  const removeHook = () =>
    act(async () => {
      const ok = await confirm({ title: 'Remove the webhook?', body: 'Its URL stops working at once.', action: 'Remove', danger: true })
      if (!ok) return
      await api.removeHook(id)
      token = ''
      job = await api.job(id)
    })

  async function copyText(t: string) {
    try {
      await navigator.clipboard.writeText(t)
      toast('Copied')
    } catch {
      toast('Copying needs a secure (https) page', { tone: 'danger' })
    }
  }

  let channels = $state<Channel[]>([])
  let jobNames = $state<Record<string, string>>({})
  api.channels().then((c) => (channels = c)).catch(() => {})
  api.jobs().then((js) => (jobNames = Object.fromEntries(js.map((j) => [j.id, j.name])))).catch(() => {})

  const eventText: Record<string, string> = { failure: 'fails', recovery: 'recovers', no_run: 'did not run', success: 'succeeds' }
  const afterText: Record<string, string> = { succeeded: 'succeeds', failed: 'fails', finished: 'ends' }

  const toggle = () =>
    act(async () => {
      job = await api.setEnabled(id, !job!.enabled)
      toast(job.enabled ? 'Schedule resumed' : 'Schedule paused')
    })

  const remove = () =>
    act(async () => {
      const ok = await confirm({
        title: `Delete ${job!.name}?`,
        body: 'The job and all its runs are deleted. Runs going on now are canceled.',
        action: 'Delete job',
        danger: true,
      })
      if (!ok) return
      await api.deleteJob(id)
      toast(`${job!.name} deleted`)
      go('jobs')
    })

  const concurrencyText = { allow: 'Run both', forbid: 'Skip the new run', replace: 'Cancel the active run' }
</script>

{#if notFound}
  <PageHeader title="Job not found" back={href('jobs')} backLabel="Jobs" />
  <p class="muted">This job does not exist anymore.</p>
{:else if !job}
  <PageHeader title="Job" back={href('jobs')} backLabel="Jobs" />
  {#if error}<div class="alert">{error}</div>{:else}<p class="faint">Loading…</p>{/if}
{:else}
  <PageHeader title={job.name} back={href('jobs')} backLabel="Jobs">
    {#snippet badges()}
      {#if job!.enabled}
        <span class="badge ok"><span class="dot"></span>active</span>
      {:else}
        <span class="badge"><span class="dot"></span>paused</span>
      {/if}
      {#if job!.health === 'failing'}<span class="badge danger">failing</span>{/if}
      {#if job!.alerted_at}<span class="badge warn" title="Since {fmtTime(job!.alerted_at)}">did not run</span>{/if}
      {#each job!.tags as t}<a class="badge" href={href('jobs', '', { tag: t })}>{t}</a>{/each}
    {/snippet}
    {#snippet actions()}
      <div class="inline-flex">
        <button class="primary rounded-r-none!" disabled={busy} onclick={trigger}><Play size={14} /> Run now</button>
        <button
          class="primary rounded-l-none! border-l-0! px-1.5!"
          disabled={busy}
          aria-label="Run with params"
          title="Run with params"
          aria-expanded={withParams}
          onclick={() => (withParams = !withParams)}
        >
          <ChevronDown size={14} />
        </button>
      </div>
      <button disabled={busy} onclick={toggle}>
        {#if job!.enabled}<Pause size={14} /> Pause{:else}<CalendarCheck size={14} /> Resume{/if}
      </button>
      <a class="btn" href={href('edit', id)}><Pencil size={14} /> Edit</a>
      <a class="btn" href={href('new', '', { from: id })} title="New job with these settings"><Copy size={14} /> Duplicate</a>
      <button class="danger" disabled={busy} onclick={remove}><Trash2 size={14} /> Delete</button>
    {/snippet}
    {#snippet subtitle()}
      {#if job!.description}{job!.description}{/if}
    {/snippet}
  </PageHeader>

  {#if error}<div class="alert mb-3">{error}</div>{/if}
  {#if app.info?.paused}
    <div class="note mb-3">All schedules are paused (Settings → Configuration → Scheduling): scheduled runs are skipped.</div>
  {/if}

  {#if withParams}
    <section class="panel p-4 mb-4 grid gap-2.5 max-w-3xl">
      <h2 class="panel-title mb-0!">Run once with params</h2>
      <KeyValues value={params} onchange={(v) => (params = v)} keyLabel="name" />
      <p class="hint">
        <code>params.NAME</code> in scripts, <code>&#123;&#123;param.NAME&#125;&#125;</code> in URLs, headers, bodies and environment,
        <code>KRONIK_PARAM_NAME</code> in commands and containers.
      </p>
      <div class="flex gap-2">
        <button class="primary" disabled={busy} onclick={triggerWith}><Play size={14} /> Run</button>
        <button onclick={() => (withParams = false)}>Close</button>
      </div>
    </section>
  {/if}

  <div class="grid lg:grid-cols-2 xl:grid-cols-[1fr_1fr_280px] gap-3 mb-7">
    <section class="panel p-4">
      <h2 class="panel-title">Schedule</h2>
      <dl class="kv">
        <dt>Expression</dt><dd><code>{job.schedule}</code></dd>
        <dt>Time zone</dt><dd>{job.timezone}</dd>
        <dt>Next run</dt>
        <dd>
          {#if !job.enabled}<span class="faint">paused</span>
          {:else if job.next_run_at}
            {fmtZoned(job.next_run_at, job.timezone)} <span class="faint">· {nextRun(job.next_run_at, clock.now)}</span>
            {#if job.timezone !== localZone}<div class="small faint">{fmtTime(job.next_run_at)} your time</div>{/if}
          {:else}<span class="faint">—</span>{/if}
        </dd>
        <dt>Still running</dt><dd>{concurrencyText[job.concurrency]}</dd>
        <dt>Missed times</dt><dd>{job.catch_up ? 'Run each (up to 100)' : 'Run the latest only'}</dd>
        <dt>Timeout</dt><dd>{job.timeout_seconds ? `${job.timeout_seconds}s` : 'none'}</dd>
        <dt>Retries</dt><dd>{job.retries ? `${job.retries}, ${job.retry_delay_seconds}s apart` : 'none'}</dd>
        {#if job.jitter_seconds}<dt>Random delay</dt><dd>up to {job.jitter_seconds}s</dd>{/if}
        <dt>Created</dt>
        <dd>{fmtTime(job.created_at)}{#if job.created_by}{' '}<span class="faint">by {job.created_by}</span>{/if}</dd>
        {#if job.updated_at !== job.created_at}
          <dt>Changed</dt>
          <dd>{fmtTime(job.updated_at)}{#if job.updated_by}{' '}<span class="faint">by {job.updated_by}</span>{/if}</dd>
        {/if}
      </dl>
    </section>

    <section class="panel p-4 min-w-0">
      <h2 class="panel-title">
        {({ http: 'HTTP request', command: 'Command', js: 'JavaScript', docker: 'Docker container', swarm: 'Swarm service', kubernetes: 'Kubernetes Job' } as Record<string, string>)[job.executor] ?? job.executor}
      </h2>
      {#if job.executor === 'http'}
        <dl class="kv">
          <dt>Request</dt><dd><code>{job.config.method} {job.config.url}</code></dd>
          {#if Object.keys(job.config.headers ?? {}).length}
            <dt>Headers</dt>
            <dd>{#each Object.entries(job.config.headers) as [k, v]}<div><code>{k}: {v}</code></div>{/each}</dd>
          {/if}
          {#if job.config.body}<dt>Body</dt><dd><pre class="term max-h-40">{job.config.body}</pre></dd>{/if}
          <dt>Success</dt><dd>{job.config.expected_status?.length ? job.config.expected_status.join(', ') : 'any 2xx'}</dd>
          {#if job.config.insecure_skip_verify}<dt>TLS</dt><dd>certificate not checked</dd>{/if}
          {#if job.config.no_redirect}<dt>Redirects</dt><dd>not followed</dd>{/if}
        </dl>
      {:else if job.executor === 'js'}
        <pre class="term max-h-80">{job.config.script}</pre>
        {#if Object.keys(job.config.env ?? {}).length}
          <dl class="kv mt-3">
            <dt>env</dt>
            <dd>{#each Object.entries(job.config.env) as [k, v]}<div><code>{k}={v}</code></div>{/each}</dd>
          </dl>
        {/if}
      {:else if ['docker', 'swarm', 'kubernetes'].includes(job.executor)}
        {@const c = job.config}
        <dl class="kv">
          <dt>Image</dt><dd><code>{c.image}</code>{#if c.pull}<span class="faint small"> · pull {c.pull}</span>{/if}</dd>
          {#if c.command?.length || c.args?.length}
            <dt>Runs</dt><dd><code>{[...(c.command ?? []), ...(c.args ?? [])].join(' ')}</code></dd>
          {/if}
          {#if c.cpu || c.memory}<dt>Limits</dt><dd>{[c.cpu && `${c.cpu} CPU`, c.memory].filter(Boolean).join(', ')}</dd>{/if}
          {#if c.namespace}<dt>Namespace</dt><dd><code>{c.namespace}</code></dd>{/if}
          {#if c.service_account}<dt>Service account</dt><dd><code>{c.service_account}</code></dd>{/if}
          {#if c.network}<dt>Network</dt><dd><code>{c.network}</code></dd>{/if}
          {#if c.networks?.length}<dt>Networks</dt><dd><code>{c.networks.join(', ')}</code></dd>{/if}
          {#if c.constraints?.length}<dt>Placement</dt><dd><code>{c.constraints.join(', ')}</code></dd>{/if}
          {#if c.mounts?.length}
            <dt>Mounts</dt>
            <dd>{#each c.mounts as m}<div><code>{m.source} → {m.target}{m.read_only ? ' (ro)' : ''}</code></div>{/each}</dd>
          {/if}
          {#if c.privileged}<dt>Privileged</dt><dd class="text-danger">yes</dd>{/if}
          {#if Object.keys(c.env ?? {}).length}
            <dt>Environment</dt><dd>{#each Object.entries(c.env) as [k, v]}<div><code>{k}={v}</code></div>{/each}</dd>
          {/if}
          {#if Object.keys(c.secret_env ?? {}).length}
            <dt>From Secrets</dt><dd>{#each Object.entries(c.secret_env) as [k, v]}<div><code>{k} ← {v}</code></div>{/each}</dd>
          {/if}
        </dl>
      {:else if job.executor === 'command'}
        <pre class="term max-h-48">{job.config.command}</pre>
        {#if job.config.dir || Object.keys(job.config.env ?? {}).length}
          <dl class="kv mt-3">
            {#if job.config.dir}<dt>Directory</dt><dd><code>{job.config.dir}</code></dd>{/if}
            {#if Object.keys(job.config.env ?? {}).length}
              <dt>Environment</dt>
              <dd>{#each Object.entries(job.config.env) as [k, v]}<div><code>{k}={v}</code></div>{/each}</dd>
            {/if}
          </dl>
        {/if}
      {:else}
        <pre class="term">{JSON.stringify(job.config, null, 2)}</pre>
      {/if}
    </section>

    <section class="panel p-4 lg:col-span-2 xl:col-span-1">
      <SchedulePreview schedule={job.schedule} timezone={job.timezone} />
    </section>
  </div>

  <section class="panel p-4 mb-3">
    <RunChart jobId={id} />
  </section>

  <div class="grid gap-3 mb-7 lg:grid-cols-3">
    <section class="panel p-4 min-w-0">
      <h2 class="panel-title"><Bell size={14} /> Alerts</h2>
      <dl class="kv">
        <dt>Did not run</dt>
        <dd>
          {#if job.alert_after_seconds}after {Math.round(job.alert_after_seconds / 60)} min without a success{:else}<span class="faint">off</span>{/if}
        </dd>
        <dt>Notify</dt>
        <dd>
          {#if job.notify.channels.length && job.notify.on.length}
            {job.notify.channels.map((c) => channels.find((x) => x.id === c)?.name ?? c).join(', ')}
            <span class="faint">when it {job.notify.on.map((e) => eventText[e] ?? e).join(', ')}</span>
          {:else}<span class="faint">nobody</span>{/if}
        </dd>
      </dl>
    </section>

    <section class="panel p-4 min-w-0">
      <h2 class="panel-title"><Link2 size={14} /> Chain</h2>
      {#if job.after_jobs.length}
        <p class="text-[13px]">
          Runs when
          {#each job.after_jobs as a, i}<a href={href('job', a)}>{jobNames[a] ?? a}</a>{i < job.after_jobs.length - 1 ? ', ' : ''}{/each}
          {afterText[job.after_on]}.
        </p>
      {:else}
        <p class="faint text-[13px]">Runs on its schedule only.</p>
      {/if}
    </section>

    <section class="panel p-4 min-w-0">
      <div class="flex items-center justify-between gap-2 mb-3">
        <h2 class="panel-title mb-0!"><Webhook size={14} /> Webhook</h2>
        <div class="flex gap-1">
          <button class="sm" disabled={busy} onclick={newHook}>{job.has_hook ? 'New URL' : 'Make a URL'}</button>
          {#if job.has_hook}<button class="sm danger" disabled={busy} onclick={removeHook}>Remove</button>{/if}
        </div>
      </div>
      {#if token}
        <div class="grid gap-1.5">
          <p class="small text-warn">Copy it now; it is not shown again.</p>
          <div class="flex gap-1.5">
            <input class="mono flex-1 text-[12px]!" readonly value={hookBase + token} onfocus={(e) => e.currentTarget.select()} />
            <button class="sm" onclick={() => copyText(hookBase + token)}><Copy size={12} /></button>
          </div>
          <p class="hint">
            <code>POST</code> it to run the job; query or JSON body values become params. <code>…/report?exit_code=0</code> records a run made
            elsewhere (for the did-not-run alert).
          </p>
        </div>
      {:else if job.has_hook}
        <p class="text-[13px]">Set. Other systems can run this job, or report a run made elsewhere. Make a new URL to see one.</p>
      {:else}
        <p class="faint text-[13px]">Let CI or another system run this job, or report a run of a cron elsewhere, through a URL with a secret token.</p>
      {/if}
    </section>
  </div>

  <div class="flex items-center justify-between mb-2.5">
    <h2>Runs</h2>
    <a class="small" href={href('runs', '', { job: id })}>Open in Runs</a>
  </div>
  <RunTable jobId={id} showJob={false} />

  <div class="grid gap-7 mt-7 xl:grid-cols-2">
    <div class="min-w-0">
      <JobVersions {job} onrestore={(j) => (job = j)} />
    </div>
    <div class="min-w-0">
      <div class="flex items-center justify-between mb-2.5">
        <h2>Activity</h2>
        <a class="small" href={href('settings', 'audit', { job: id })}>Open in the audit log</a>
      </div>
      <AuditTable jobId={id} limit={10} showJob={false} />
    </div>
  </div>
{/if}
