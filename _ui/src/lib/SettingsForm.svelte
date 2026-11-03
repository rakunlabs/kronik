<script lang="ts">
  import { AlertTriangle, RotateCcw, Plug, Check, X, Trash2 } from '@lucide/svelte'
  import { api, errorMessage, type SettingEntry, type SettingsPage } from './api'
  import { app } from './app.svelte'
  import { fmtTime, relative, bytes } from './format'
  import { clock } from './live.svelte'
  import { toast } from './toast.svelte'
  import { confirm } from './confirm.svelte'
  import ListEditor from './ListEditor.svelte'

  // Every setting that can be changed while kronik runs, by group. Changes
  // are kept as a draft until saved; then they reach every replica within a
  // few seconds.
  let page = $state<SettingsPage | null>(null)
  let error = $state('')
  let saving = $state(false)
  let draft = $state<Record<string, unknown>>({})
  let resets = $state<string[]>([])
  const replicaGroup = 'replica'

  const navItem = (on: boolean) =>
    'justify-between! border-0! px-2.5! py-1.5! font-medium! shrink-0 ' +
    (on
      ? 'bg-surface! text-ink! shadow-[inset_2px_0_0_var(--text),0_0_0_1px_var(--border-strong)]'
      : 'bg-transparent! text-ink-2! hover:bg-surface-2! hover:text-ink!')
  let group = $state(new URLSearchParams(location.hash.split('?')[1] ?? '').get('group') ?? 'general')

  $effect(() => {
    const h = location.hash.split('?')[0]
    history.replaceState(history.state, '', group === 'general' ? h : `${h}?group=${group}`)
  })

  async function load() {
    try {
      page = await api.settings()
      error = ''
    } catch (e) {
      error = errorMessage(e)
    }
  }

  load()

  const entries = $derived(page?.entries ?? [])
  const inGroup = $derived(entries.filter((e) => e.group === group))
  const dirtyKeys = $derived([...Object.keys(draft), ...resets])
  const dirtyGroups = $derived(new Set(entries.filter((e) => dirtyKeys.includes(e.key)).map((e) => e.group)))

  const current = (e: SettingEntry) => (e.key in draft ? draft[e.key] : resets.includes(e.key) ? e.base : e.value)
  const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b)

  function set(e: SettingEntry, v: unknown) {
    resets = resets.filter((k) => k !== e.key)
    if (same(v, e.value) && !e.overridden) delete draft[e.key]
    else if (same(v, e.value)) delete draft[e.key]
    else draft[e.key] = v
  }

  function reset(e: SettingEntry) {
    delete draft[e.key]
    if (e.overridden && !resets.includes(e.key)) resets = [...resets, e.key]
  }

  function discard() {
    draft = {}
    resets = []
  }

  // Turning on an executor that reaches the host or cluster asks first.
  async function save() {
    const risky = entries.filter((e) => e.danger && e.type === 'bool' && draft[e.key] === true && e.value !== true)
    if (risky.length) {
      const ok = await confirm({
        title: 'Turn on access to the host or cluster?',
        body: `${risky.map((e) => e.label).join(', ')}: anyone who can create jobs here gets this access. Keep kronik behind your proxy's login.`,
        action: 'Turn on',
        danger: true,
      })
      if (!ok) return
    }

    saving = true
    try {
      page = await api.putSettings({ values: $state.snapshot(draft), reset: resets })
      const n = dirtyKeys.length
      discard()
      toast(`${n} setting${n === 1 ? '' : 's'} saved; every replica applies them within a few seconds`)
      // The sidebar's environment, MCP and executors follow.
      api.info().then((i) => (app.info = i)).catch(() => {})
    } catch (e) {
      toast(errorMessage(e), { tone: 'danger' })
    } finally {
      saving = false
    }
  }

  // Duration fields take 30d, 720h, 10m; numbers stay numbers.
  const asText = (v: unknown) => (v === null || v === undefined ? '' : String(v))

  let checks = $state<Record<string, { ok: boolean; message: string } | 'busy'>>({})
  async function check(executor: string) {
    checks[executor] = 'busy'
    try {
      checks[executor] = await api.checkExecutor(executor)
    } catch (e) {
      checks[executor] = { ok: false, message: errorMessage(e) }
    }
  }

  const checkable: Record<string, string> = { docker: 'docker', swarm: 'swarm', kubernetes: 'kubernetes' }
  const executorOn = (g: string) => entries.find((e) => e.key === `executor.${g}`)?.value === true

  let cleaning = $state(false)
  async function cleanup() {
    const r = app.info?.retention
    const ok = await confirm({
      title: 'Clean up now?',
      body: `Deletes finished runs older than ${r?.runs ?? 'the set time'} and audit entries older than ${r?.audit ?? 'the set time'}, as the daily cleanup does.`,
      action: 'Clean up',
      danger: true,
    })
    if (!ok) return
    cleaning = true
    try {
      const res = await api.cleanup()
      toast(`Deleted ${res.runs} runs and ${res.audit} audit entries`)
    } catch (e) {
      toast(errorMessage(e), { tone: 'danger' })
    } finally {
      cleaning = false
    }
  }

  // Leaving with unsaved settings asks first.
  $effect(() => {
    const onUnload = (e: BeforeUnloadEvent) => {
      if (dirtyKeys.length) e.preventDefault()
    }
    window.addEventListener('beforeunload', onUnload)
    return () => window.removeEventListener('beforeunload', onUnload)
  })
