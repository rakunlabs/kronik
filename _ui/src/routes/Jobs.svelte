<script lang="ts">
  import { Plus, Search, Play, Pause, CalendarCheck, ArrowUp, ArrowDown } from '@lucide/svelte'
  import { api, errorMessage, type Job, type Stats } from '../lib/api'
  import { href, go, replace, router } from '../lib/router.svelte'
  import { relative, nextRun, fmtTime } from '../lib/format'
  import { clock, poll, debounced } from '../lib/live.svelte'
  import { toast } from '../lib/toast.svelte'
  import { rowClick } from '../lib/rows'
  import StatusBadge from '../lib/StatusBadge.svelte'
  import ResultView from '../lib/ResultView.svelte'
  import PageHeader from '../lib/PageHeader.svelte'

  let jobs = $state<Job[] | null>(null)
  let stats = $state<Stats | null>(null)
  let tags = $state<string[]>([])
  let error = $state('')
  let search = $state(router.route.query.get('search') ?? '')
  let tag = $state(router.route.query.get('tag') ?? '')
  let busy = $state<Record<string, boolean>>({})

  // Sorting is done here: the whole list is loaded anyway. The default is
  // by next run, soonest first; "-next" is the reverse.
  type SortKey = 'next' | 'name' | 'last'
  const sortKeys: SortKey[] = ['next', 'name', 'last']
  const parseSort = (v: string | null): { key: SortKey; desc: boolean } => {
    const desc = !!v?.startsWith('-')
    const key = (desc ? v!.slice(1) : v) as SortKey
    return sortKeys.includes(key) ? { key, desc } : { key: 'next', desc: false }
  }
  let sort = $state(parseSort(router.route.query.get('sort')))

  function sortBy(key: SortKey) {
    // A new column starts ascending, except Last run: most recent first.
    sort = sort.key === key ? { key, desc: !sort.desc } : { key, desc: key === 'last' }
  }

  const sortParam = $derived(sort.key === 'next' && !sort.desc ? '' : (sort.desc ? '-' : '') + sort.key)

  // A time to sort by; jobs without one (paused, never ran) go last in
  // either direction.
  const nextTime = (j: Job) => (j.enabled && j.next_run_at ? new Date(j.next_run_at).getTime() : null)
  const lastTime = (j: Job) => {
    const t = j.last_run?.started_at ?? j.last_run?.finished_at
    return t ? new Date(t).getTime() : null
  }

  // report-2 before report-10.
  const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' })

  const sorted = $derived.by(() => {
    if (!jobs) return null
    const { key, desc } = sort
    const dir = desc ? -1 : 1
    const byName = (a: Job, b: Job) => collator.compare(a.name, b.name)
    const byTime = (get: (j: Job) => number | null) => (a: Job, b: Job) => {
      const x = get(a)
      const y = get(b)
      if (x === null && y === null) return byName(a, b)
      if (x === null) return 1
      if (y === null) return -1
      return (x - y) * dir || byName(a, b)
    }
    const cmp = key === 'name' ? (a: Job, b: Job) => byName(a, b) * dir : byTime(key === 'next' ? nextTime : lastTime)
    return [...jobs].sort(cmp)
  })

  // A link to #/jobs (sidebar, a tag badge) sets the filter from the URL;
  // the address this page wrote itself is skipped, so typing is not undone.
  let written = ''
  $effect(() => {
    const q = router.route.query
    if (q.toString() === written) return
    search = q.get('search') ?? ''
    tag = q.get('tag') ?? ''
    sort = parseSort(q.get('sort'))
  })

  const filter = debounced(() => ({ search: search.trim(), tag }), 250)

  // The filter and the sort live in the URL, so a reload or a shared link
  // keeps them.
  $effect(() => {
    const { search, tag } = filter.value
    const q: Record<string, string> = { search, tag, sort: sortParam }
    written = new URLSearchParams(Object.entries(q).filter(([, v]) => v)).toString()
    replace('jobs', '', q)
  })

  // gen counts loads; a reply older than the last change (filter, pause,
  // resume) is dropped instead of overwriting newer data.
  let gen = 0

  $effect(() => {
    const { search, tag } = filter.value
    return poll(async (signal) => {
      const g = ++gen
      try {
        const [j, s, t] = await Promise.all([api.jobs(search, tag, signal), api.stats(signal), api.tags(signal)])
        if (g !== gen || signal.aborted) return
        jobs = j
        stats = s
        tags = t
        error = ''
      } catch (e) {
        if (g === gen && !signal.aborted) error = errorMessage(e)
      }
    }, 5000)
  })

  async function act(j: Job, fn: () => Promise<void>) {
    busy[j.id] = true
    gen++
    try {
      await fn()
    } catch (e) {
      toast(errorMessage(e), { tone: 'danger' })
    } finally {
      busy[j.id] = false
    }
  }

  const toggle = (j: Job) =>
    act(j, async () => {
      const updated = await api.setEnabled(j.id, !j.enabled)
      Object.assign(j, updated)
      toast(`${j.name} ${updated.enabled ? 'resumed' : 'paused'}`)
    })

  const trigger = (j: Job) =>
    act(j, async () => {
      const { run_id } = await api.trigger(j.id)
      toast(`${j.name} queued`, { href: href('run', run_id), link: 'View run' })
    })

  const cards = $derived(
    stats
      ? [
          { label: 'Jobs', value: stats.jobs, sub: `${stats.enabled} active, ${stats.jobs - stats.enabled} paused` },
          { label: 'Running', value: stats.running, to: href('runs', '', { status: 'running' }) },
          { label: 'Queued', value: stats.queued, to: href('runs', '', { status: 'queued' }) },
          { label: 'Succeeded · 24h', value: stats.succeeded_24h, to: href('runs', '', { status: 'succeeded' }), sub: 'all runs →' },
          {
            label: 'Failed · 24h',
            value: stats.failed_24h,
            danger: stats.failed_24h > 0,
            to: href('runs', '', { status: 'failed,timed_out,lost' }),
            sub: 'all failures →',
          },
        ]
      : [],
  )

  const filtered = $derived(!!(filter.value.search || filter.value.tag))
