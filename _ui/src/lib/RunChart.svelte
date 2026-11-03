<script lang="ts">
  import { api, type RunPoint } from './api'
  import { href } from './router.svelte'
  import { fmtTime, statusLabel } from './format'
  import { poll } from './live.svelte'

  // The last runs of a job: a bar per run, as tall as it took, coloured by
  // how it ended. Shows whether a job got slower or started failing.
  let { jobId }: { jobId: string } = $props()

  let points = $state<RunPoint[]>([])
  let loaded = $state(false)

  $effect(() => {
    const id = jobId
    return poll(async (signal) => {
      try {
        points = await api.jobStats(id, 100, signal)
        loaded = true
      } catch {}
    }, 15000)
  })

  const ran = $derived(points.filter((p) => p.duration_ms > 0))
  const maxMs = $derived(Math.max(1, ...ran.map((p) => p.duration_ms)))
  const ok = $derived(points.filter((p) => p.status === 'succeeded').length)
  const ended = $derived(points.filter((p) => !['skipped', 'canceled'].includes(p.status)).length)
  const rate = $derived(ended ? Math.round((ok / ended) * 100) : null)

  function pct(arr: number[], q: number) {
    if (!arr.length) return 0
    const s = [...arr].sort((a, b) => a - b)
    return s[Math.min(s.length - 1, Math.floor(q * s.length))]
  }
  const durations = $derived(ran.map((p) => p.duration_ms))
  const p50 = $derived(pct(durations, 0.5))
  const p95 = $derived(pct(durations, 0.95))

  function ms(v: number) {
    if (v < 1000) return `${v} ms`
    if (v < 60_000) return `${(v / 1000).toFixed(v < 10_000 ? 1 : 0)} s`
    const m = Math.floor(v / 60_000)
    const s = Math.round((v % 60_000) / 1000)
    return s ? `${m}m ${s}s` : `${m}m`
  }

  const color: Record<string, string> = {
    succeeded: 'bg-ok',
    failed: 'bg-danger',
    timed_out: 'bg-danger',
    lost: 'bg-danger',
    canceled: 'bg-ink-3',
    skipped: 'bg-line-strong',
  }

  const H = 72
</script>

<div class="grid gap-3">
  <div class="flex flex-wrap items-baseline gap-x-6 gap-y-1 text-[13px]">
    <h2 class="panel-title mb-0!">Last {points.length || ''} runs</h2>
    {#if rate !== null}
      <span><span class="faint">success</span> <b class={rate < 90 ? 'text-danger' : rate < 100 ? 'text-warn' : 'text-ok'}>{rate}%</b></span>
    {/if}
    {#if durations.length}
      <span><span class="faint">median</span> {ms(p50)}</span>
      <span><span class="faint">p95</span> {ms(p95)}</span>
      <span><span class="faint">longest</span> {ms(maxMs)}</span>
    {/if}
  </div>

  {#if !loaded}
    <div class="h-[72px]"></div>
  {:else if !points.length}
    <p class="faint small h-[72px] grid place-items-center">No finished runs yet.</p>
  {:else}
    <div class="flex h-[72px] items-end gap-[2px] border-b border-line" role="img" aria-label="Duration and result of the last {points.length} runs">
      {#each points as p (p.id)}
        <a
          href={href('run', p.id)}
          class="group relative block min-w-[3px] flex-1 max-w-4 rounded-t-[1px] opacity-85 hover:opacity-100 {color[p.status] ?? 'bg-ink-3'}"
          style:height="{p.duration_ms > 0 ? Math.max(3, Math.round((p.duration_ms / maxMs) * H)) : 3}px"
          title="{fmtTime(p.scheduled_at)} · {statusLabel[p.status] ?? p.status}{p.duration_ms ? ` · ${ms(p.duration_ms)}` : ''}"
        ></a>
      {/each}
    </div>
    <div class="flex justify-between text-[11px] faint">
      <span>{fmtTime(points[0].scheduled_at)}</span>
      <span>{fmtTime(points[points.length - 1].scheduled_at)}</span>
    </div>
  {/if}
</div>
