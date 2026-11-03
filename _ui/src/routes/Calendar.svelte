<script lang="ts">
  import { ChevronLeft, ChevronRight } from '@lucide/svelte'
  import { api, errorMessage, type CalendarPage } from '../lib/api'
  import { router, href, replace } from '../lib/router.svelte'
  import { clock } from '../lib/live.svelte'
  import PageHeader from '../lib/PageHeader.svelte'

  // What runs when: the run times of the active jobs on a day or a week, in
  // the browser's time zone, with maintenance windows and a pause shaded.
  type View = 'day' | 'week'

  const q = router.route.query
  const initialView: View = q.get('view') === 'week' ? 'week' : 'day'
  let view = $state<View>(initialView)
  let tag = $state(q.get('tag') ?? '')
  let start = $state(startOf(q.get('date') ? new Date(q.get('date') + 'T00:00') : new Date(), initialView))

  function startOf(d: Date, v: View) {
    const s = new Date(d.getFullYear(), d.getMonth(), d.getDate())
    if (v === 'week') s.setDate(s.getDate() - ((s.getDay() + 6) % 7))
    return s
  }

  const days = $derived(view === 'week' ? 7 : 1)
  const end = $derived(new Date(start.getFullYear(), start.getMonth(), start.getDate() + days))
  const iso = (d: Date) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`

  let page = $state<CalendarPage | null>(null)
  let tags = $state<string[]>([])
  let error = $state('')
  let loading = $state(false)

  api.tags().then((t) => (tags = t)).catch(() => {})

  $effect(() => {
    replace('calendar', '', { view: view === 'day' ? '' : view, date: iso(start), tag })
  })

  $effect(() => {
    const from = start.toISOString()
    const to = end.toISOString()
    const t = tag
    const ctl = new AbortController()
    loading = true
    api
      .calendar(from, to, t, ctl.signal)
      .then((p) => {
        page = p
        error = ''
      })
      .catch((e) => {
        if (!ctl.signal.aborted) error = errorMessage(e)
      })
      .finally(() => {
        if (!ctl.signal.aborted) loading = false
      })
    return () => ctl.abort()
  })

  function move(n: number) {
    start = new Date(start.getFullYear(), start.getMonth(), start.getDate() + n * days)
  }

  function setView(v: View) {
    view = v
    start = startOf(start, v)
  }

  const hours = Array.from({ length: 24 }, (_, i) => i)
  const dayList = $derived(Array.from({ length: days }, (_, i) => new Date(start.getFullYear(), start.getMonth(), start.getDate() + i)))

  type Item = CalendarPage['items'][number]

  // Runs per day and hour; within an hour, per job, so a job that runs
  // every minute is one entry with a count.
  const grid = $derived.by(() => {
    const m = new Map<string, Map<string, { name: string; id: string; times: Date[]; skipped: number }>>()
    for (const it of page?.items ?? []) {
      const t = new Date(it.at)
      const key = `${iso(t)}|${t.getHours()}`
      let cell = m.get(key)
      if (!cell) m.set(key, (cell = new Map()))
      let e = cell.get(it.job_id)
      if (!e) cell.set(it.job_id, (e = { name: it.job_name, id: it.job_id, times: [], skipped: 0 }))
      e.times.push(t)
      if (it.skipped) e.skipped++
    }
    return m
  })

  const cell = (d: Date, h: number) => [...(grid.get(`${iso(d)}|${h}`)?.values() ?? [])].sort((a, b) => a.times[0].getTime() - b.times[0].getTime())

  // A window covers an hour of a day.
  function shaded(d: Date, h: number) {
    const a = new Date(d.getFullYear(), d.getMonth(), d.getDate(), h).getTime()
    const b = a + 3600e3
    return (page?.windows ?? []).filter((w) => new Date(w.from).getTime() < b && new Date(w.to).getTime() > a)
  }

  const fmtHM = (t: Date) => t.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })
  const fmtDay = (d: Date) => d.toLocaleDateString(undefined, { weekday: 'short', day: 'numeric', month: 'short' })
  const title = $derived(
    view === 'day'
      ? start.toLocaleDateString(undefined, { weekday: 'long', day: 'numeric', month: 'long', year: 'numeric' })
      : `${fmtDay(start)} – ${fmtDay(new Date(end.getTime() - 1))}`,
  )

  const now = $derived(new Date(clock.now))
  const total = $derived(page?.items.length ?? 0)
  const jobsShown = $derived(new Set((page?.items ?? []).map((i: Item) => i.job_id)).size)
</script>

<PageHeader title="Calendar" />

<div class="mb-3 flex flex-wrap items-center gap-2">
  <div class="seg" role="group" aria-label="View">
    <button type="button" class:on={view === 'day'} aria-pressed={view === 'day'} onclick={() => setView('day')}>Day</button>
    <button type="button" class:on={view === 'week'} aria-pressed={view === 'week'} onclick={() => setView('week')}>Week</button>
  </div>
  <div class="flex items-center gap-1">
    <button class="icon" aria-label="Earlier" onclick={() => move(-1)}><ChevronLeft size={15} /></button>
    <button onclick={() => (start = startOf(new Date(), view))}>Today</button>
    <button class="icon" aria-label="Later" onclick={() => move(1)}><ChevronRight size={15} /></button>
  </div>
  <input type="date" aria-label="Go to date" value={iso(start)} onchange={(e) => e.currentTarget.value && (start = startOf(new Date(e.currentTarget.value + 'T00:00'), view))} />
  {#if tags.length || tag}
    <select bind:value={tag} aria-label="Tag">
      <option value="">All tags</option>
      {#each tags as t}<option value={t}>{t}</option>{/each}
    </select>
  {/if}
  <span class="font-semibold text-[14px] ml-1">{title}</span>
  <span class="small faint ml-auto">
    {#if loading}Loading…{:else}{total} runs of {jobsShown} jobs · your time zone{/if}
  </span>
</div>

{#if error}<div class="alert mb-3">{error}</div>{/if}
{#if page?.capped.length}
  <div class="note mb-3">Some jobs run too often to list every time; only their first runs are shown.</div>
{/if}
{#if page?.windows.some((w) => w.pause)}
  <div class="note mb-3">All schedules are paused: every run shown here is skipped until the pause is turned off.</div>
{/if}

<div class="panel table-wrap">
  <table class="table-fixed">
    <thead>
      <tr>
        <th class="w-16"></th>
        {#each dayList as d}
          <th class={iso(d) === iso(now) ? 'text-ink!' : ''}>{view === 'week' ? fmtDay(d) : 'Runs'}</th>
        {/each}
      </tr>
    </thead>
    <tbody>
      {#each hours as h}
        <tr>
          <td class="align-top text-[11.5px] faint tabular-nums">{String(h).padStart(2, '0')}:00</td>
          {#each dayList as d}
            {@const ws = shaded(d, h)}
            {@const entries = cell(d, h)}
            {@const isNow = iso(d) === iso(now) && now.getHours() === h}
            <td
              class="align-top whitespace-normal! py-1.5! {ws.length ? 'bg-warn-soft!' : ''} {isNow ? 'shadow-[inset_2px_0_0_var(--cta-border)]' : ''}"
              title={ws.map((w) => (w.pause ? 'paused' : `maintenance: ${w.expr}`)).join('\n') || undefined}
            >
              <div class="grid gap-0.5 {view === 'week' ? 'max-h-28 overflow-y-auto' : ''}">
                {#each entries.slice(0, view === 'week' ? 6 : 40) as e (e.id)}
                  <a
                    href={href('job', e.id)}
                    class="flex min-w-0 items-baseline gap-1.5 text-[12px] {e.skipped === e.times.length ? 'line-through text-ink-3!' : ''}"
                    title="{e.name}: {e.times.map(fmtHM).join(', ')}{e.skipped ? ` (${e.skipped} skipped)` : ''}"
                  >
                    <span class="tabular-nums faint">:{String(e.times[0].getMinutes()).padStart(2, '0')}</span>
                    <span class="truncate">{e.name}</span>
                    {#if e.times.length > 1}<span class="badge text-[10px]! px-1!">×{e.times.length}</span>{/if}
                  </a>
                {/each}
                {#if entries.length > (view === 'week' ? 6 : 40)}
                  <span class="text-[11px] faint">+{entries.length - (view === 'week' ? 6 : 40)} more</span>
                {/if}
              </div>
            </td>
          {/each}
        </tr>
      {/each}
    </tbody>
  </table>
</div>
<p class="hint mt-2">Shaded hours are in a maintenance window: scheduled runs there are skipped. A struck-out job is skipped there.</p>
