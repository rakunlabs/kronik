<script lang="ts">
  import { untrack } from 'svelte'
  import { Play, KeyRound } from '@lucide/svelte'
  import {
    api,
    errorMessage,
    type Job,
    type JobInput,
    type HTTPConfig,
    type CommandConfig,
    type JSConfig,
    type ContainerConfig,
    type TestRun,
    type Channel,
    type NotifyEvent,
  } from '../lib/api'
  import { app } from '../lib/app.svelte'
  import { router, href, open, setGuard } from '../lib/router.svelte'
  import { toast } from '../lib/toast.svelte'
  import PageHeader from '../lib/PageHeader.svelte'
  import SchedulePreview from '../lib/SchedulePreview.svelte'
  import KeyValues from '../lib/KeyValues.svelte'
  import TestResult from '../lib/TestResult.svelte'
  import ContainerForm from '../lib/ContainerForm.svelte'

  let { id = '' }: { id?: string } = $props()

  const executors = $derived(app.info?.executors ?? ['http', 'js', 'command'])
  const executorLabel: Record<string, string> = {
    http: 'HTTP request',
    js: 'JavaScript',
    command: 'Command',
    docker: 'Docker',
    swarm: 'Swarm',
    kubernetes: 'Kubernetes',
  }
  const containerKinds = ['docker', 'swarm', 'kubernetes'] as const
  type ContainerKind = (typeof containerKinds)[number]
  const isContainer = (e: string): e is ContainerKind => (containerKinds as readonly string[]).includes(e)

  const blankHTTP = (): HTTPConfig => ({
    method: 'GET',
    url: '',
    headers: {},
    body: '',
    expected_status: [],
    insecure_skip_verify: false,
    no_redirect: false,
  })
  const blankCommand = (): CommandConfig => ({ command: '', env: {}, dir: '' })
  const blankJS = (): JSConfig => ({ script: '', env: {} })
  const blankContainer = (): ContainerConfig => ({
    image: '',
    command: [],
    args: [],
    env: {},
    workdir: '',
    user: '',
    cpu: '',
    memory: '',
    pull: '',
    labels: {},
    network: '',
    networks: [],
    mounts: [],
    privileged: false,
    constraints: [],
    namespace: '',
    service_account: '',
    node_selector: {},
    secret_env: {},
  })

  const exampleScript = `// The body of an async function: await, console.log, return a result.
const res = await fetch('https://api.github.com/repos/rakunlabs/kronik', {
  headers: { Accept: 'application/json' },
  // insecure: true,  // skip the TLS certificate check
})
if (!res.ok) throw new Error(\`GitHub answered \${res.status}\`)

const repo = await res.json()
console.log(repo.full_name, 'has', repo.stargazers_count, 'stars')

return { stars: repo.stargazers_count }
`

  let form = $state<JobInput>({
    name: '',
    description: '',
    tags: [],
    schedule: '0 * * * *',
    timezone: Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC',
    enabled: true,
    executor: 'http',
    config: {},
    concurrency: 'forbid',
    catch_up: false,
    timeout_seconds: 0,
    retries: 0,
    retry_delay_seconds: 10,
    jitter_seconds: 0,
    alert_after_seconds: 0,
    notify: { channels: [], on: [] },
    after_jobs: [],
    after_on: 'succeeded',
  })
  // Both configs are kept, so switching the executor back loses nothing.
  let http = $state<HTTPConfig>(blankHTTP())
  let command = $state<CommandConfig>(blankCommand())
  let js = $state<JSConfig>(blankJS())
  // One container config shared by Docker, Swarm and Kubernetes, so moving a
  // job between them keeps its image and command.
  let ctr = $state<ContainerConfig>(blankContainer())
  let tagsText = $state('')
  let statusText = $state('')

  let loaded = $state(false)
  let saving = $state(false)
  let error = $state('')
  let notice = $state('')
  // The version being edited, sent back so a newer change is not overwritten.
  let version = ''

  function fill(j: Job, copy: boolean) {
    const {
      id: _id,
      next_run_at: _n,
      created_at: _c,
      updated_at,
      last_run: _l,
      created_by: _cb,
      updated_by: _ub,
      alerted_at: _a,
      health: _h,
      has_hook: _hh,
      ...rest
    } = j
    // A copy starts paused, so it does not run beside the original at once.
    form = copy ? { ...rest, name: `${j.name}-copy`, enabled: false } : rest
    version = copy ? '' : updated_at
    tagsText = j.tags.join(', ')
    alertMinutes = j.alert_after_seconds ? Math.round(j.alert_after_seconds / 60) : null
    if (j.executor === 'http') {
      http = { ...blankHTTP(), ...j.config }
      statusText = (http.expected_status ?? []).join(', ')
    } else if (j.executor === 'command') {
      command = { ...blankCommand(), ...j.config }
    } else if (j.executor === 'js') {
      js = { ...blankJS(), ...j.config }
    } else if (isContainer(j.executor)) {
      ctr = { ...blankContainer(), ...j.config }
    }
  }

  // The "did not run" alert is set in minutes.
  let alertMinutes = $state<number | null>(null)

  // saved is the form as it was loaded; leaving with changes asks first.
  let saved = $state('')
  const snapshot = () => JSON.stringify([form, http, command, js, ctr, tagsText, statusText, alertMinutes])
  const dirty = $derived(loaded && !saving && saved !== '' && snapshot() !== saved)

  function ready() {
    loaded = true
    saved = snapshot()
  }

  // The page is remounted for another job ({#key} in App), so this runs once.
  untrack(() => {
    const from = router.route.query.get('from') || ''
    const source = id || from
    if (!source) {
      ready()
      return
    }
    api
      .job(source)
      .then((j) => {
        fill(j, !id)
        ready()
      })
      .catch((e) => {
        if (id) {
          error = errorMessage(e)
          return
        }
        // The job to copy is gone: start from a blank form instead.
        notice = `Could not copy the job: ${errorMessage(e)}`
        ready()
      })
  })

  // A new job uses an executor this replica has.
  $effect(() => {
    if (!id && loaded && !executors.includes(form.executor) && executors.length) untrack(() => (form.executor = executors[0]))
  })

  const leave = () => !dirty || window.confirm('Leave this page? Your changes are not saved.')
  $effect(() => {
    setGuard(leave)
    const onUnload = (e: BeforeUnloadEvent) => {
      if (dirty) e.preventDefault()
    }
    window.addEventListener('beforeunload', onUnload)
    return () => {
      setGuard(null)
      window.removeEventListener('beforeunload', onUnload)
    }
  })

  const presets = [
    { label: 'Every minute', value: '* * * * *' },
    { label: 'Every 5 min', value: '*/5 * * * *' },
    { label: 'Hourly', value: '0 * * * *' },
    { label: 'Daily 03:00', value: '0 3 * * *' },
    { label: 'Weekdays 09:00', value: '0 9 * * 1-5' },
    { label: 'Weekly', value: '0 0 * * 1' },
    { label: 'Monthly', value: '0 0 1 * *' },
  ]

  const zones: string[] = (() => {
    try {
      return (Intl as any).supportedValuesOf('timeZone') as string[]
    } catch {
      return []
    }
  })()

  // Only the fields of the chosen runtime are sent; the server refuses the
  // others.
  function containerConfig(kind: ContainerKind): Partial<ContainerConfig> {
    const c = $state.snapshot(ctr) as ContainerConfig
    const common: Partial<ContainerConfig> = {
      image: c.image.trim(),
      command: c.command,
      args: c.args,
      env: c.env,
      workdir: c.workdir,
      user: c.user,
      cpu: c.cpu,
      memory: c.memory,
      pull: c.pull,
      labels: c.labels,
    }
    if (kind === 'docker') return { ...common, network: c.network, mounts: c.mounts.filter((m) => m.source || m.target), privileged: c.privileged }
    if (kind === 'swarm') return { ...common, networks: c.networks, constraints: c.constraints, mounts: c.mounts.filter((m) => m.source || m.target) }
    return { ...common, namespace: c.namespace, service_account: c.service_account, node_selector: c.node_selector, secret_env: c.secret_env }
  }

  // An empty number field binds as null; it falls back to the default.
  const num = (v: unknown, fallback: number) => (v === null || v === undefined || v === '' ? fallback : Number(v))

  function buildBody(): (JobInput & { updated_at?: string }) | string {
    const expected = statusText.split(/[\s,]+/).filter(Boolean).map(Number)
    if (form.executor === 'http' && expected.some((n) => !Number.isInteger(n))) {
      return 'Success statuses must be numbers, e.g. 200, 204'
    }

    let config: unknown = form.config
    if (form.executor === 'http') config = { ...http, body: ['GET', 'HEAD'].includes(http.method) ? '' : http.body, expected_status: expected }
    else if (form.executor === 'command') config = command
    else if (form.executor === 'js') config = js
    else if (isContainer(form.executor)) config = containerConfig(form.executor)

    const { enabled, ...rest } = form

    return {
      ...rest,
      // On edit, pausing and resuming are left to their own buttons, so a
      // pause made while the form was open is not undone by saving.
      ...(id ? {} : { enabled }),
      ...(id && version ? { updated_at: version } : {}),
      name: form.name.trim(),
      tags: tagsText.split(',').map((t) => t.trim()).filter(Boolean),
      timeout_seconds: num(form.timeout_seconds, 0),
      retries: num(form.retries, 0),
      retry_delay_seconds: num(form.retry_delay_seconds, 10),
      jitter_seconds: num(form.jitter_seconds, 0),
      alert_after_seconds: alertMinutes ? Math.round(num(alertMinutes, 0) * 60) : 0,
      config,
    } as JobInput
  }

  async function save(e: SubmitEvent) {
    e.preventDefault()
    error = ''

    const body = buildBody()
    if (typeof body === 'string') {
      error = body
      return
    }

    saving = true
    try {
      const j = id ? await api.updateJob(id, body) : await api.createJob(body)
      toast(id ? 'Job saved' : 'Job created')
      // Back from the job does not return to the form.
      open('job', j.id)
    } catch (err) {
      error = errorMessage(err)
      saving = false
    }
  }

  const cancelHref = $derived(id ? href('job', id) : href('jobs'))

  // Test run: the executor once, as the form is now, without saving.
  let testing = $state(false)
  let tested = $state<TestRun | null>(null)
  let testError = $state('')
  let testCtl: AbortController | null = null

  async function testRun() {
    const body = buildBody()
    if (typeof body === 'string') {
      testError = body
      return
    }
    testCtl?.abort()
    const ctl = new AbortController()
    testCtl = ctl
    testing = true
    testError = ''
    try {
      tested = await api.testJob(body, ctl.signal)
    } catch (e) {
      if (!ctl.signal.aborted) testError = errorMessage(e)
    } finally {
      if (testCtl === ctl) testing = false
    }
  }

  function stopTest() {
    testCtl?.abort()
    testing = false
  }

  $effect(() => () => testCtl?.abort())

  const secretNames = $derived(app.info?.secrets ?? [])

  // The script editor (CodeMirror) is large: it is loaded only for a js job.
  const ScriptEditor = $derived(form.executor === 'js' ? import('../lib/ScriptEditor.svelte').then((m) => m.default) : null)

  // Other jobs, for "run after", and the notification channels.
  let others = $state<{ id: string; name: string }[]>([])
  let channels = $state<Channel[]>([])
  api.jobs().then((js) => (others = js.filter((j) => j.id !== id).map((j) => ({ id: j.id, name: j.name })))).catch(() => {})
  api.channels().then((c) => (channels = c)).catch(() => {})

  const events: { id: NotifyEvent; label: string; hint: string }[] = [
    { id: 'failure', label: 'Fails', hint: 'when it starts failing, once' },
    { id: 'recovery', label: 'Recovers', hint: 'the first success after failing' },
    { id: 'no_run', label: 'Did not run', hint: 'no success within the alert time' },
    { id: 'success', label: 'Every success', hint: 'can be noisy' },
  ]

  function toggleIn<T>(list: T[], v: T): T[] {
    return list.includes(v) ? list.filter((x) => x !== v) : [...list, v]
  }

  let afterPick = $state('')
