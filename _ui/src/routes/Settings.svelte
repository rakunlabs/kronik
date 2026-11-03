<script lang="ts">
  import { ExternalLink } from '@lucide/svelte'
  import { api, errorMessage, type Node } from '../lib/api'
  import { app } from '../lib/app.svelte'
  import { href, router, replace } from '../lib/router.svelte'
  import { fmtTime, relative, parseBuildDate } from '../lib/format'
  import { clock, poll } from '../lib/live.svelte'
  import PageHeader from '../lib/PageHeader.svelte'
  import AuditTable from '../lib/AuditTable.svelte'
  import McpSetup from '../lib/McpSetup.svelte'
  import SettingsForm from '../lib/SettingsForm.svelte'
  import SecretsManager from '../lib/SecretsManager.svelte'
  import ChannelsManager from '../lib/ChannelsManager.svelte'

  let { tab = '' }: { tab?: string } = $props()

  const tabs = [
    { id: 'configuration', label: 'Configuration' },
    { id: 'secrets', label: 'Secrets' },
    { id: 'notifications', label: 'Notifications' },
    { id: 'replicas', label: 'Replicas' },
    { id: 'audit', label: 'Audit log' },
    { id: 'mcp', label: 'MCP' },
    { id: 'about', label: 'About' },
  ]

  // Audit log filters live in the address.
  const auditJob = $derived(router.route.query.get('job') ?? '')
  const auditUser = $derived(router.route.query.get('user') ?? '')

  // The MCP address as this page reaches the server.
  const mcpURL = $derived(new URL('mcp', location.href.split('#')[0]).href)
  // One server per environment in the same client: kronik-prod, kronik-sandbox.
  const mcpName = $derived.by(() => {
    const env = (info?.environment?.name ?? '').toLowerCase().replace(/[^a-z0-9_-]+/g, '-').replace(/^-+|-+$/g, '')
    return env ? `kronik-${env}` : 'kronik'
  })
  // The old Scheduler tab is part of Configuration now.
  const current = $derived(tabs.some((t) => t.id === tab) ? tab : 'configuration')

  let nodes = $state<Node[] | null>(null)
  let error = $state('')

  $effect(() => {
    if (current !== 'replicas') return
    return poll(async (signal) => {
      try {
        const n = await api.nodes(signal)
        if (signal.aborted) return
        nodes = n
        error = ''
      } catch (e) {
        if (!signal.aborted) error = errorMessage(e)
      }
    }, 5000)
  })

  const info = $derived(app.info)

  const built = $derived(parseBuildDate(info?.build.date))

  const commitURL = $derived.by(() => {
    const c = info?.build.commit
    if (!info || !c || c === '-' || c.endsWith('-dirty')) return ''
    return `${info.build.repository}/commit/${c}`
  })

  const releaseURL = $derived.by(() => {
    const v = info?.build.version
    if (!info || !v || v === 'v0.0.0' || v.includes('SNAPSHOT')) return ''
    return `${info.build.repository}/releases/tag/${v}`
  })
</script>

<PageHeader title="Settings" />

