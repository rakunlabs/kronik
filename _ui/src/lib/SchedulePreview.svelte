<script lang="ts">
  import { api, errorMessage, type Preview } from './api'
  import { fmtTime, relative, localZone } from './format'
  import { clock, debounced } from './live.svelte'

  let { schedule, timezone }: { schedule: string; timezone: string } = $props()

  let preview = $state<Preview | null>(null)
  const input = debounced(() => ({ schedule: schedule.trim(), timezone: timezone.trim() }), 250)

  $effect(() => {
    const { schedule, timezone } = input.value
    if (!schedule) {
      preview = null
      return
    }
    const ctl = new AbortController()
    api
      .next(schedule, timezone, 5, ctl.signal)
      .then((p) => (preview = p))
      .catch((e) => {
        if (!ctl.signal.aborted) preview = { times: [], error: errorMessage(e) }
      })
    return () => ctl.abort()
  })
</script>

<div class="small">
  <div class="faint mb-1.5">Next runs <span class="text-[11px]">· shown in {localZone}</span></div>
  {#if !preview}
    <p class="faint">—</p>
  {:else if preview.error}
    <p class="text-danger">{preview.error}</p>
  {:else if preview.times.length === 0}
    <p class="faint">This schedule never runs.</p>
  {:else}
    <ul class="grid gap-1 m-0 p-0 list-none">
      {#each preview.times as t}
        <li class="flex justify-between gap-3">
          <span class="mono">{fmtTime(t)}</span>
          <span class="faint">{relative(t, clock.now)}</span>
        </li>
      {/each}
    </ul>
  {/if}
</div>