</script>

<PageHeader title={id ? 'Edit job' : 'New job'} back={cancelHref} backLabel={id ? 'Job' : 'Jobs'} />

{#if !loaded}
  {#if error}<div class="alert">{error}</div>{:else}<p class="faint">Loading…</p>{/if}
{:else}
  {#if notice}<div class="note mb-4 max-w-[1600px]">{notice}</div>{/if}
  {#if !id && router.route.query.get('from') && !notice}
    <div class="note mb-4 max-w-[1600px]">A copy starts paused, so it does not run beside the original. Tick <em>Active</em> to schedule it.</div>
  {/if}
  <form onsubmit={save} class="grid max-w-[1600px] gap-4 2xl:grid-cols-2">
    <section class="panel grid content-start gap-3 p-5">
      <h2 class="panel-title mb-0!">General</h2>
      <div class="grid md:grid-cols-2 gap-3">
        <label>Name <input required maxlength="200" bind:value={form.name} placeholder="nightly-report" /></label>
        <label>Tags <input bind:value={tagsText} placeholder="reports, billing" /></label>
      </div>
      <label>Description <input bind:value={form.description} placeholder="What the job does" /></label>
      {#if id}
        <p class="hint">Pause and resume the schedule from the job page.</p>
      {:else}
        <label class="check"><input type="checkbox" bind:checked={form.enabled} /> Active: runs on its schedule</label>
      {/if}
    </section>

    <section class="panel grid content-start gap-3 p-5">
      <h2 class="panel-title mb-0!">Schedule</h2>
      <div class="flex flex-wrap gap-1.5">
        {#each presets as p}
          <button type="button" class="sm" class:primary={form.schedule === p.value} onclick={() => (form.schedule = p.value)}>{p.label}</button>
        {/each}
      </div>
      <div class="grid gap-4 md:grid-cols-2 2xl:grid-cols-1 min-[1900px]:grid-cols-2">
        <div class="grid gap-3 content-start">
          <label>
            Cron expression
            <input class="mono" required bind:value={form.schedule} placeholder="*/5 * * * *" spellcheck="false" />
            <span class="hint">
              <code>minute hour day month weekday</code>, or <code>@hourly</code>, <code>@daily</code>, <code>@every 10m</code>. Day and weekday
              must both match.
            </span>
          </label>
          <label>
            Time zone
            <input list="zones" bind:value={form.timezone} placeholder="UTC" spellcheck="false" />
            <datalist id="zones">{#each zones as z}<option value={z}></option>{/each}</datalist>
          </label>
        </div>
        <div class="rounded-box border border-line bg-surface-2 px-3.5 py-3">
          <SchedulePreview schedule={form.schedule} timezone={form.timezone} />
        </div>
      </div>
    </section>

    <section class="panel grid gap-3 p-5 2xl:col-span-2">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <h2 class="panel-title mb-0!">What to run</h2>
        <div class="seg" role="group" aria-label="Executor">
          {#each executors as ex}
            <button type="button" aria-pressed={form.executor === ex} class:on={form.executor === ex} onclick={() => (form.executor = ex)}>
              {executorLabel[ex] ?? ex}
            </button>
          {/each}
        </div>
      </div>

      {#if form.executor === 'http'}
        <div class="grid grid-cols-[110px_minmax(0,1fr)] gap-2">
          <label>
            Method
            <select bind:value={http.method}>
              {#each ['GET', 'POST', 'PUT', 'PATCH', 'DELETE', 'HEAD'] as m}<option>{m}</option>{/each}
            </select>
          </label>
          <label>URL <input class="mono" required type="url" bind:value={http.url} placeholder="https://example.com/hooks/cleanup" spellcheck="false" /></label>
        </div>
        <div class="grid gap-1">
          <span class="text-[12px] font-medium muted">Headers</span>
          <KeyValues value={http.headers} onchange={(v) => (http.headers = v)} keyLabel="Header" />
        </div>
        {#if !['GET', 'HEAD'].includes(http.method)}
          <label>Body <textarea rows="5" bind:value={http.body} placeholder={'{"key": "value"}'} spellcheck="false"></textarea></label>
        {/if}
        <label>
          Success statuses
          <input class="mono" bind:value={statusText} placeholder="any 2xx" />
          <span class="hint">Comma-separated, e.g. <code>200, 204</code>. Empty accepts any 2xx.</span>
        </label>
        <div class="flex flex-wrap gap-x-5 gap-y-2">
          <label class="check"><input type="checkbox" bind:checked={http.no_redirect} /> Do not follow redirects</label>
          <label class="check"><input type="checkbox" bind:checked={http.insecure_skip_verify} /> Skip TLS certificate check</label>
        </div>
        <p class="hint">
          Each request carries <code>X-Kronik-Job</code>, <code>X-Kronik-Run</code>, <code>X-Kronik-Attempt</code> and <code>X-Kronik-Scheduled-At</code>.
        </p>
      {:else if form.executor === 'js'}
        <div class="grid gap-1">
          <div class="flex flex-wrap items-end justify-between gap-2">
            <span class="text-[12px] font-medium muted">Script</span>
            {#if !js.script.trim()}
              <button type="button" class="ghost sm" onclick={() => (js.script = exampleScript)}>Insert an example</button>
            {/if}
          </div>
          {#await ScriptEditor}
            <div class="term min-h-60 text-term-dim">Loading the editor…</div>
          {:then Editor}
            {#if Editor}
              <Editor
                value={js.script}
                onchange={(v: string) => (js.script = v)}
                onrun={testRun}
                secrets={secretNames}
                env={Object.keys(js.env)}
                placeholder="const res = await fetch('https://…')"
              />
            {/if}
          {:catch e}
            <div class="alert">Could not load the editor: {errorMessage(e)}</div>
          {/await}
          <span class="hint">
            The body of an async function. <code>fetch(url, &#123; method, headers, body, insecure, redirect, timeout &#125;)</code> works as in a browser;
            <code>insecure: true</code> skips the TLS certificate check and an object <code>body</code> is sent as JSON.
            <code>console.log</code> writes the output, <code>return</code> sets the result, <code>throw</code> fails the run.
            Also: <code>sleep(ms)</code>, <code>env</code>, <code>secrets</code>, <code>params</code> (values given to a run), <code>job</code>.
            Ctrl/⌘+Enter tests it.
          </span>
        </div>

        <div class="grid md:grid-cols-2 gap-4">
          <div class="grid gap-1 content-start">
            <span class="text-[12px] font-medium muted">env <span class="faint font-normal">· stored with the job, shown here</span></span>
            <KeyValues value={js.env} onchange={(v) => (js.env = v)} keyLabel="NAME" />
          </div>
          <div class="grid gap-1 content-start">
            <span class="text-[12px] font-medium muted">secrets <span class="faint font-normal">· encrypted, never shown</span></span>
            {#if secretNames.length}
              <div class="flex flex-wrap gap-1.5">
                {#each secretNames as n}<code class="badge"><KeyRound size={11} />secrets.{n}</code>{/each}
              </div>
            {:else}
              <p class="hint">None yet. Add them under <a href={href('settings', 'secrets')}>Settings → Secrets</a>.</p>
            {/if}
          </div>
        </div>
      {:else if isContainer(form.executor)}
        <p class="hint -mt-1">
          {#if form.executor === 'docker'}
            Runs a container on the Docker Engine, streams its output and removes it when it ends. Exit code 0 is success.
          {:else if form.executor === 'swarm'}
            Runs a one-off service (replicated-job) on the Swarm cluster, which places it on any node, then removes it.
          {:else}
            Creates a Kubernetes Job (one pod, no retries of its own), streams the pod's log and deletes the Job when it ends.
          {/if}
        </p>
        <ContainerForm bind:value={ctr} kind={form.executor} />
      {:else if form.executor === 'command'}
        <label>
          Command
          <textarea rows="6" required aria-label="Command" bind:value={command.command} spellcheck="false" placeholder={'pg_dump "$DATABASE_URL" | gzip > /backups/db-$(date +%F).sql.gz'}></textarea>
          <span class="hint">Runs with <code>sh -c</code> in the kronik container. Exit code 0 is success.</span>
        </label>
        <label>Working directory <input class="mono" bind:value={command.dir} placeholder="kronik's directory" spellcheck="false" /></label>
        <div class="grid gap-1">
          <span class="text-[12px] font-medium muted">Environment</span>
          <KeyValues value={command.env} onchange={(v) => (command.env = v)} keyLabel="NAME" />
        </div>
        <p class="hint">
          Also set: <code>KRONIK_JOB_ID</code>, <code>KRONIK_JOB_NAME</code>, <code>KRONIK_RUN_ID</code>, <code>KRONIK_ATTEMPT</code>,
          <code>KRONIK_SCHEDULED_AT</code>.
        </p>
      {:else}
        <p class="alert">This replica cannot run "{form.executor}" jobs. Choose another executor.</p>
      {/if}

      {#if form.executor !== 'js' && executors.includes(form.executor)}
        <p class="hint">
          <code>&#123;&#123;secret.NAME&#125;&#125;</code> and <code>&#123;&#123;param.NAME&#125;&#125;</code> in the URL, headers, body and environment are
          filled in when the job runs; secret values are hidden in the output.
          {#if secretNames.length}Secrets: {#each secretNames as n, i}<code>{n}</code>{i < secretNames.length - 1 ? ', ' : ''}{/each}.{/if}
          {#if form.executor !== 'http'}Params are also in <code>KRONIK_PARAM_&lt;NAME&gt;</code>.{/if}
        </p>
      {/if}

      {#if executors.includes(form.executor)}
        <div class="mt-1 grid gap-2.5 border-t border-dashed border-line-strong pt-3">
          <div class="flex flex-wrap items-center gap-2">
            {#if testing}
              <button type="button" onclick={stopTest}>Stop test</button>
              <span class="small faint">Running on this replica…</span>
            {:else}
              <button type="button" onclick={testRun}><Play size={13} /> Test run</button>
              <span class="small faint">
                Runs it once now, without saving.{#if form.executor !== 'js'}{' '}It does its real work, like a scheduled run.{/if}
              </span>
            {/if}
          </div>
          {#if testError}<div class="alert">{testError}</div>{/if}
          {#if tested && !testing}<TestResult result={tested} />{/if}
        </div>
      {/if}
    </section>

    <section class="panel grid gap-3 p-5 2xl:col-span-2">
      <h2 class="panel-title mb-0!">Behaviour</h2>
      <div class="grid md:grid-cols-2 gap-x-4 gap-y-3">
        <label>
          When the previous run is still active
          <select bind:value={form.concurrency}>
            <option value="forbid">Skip the new run</option>
            <option value="allow">Run both</option>
            <option value="replace">Cancel the active run, start the new one</option>
          </select>
        </label>
        <label>
          Times missed while kronik was down
          <select bind:value={form.catch_up}>
            <option value={false}>Run the latest one only</option>
            <option value={true}>Run each of them (up to 100)</option>
          </select>
        </label>
        <label>
          Timeout (seconds)
          <input type="number" min="0" bind:value={form.timeout_seconds} />
          <span class="hint">0 means no timeout.</span>
        </label>
        <div class="grid grid-cols-2 gap-2 content-start">
          <label>Retries <input type="number" min="0" max="100" bind:value={form.retries} /></label>
          <label>Retry after (s) <input type="number" min="0" bind:value={form.retry_delay_seconds} disabled={!form.retries} /></label>
        </div>
        <label>
          Random delay (seconds)
          <input type="number" min="0" max="3600" bind:value={form.jitter_seconds} />
          <span class="hint">Each scheduled run waits 0 to this many seconds, so jobs due at the same minute do not all start at once.</span>
        </label>
        <div class="grid gap-1 content-start">
          <span class="text-[12px] font-medium muted">Run after other jobs</span>
          <div class="flex flex-wrap items-center gap-1.5">
            {#each form.after_jobs as a}
              <span class="badge">
                {others.find((o) => o.id === a)?.name ?? a}
                <button type="button" class="ghost icon p-0!" aria-label="Remove" onclick={() => (form.after_jobs = form.after_jobs.filter((x) => x !== a))}>×</button>
              </span>
            {/each}
            <select
              bind:value={afterPick}
              aria-label="Add a job to run after"
              onchange={() => {
                if (afterPick && !form.after_jobs.includes(afterPick)) form.after_jobs = [...form.after_jobs, afterPick]
                afterPick = ''
              }}
            >
              <option value="">Add a job…</option>
              {#each others.filter((o) => !form.after_jobs.includes(o.id)) as o}<option value={o.id}>{o.name}</option>{/each}
            </select>
            {#if form.after_jobs.length}
              <select bind:value={form.after_on} aria-label="On">
                <option value="succeeded">when it succeeds</option>
                <option value="failed">when it fails</option>
                <option value="finished">when it ends either way</option>
              </select>
            {/if}
          </div>
          <span class="hint">
            A run of this job is queued when a run of one of these ends; it gets <code>after_job</code>, <code>after_run</code> and
            <code>after_status</code> as params. Its own schedule keeps going too (use a far schedule such as <code>0 0 1 1 *</code> to run only after).
          </span>
        </div>
      </div>
    </section>

    <section class="panel grid gap-3 p-5 2xl:col-span-2">
      <h2 class="panel-title mb-0!">Alerts</h2>
      <div class="grid gap-x-6 gap-y-4 md:grid-cols-2">
        <label>
          Alert when it did not run for (minutes)
          <input type="number" min="1" bind:value={alertMinutes} placeholder="off" />
          <span class="hint">
            No successful run for this long marks the job failing and sends the "did not run" notification, e.g. when every replica was down
            or the schedule was paused. Leave empty for no alert.
          </span>
        </label>
        <div class="grid gap-1.5 content-start">
          <span class="text-[12px] font-medium muted">Notify</span>
          {#if channels.length}
            <div class="flex flex-wrap gap-x-4 gap-y-1.5">
              {#each channels as c}
                <label class="check">
                  <input type="checkbox" checked={form.notify.channels.includes(c.id)} onchange={() => (form.notify.channels = toggleIn(form.notify.channels, c.id))} />
                  {c.name} <span class="faint small">{c.type}</span>
                </label>
              {/each}
            </div>
            <span class="text-[12px] font-medium muted mt-1.5">when the job</span>
            <div class="flex flex-wrap gap-x-4 gap-y-1.5">
              {#each events as e}
                <label class="check" title={e.hint}>
                  <input type="checkbox" checked={form.notify.on.includes(e.id)} onchange={() => (form.notify.on = toggleIn(form.notify.on, e.id))} />
                  {e.label}
                </label>
              {/each}
            </div>
            {#if form.notify.channels.length && !form.notify.on.length}<p class="hint text-warn!">Pick at least one event.</p>{/if}
          {:else}
            <p class="hint">No channels yet. Add Slack, Teams, a webhook or e-mail under <a href={href('settings', 'notifications')}>Settings → Notifications</a>.</p>
          {/if}
        </div>
      </div>
    </section>

    {#if error}<div class="alert 2xl:col-span-2">{error}</div>{/if}

    <!-- Save stays in reach on a long form. -->
    <div class="sticky bottom-0 flex gap-2 bg-linear-to-t from-bg from-70% to-transparent py-3 2xl:col-span-2">
      <button class="primary" type="submit" disabled={saving}>{saving ? 'Saving…' : id ? 'Save changes' : 'Create job'}</button>
      <a class="btn" href={cancelHref}>Cancel</a>
    </div>
  </form>
{/if}