</script>

{#if error && !page}
  <div class="alert">Could not load the settings: {error}</div>
{:else if !page}
  <p class="faint">Loading…</p>
{:else}
  {#if !page.editable}
    <div class="note mb-4">These are the configuration's values; this server cannot change them.</div>
  {/if}

  <div class="grid items-start gap-4 md:grid-cols-[200px_minmax(0,1fr)]">
    <nav
      class="flex gap-0.5 overflow-x-auto md:sticky md:top-4 md:grid md:overflow-visible"
      aria-label="Setting groups"
    >
      {#each page.groups as g}
        <button type="button" class={navItem(group === g.id)} onclick={() => (group = g.id)}>
          {g.label}
          {#if dirtyGroups.has(g.id)}<span class="dot" style:color="var(--warn)" title="Unsaved changes"></span>{/if}
        </button>
      {/each}
      <hr class="mx-1 my-1.5 hidden border-0 border-t border-line md:block" />
      <button type="button" class={navItem(group === replicaGroup)} onclick={() => (group = replicaGroup)}>
        This replica <span class="badge text-[10px]!">read-only</span>
      </button>
    </nav>

    <section class="panel p-5 min-w-0">
      {#if group === replicaGroup}
        <h2>This replica</h2>
        <p class="muted text-[13px] mt-0.5 mb-4">
          Set with environment variables on each replica, as they differ per replica or need a restart; they cannot be changed here.
          Values of the replica serving this page{#if app.info}, <code>{app.info.node}</code>{/if}.
        </p>
        {#if app.info}
          {@const s = app.info.scheduler}
          <dl class="kv max-w-4xl">
            <dt>Poll interval</dt><dd>{s.poll_interval} <span class="faint small">· KRONIK_SCHEDULER_POLL_INTERVAL</span></dd>
            <dt>Runs at once</dt><dd>{s.max_concurrent} <span class="faint small">· KRONIK_SCHEDULER_MAX_CONCURRENT</span></dd>
            <dt>Output kept per run</dt><dd>{bytes(s.max_output)}, the end <span class="faint small">· KRONIK_SCHEDULER_MAX_OUTPUT</span></dd>
            <dt>Shutdown wait</dt><dd>{s.shutdown_timeout} <span class="faint small">· KRONIK_SCHEDULER_SHUTDOWN_TIMEOUT</span></dd>
            <dt>Replica timeout</dt><dd>{s.node_timeout} <span class="faint small">· KRONIK_SCHEDULER_NODE_TIMEOUT</span></dd>
            <dt>User header</dt><dd><code>{app.info.user_header}</code> <span class="faint small">· KRONIK_SERVER_USER_HEADER</span></dd>
            <dt>Script secrets</dt>
            <dd>
              {#if app.info.secrets.length}
                {#each app.info.secrets as n}<code class="badge mr-1">{n}</code>{/each}
              {:else}<span class="faint">none</span>{/if}
              <span class="faint small">· KRONIK_SECRET_&lt;NAME&gt;</span>
            </dd>
          </dl>
        {:else}
          <p class="faint">Waiting for the server…</p>
        {/if}
      {/if}
      {#each page.groups.filter((g) => g.id === group) as g}
        <div class="flex flex-wrap items-start justify-between gap-3 mb-4">
          <div>
            <h2>{g.label}</h2>
            <p class="muted text-[13px] mt-0.5">{g.description}</p>
          </div>
          {#if checkable[g.id]}
            <div class="flex items-center gap-2">
              {#if !executorOn(g.id)}
                <span class="badge">turned off</span>
              {/if}
              <button type="button" class="sm" disabled={checks[g.id] === 'busy' || !executorOn(g.id) || dirtyKeys.length > 0} onclick={() => check(g.id)}
                title={dirtyKeys.length ? 'Save first: the check uses the saved settings' : 'Connect with the saved settings'}>
                <Plug size={13} /> {checks[g.id] === 'busy' ? 'Connecting…' : 'Test connection'}
              </button>
            </div>
          {/if}
          {#if g.id === 'retention'}
            <button type="button" class="sm danger" disabled={cleaning || !page.editable} onclick={cleanup}><Trash2 size={13} /> Clean up now</button>
          {/if}
        </div>

        {#if checkable[g.id] && checks[g.id] && checks[g.id] !== 'busy'}
          {@const c = checks[g.id] as { ok: boolean; message: string }}
          <div
            class={c.ok
              ? 'mb-4 flex items-center gap-1.5 rounded-[var(--radius)] border border-ok/35 bg-[var(--ok-soft)] px-3 py-2 text-[13px] text-ok'
              : 'alert mb-4'}
          >
            {#if c.ok}<Check size={14} />{:else}<X size={14} />{/if}
            {c.message}
          </div>
        {/if}

        <div class="grid gap-x-10 gap-y-6 2xl:grid-cols-2">
          {#each inGroup as e (e.key)}
            {@const v = current(e)}
            {@const changed = e.key in draft || resets.includes(e.key)}
            <div class="min-w-0 border-l-2 pl-2.5 {changed ? 'border-warn' : 'border-transparent'}">
              <div class="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
                <label for="s-{e.key}" class="flex! items-center gap-2 text-[13px]! font-semibold text-ink!">
                  {e.label}
                  {#if e.danger}
                    <span class="inline-flex items-center gap-1 text-[11px] font-medium text-warn" title="Gives jobs access to the host or cluster">
                      <AlertTriangle size={11} /> host access
                    </span>
                  {/if}
                </label>
                <span class="small faint">
                  {#if changed}
                    <span class="text-warn">unsaved</span>
                  {:else if e.overridden}
                    changed {e.updated_by ? `by ${e.updated_by}` : ''} <span title={fmtTime(e.updated_at)}>{relative(e.updated_at, clock.now)}</span>
                  {:else}
                    from the configuration
                  {/if}
                </span>
              </div>
              {#if e.description}<p class="hint">{e.description}</p>{/if}

              <div class="flex items-start gap-2 mt-1">
                <div class="min-w-0 flex-1">
                  {#if e.type === 'bool'}
                    <label class="check text-[13px]!">
                      <input id="s-{e.key}" type="checkbox" checked={v === true} disabled={!page.editable} onchange={(ev) => set(e, ev.currentTarget.checked)} />
                      {v === true ? 'On' : 'Off'}
                    </label>
                  {:else if e.type === 'choice'}
                    <select id="s-{e.key}" value={v} disabled={!page.editable} onchange={(ev) => set(e, ev.currentTarget.value)}>
                      {#each e.choices ?? [] as c}<option value={c}>{c}</option>{/each}
                    </select>
                  {:else if e.type === 'int'}
                    <div class="flex items-center gap-2">
                      <input
                        id="s-{e.key}"
                        type="number"
                        class="w-48"
                        min={e.min}
                        max={e.max}
                        value={v}
                        disabled={!page.editable}
                        oninput={(ev) => ev.currentTarget.value !== '' && set(e, Number(ev.currentTarget.value))}
                      />
                      {#if e.unit}<span class="small faint">{e.unit}</span>{/if}
                    </div>
                  {:else if e.type === 'list'}
                    <ListEditor value={(v as string[]) ?? []} onchange={(x) => set(e, x)} placeholder={e.placeholder} label={e.label} disabled={!page.editable} />
                  {:else}
                    <input
                      id="s-{e.key}"
                      class="w-full"
                      class:mono={e.type !== 'string' || e.key.endsWith('host') || e.key.includes('kubeconfig')}
                      value={asText(v)}
                      placeholder={e.placeholder}
                      disabled={!page.editable}
                      spellcheck="false"
                      oninput={(ev) => set(e, ev.currentTarget.value)}
                    />
                  {/if}
                </div>
                {#if page.editable && (e.overridden || changed)}
                  <button type="button" class="ghost icon" title="Back to the configuration's value" aria-label="Reset {e.label}" onclick={() => reset(e)}>
                    <RotateCcw size={13} />
                  </button>
                {/if}
              </div>
              {#if e.overridden && !same(e.base, e.value)}
                <p class="hint">Configuration: <code>{JSON.stringify(e.base)}</code></p>
              {/if}
            </div>
          {/each}
        </div>
      {/each}
    </section>
  </div>

  {#if dirtyKeys.length}
    <div
      class="panel sticky bottom-3 mt-4 flex items-center justify-end gap-2 px-3 py-2.5 shadow-[0_8px_24px_-10px_rgb(0_0_0/0.25)]"
    >
      <span class="mr-auto text-[12px] font-medium text-warn">{dirtyKeys.length} unsaved change{dirtyKeys.length === 1 ? '' : 's'}</span>
      <button type="button" onclick={discard} disabled={saving}>Discard</button>
      <button type="button" class="primary" onclick={save} disabled={saving}>{saving ? 'Saving…' : 'Save'}</button>
    </div>
  {/if}
{/if}