</script>

<PageHeader title="Jobs">
  {#snippet actions()}
    <a class="btn primary" href={href('new')}><Plus size={15} /> New job</a>
  {/snippet}
</PageHeader>

<div class="grid grid-cols-2 md:grid-cols-3 xl:grid-cols-5 gap-2.5 mb-5">
  {#if stats}
    {#each cards as c}
      <svelte:element this={c.to ? 'a' : 'div'} href={c.to} class="panel block px-3.5 py-2.5 text-ink {c.to ? 'hover:border-ink-3 hover:no-underline' : ''}">
        <div class="small faint">{c.label}</div>
        <div class="text-[22px] font-semibold tabular-nums leading-tight" class:text-danger={c.danger}>{c.value}</div>
        {#if c.sub}<div class="text-[11.5px] faint">{c.sub}</div>{/if}
      </svelte:element>
    {/each}
  {:else}
    {#each Array(5) as _}<div class="panel block h-[74px] px-3.5 py-2.5" aria-hidden="true"></div>{/each}
  {/if}
</div>

<div class="flex flex-wrap gap-2 mb-3">
  <div class="relative flex-1 min-w-52 max-w-[420px]">
    <Search size={14} class="absolute left-2.5 top-1/2 -translate-y-1/2 faint pointer-events-none" />
    <input type="search" class="w-full pl-8!" aria-label="Search jobs" placeholder="Search name, description or tag" bind:value={search} />
  </div>
  {#if tags.length || tag}
    <select bind:value={tag} aria-label="Tag">
      <option value="">All tags</option>
      {#each tags as t}<option value={t}>{t}</option>{/each}
      {#if tag && !tags.includes(tag)}<option value={tag}>{tag}</option>{/if}
    </select>
  {/if}
</div>

{#if error && jobs}<div class="alert mb-3">{error}</div>{/if}

<div class="panel table-wrap">
  <table>
    <thead>
      <tr>
        {#snippet sortable(key: SortKey, label: string)}
          <th aria-sort={sort.key === key ? (sort.desc ? 'descending' : 'ascending') : 'none'}>
            <button
              class="inline-flex cursor-pointer items-center gap-[3px] rounded-none border-0 bg-transparent p-0 [font:inherit] text-inherit hover:bg-transparent hover:text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus {sort.key === key ? 'text-ink' : ''}" onclick={() => sortBy(key)} title="Sort by {label.toLowerCase()}">
              {label}
              {#if sort.key === key}
                {#if sort.desc}<ArrowDown size={12} />{:else}<ArrowUp size={12} />{/if}
              {/if}
            </button>
          </th>
        {/snippet}
        {@render sortable('name', 'Name')}
        <th>Schedule</th>
        {@render sortable('last', 'Last run')}
        {@render sortable('next', 'Next run')}
        <th><span class="sr-only">Actions</span></th>
      </tr>
    </thead>
    <tbody>
      {#if sorted === null}
        <tr>
          <td colspan="5" class="empty">
            {#if error}<span class="text-danger">Could not load jobs: {error}</span>{:else}Loading…{/if}
          </td>
        </tr>
      {:else if sorted.length === 0}
        <tr>
          <td colspan="5" class="empty">
            {#if filtered}
              No job matches. <button class="ghost sm" onclick={() => ((search = ''), (tag = ''))}>Clear filter</button>
            {:else}
              No jobs yet. <a href={href('new')}>Create the first one</a>
            {/if}
          </td>
        </tr>
      {:else}
        {#each sorted as j (j.id)}
          <tr class="link" onclick={rowClick(() => go('job', j.id))}>
            <td class="grow">
              <div class="flex items-center gap-2 min-w-0">
                <span class="dot" style:color={j.enabled ? 'var(--ok)' : 'var(--border-strong)'} title={j.enabled ? 'Active' : 'Paused'}></span>
                <a href={href('job', j.id)} class="font-medium truncate min-w-16 shrink">{j.name}</a>
                <span class="badge">{j.executor}</span>
                {#each j.tags.slice(0, 3) as t}<span class="badge">{t}</span>{/each}
                {#if j.tags.length > 3}<span class="badge" title={j.tags.slice(3).join(', ')}>+{j.tags.length - 3}</span>{/if}
              </div>
              {#if j.description}<div class="small faint truncate pl-[15px]">{j.description}</div>{/if}
            </td>
            <td>
              <code>{j.schedule}</code>
              {#if j.timezone !== 'UTC'}<div class="text-[11px] faint">{j.timezone}</div>{/if}
            </td>
            <td>
              {#if j.last_run}
                <div class="flex items-center gap-2">
                  <StatusBadge status={j.last_run.status} />
                  <span class="small faint" title={fmtTime(j.last_run.started_at ?? j.last_run.finished_at)}>
                    {relative(j.last_run.started_at ?? j.last_run.finished_at, clock.now)}
                  </span>
                </div>
                {#if j.last_run.result}<div class="small max-w-64"><ResultView value={j.last_run.result} compact /></div>{/if}
              {:else}
                <span class="faint">never</span>
              {/if}
            </td>
            <td class="small">
              {#if !j.enabled}
                <span class="badge">paused</span>
              {:else if j.next_run_at}
                <span title={fmtTime(j.next_run_at)}>{nextRun(j.next_run_at, clock.now)}</span>
              {:else}
                <span class="faint">—</span>
              {/if}
            </td>
            <td class="shrink">
              <div class="flex gap-1 justify-end">
                <button class="ghost sm" disabled={busy[j.id]} aria-label="Run {j.name} now" title="Queue a run now" onclick={() => trigger(j)}>
                  <Play size={13} /> Run
                </button>
                <button
                  class="ghost sm w-[84px]"
                  disabled={busy[j.id]}
                  aria-label="{j.enabled ? 'Pause' : 'Resume'} {j.name}"
                  title={j.enabled ? 'Stop scheduling' : 'Start scheduling'}
                  onclick={() => toggle(j)}
                >
                  {#if j.enabled}<Pause size={13} /> Pause{:else}<CalendarCheck size={13} /> Resume{/if}
                </button>
              </div>
            </td>
          </tr>
        {/each}
      {/if}
    </tbody>
  </table>
</div>