<div class="tabs mb-5">
  {#each tabs as t}
    <a href={href('settings', t.id)} class:on={current === t.id}>{t.label}</a>
  {/each}
</div>

{#if current === 'configuration'}
  <SettingsForm />
{:else if current === 'secrets'}
  <SecretsManager />
{:else if current === 'notifications'}
  <ChannelsManager />
{:else if current === 'replicas'}
  <p class="muted text-[13px] mb-4 max-w-[70ch]">
    Every replica schedules and runs jobs from the same PostgreSQL database; each run is taken by one of them. A replica not seen for
    {info?.scheduler.node_timeout ?? 'a while'} is dropped and its running runs are marked lost.
  </p>

  {#if error && nodes}<div class="alert mb-3">{error}</div>{/if}

  <div class="panel table-wrap">
    <table>
      <thead>
        <tr><th>Name</th><th>Version</th><th>Started</th><th>Last seen</th><th class="text-right!">Running</th></tr>
      </thead>
      <tbody>
        {#if nodes === null}
          <tr>
            <td colspan="5" class="empty">{#if error}<span class="text-danger">Could not load replicas: {error}</span>{:else}Loading…{/if}</td>
          </tr>
        {:else if nodes.length === 0}
          <tr><td colspan="5" class="empty">No replica is running.</td></tr>
        {:else}
          {#each nodes as n (n.name)}
            <tr>
              <td class="grow">
                <span class="mono">{n.name}</span>
                {#if info?.node === n.name}<span class="badge ml-2">this page</span>{/if}
              </td>
              <td class="mono small">{n.version}</td>
              <td class="small" title={fmtTime(n.started_at)}>{relative(n.started_at, clock.now)}</td>
              <td class="small" title={fmtTime(n.seen_at)}>{relative(n.seen_at, clock.now)}</td>
              <td class="text-right">{n.running}</td>
            </tr>
          {/each}
        {/if}
      </tbody>
    </table>
  </div>
{:else if current === 'audit'}
  <p class="muted text-[13px] mb-4 max-w-[75ch]">
    Every change made from the UI, the API and MCP, with the user the reverse proxy sent in
    <code>{info?.user_header || 'the user header'}</code>.
    {#if info && !info.user}<span class="text-danger">This request carried no user, so its changes are recorded without one.</span>{/if}
  </p>
  <div class="flex flex-wrap gap-2 mb-3">
    <input
      aria-label="User"
      placeholder="Filter by user"
      value={auditUser}
      onchange={(e) => replace('settings', 'audit', { job: auditJob, user: e.currentTarget.value.trim() })}
    />
    {#if auditJob || auditUser}<a class="btn ghost" href={href('settings', 'audit')}>Clear</a>{/if}
  </div>
  <AuditTable jobId={auditJob} user={auditUser} />
{:else if current === 'mcp'}
  {#if info && !info.mcp.enabled}
    <div class="note max-w-4xl">MCP is turned off on this replica. Set <code>KRONIK_MCP_ENABLED=true</code> to turn it on.</div>
  {:else}
    <section class="panel grid max-w-5xl gap-4 p-5">
      <p class="muted text-[13px]">
        AI agents (Claude Code, OpenCode, any MCP client) can list, create, change, run and test jobs.{#if info?.environment?.name}{' '}They are told this is the <strong>{info.environment.name}</strong> environment.{/if} They go through the same checks as the UI,
        and every change is in the audit log with the user your reverse proxy puts in <code>{info?.user_header}</code>.
        {#if info?.mcp.read_only}<strong>This endpoint is read-only.</strong>{/if}
      </p>
      <McpSetup name={mcpName} url={mcpURL} />
      <p class="hint">
        The client must reach kronik through your reverse proxy, which signs it in and sets <code>{info?.user_header}</code>. If the proxy
        wants a token or cookie, add it as a header: <code>--header "Authorization: Bearer …"</code> for Claude Code, <code>--header</code>
        or <code>headers</code> for OpenCode.
      </p>
      <div>
        <span class="text-[12px] font-medium muted">Tools</span>
        <p class="small faint mt-1">
          list_jobs, get_job, list_runs, get_run, list_channels, schedule_preview, audit_log{#if !info?.mcp.read_only}, create_job, update_job, pause_job,
            resume_job, trigger_job, cancel_run, delete_job, test_job{/if} (each prefixed <code>kronik_</code>).
        </p>
      </div>
    </section>
  {/if}
{:else if current === 'about'}
  {#if info}
    <section class="panel max-w-4xl p-5">
      <div class="flex items-center gap-3 mb-5">
        <img src="./favicon.svg" alt="" width="36" height="36" />
        <div>
          <h2 class="text-[17px]!">kronik</h2>
          <p class="muted text-[13px]">Cron jobs with a web UI, on PostgreSQL.</p>
        </div>
      </div>
      <dl class="kv">
        <dt>Version</dt>
        <dd class="mono">
          {#if releaseURL}<a href={releaseURL} target="_blank" rel="noreferrer">{info.build.version}</a>{:else}{info.build.version}{/if}
        </dd>
        <dt>Commit</dt>
        <dd class="mono">
          {#if commitURL}<a href={commitURL} target="_blank" rel="noreferrer">{info.build.commit}</a>{:else}{info.build.commit}{/if}
        </dd>
        <dt>Built</dt>
        <dd>
          {#if built}{fmtTime(built)} <span class="faint">· {relative(built, clock.now)}</span>{:else}<span class="faint">unknown</span>{/if}
        </dd>
        <dt>Go</dt><dd class="mono">{info.build.go_version}</dd>
        <dt>Platform</dt><dd class="mono">{info.build.os}/{info.build.arch}</dd>
        <dt>Replica</dt><dd class="mono">{info.node}</dd>
        {#if info.environment?.name}
          <dt>Environment</dt>
          <dd><span class="badge" style:border-color={info.environment.color} style:color={info.environment.color}>{info.environment.name}</span></dd>
        {/if}
        <dt>You</dt>
        <dd>{#if info.user}{info.user}{:else}<span class="faint">no <code>{info.user_header}</code> header</span>{/if}</dd>
        <dt>Source</dt>
        <dd>
          <a href={info.build.repository} target="_blank" rel="noreferrer" class="inline-flex items-center gap-1">
            {info.build.repository.replace(/^https:\/\//, '')} <ExternalLink size={12} />
          </a>
        </dd>
        <dt>Issues</dt>
        <dd>
          <a href="{info.build.repository}/issues" target="_blank" rel="noreferrer" class="inline-flex items-center gap-1">
            Report a problem <ExternalLink size={12} />
          </a>
        </dd>
        <dt>License</dt><dd>MIT</dd>
      </dl>
    </section>
  {:else}
    <p class="faint">Waiting for the server…</p>
  {/if}
{/if}
