<script lang="ts">
  import type { TestRun } from './api'
  import StatusBadge from './StatusBadge.svelte'
  import ResultView from './ResultView.svelte'

  let { result }: { result: TestRun } = $props()

  const duration = $derived(result.duration_ms < 1000 ? `${result.duration_ms}ms` : `${(result.duration_ms / 1000).toFixed(1)}s`)
</script>

<div class="grid gap-2" aria-live="polite">
  <div class="flex flex-wrap items-center gap-2 small">
    <StatusBadge status={result.status} />
    <span class="faint">{duration}</span>
    {#if result.result_code !== null}<span class="faint mono">· code {result.result_code}</span>{/if}
    <span class="faint">· not saved</span>
  </div>
  {#if result.error}<div class="alert">{result.error}</div>{/if}
  {#if result.result}
    <div>
      <div class="small faint mb-1">Returned</div>
      <ResultView value={result.result} />
    </div>
  {/if}
  <div>
    <div class="small faint mb-1">Output{#if result.output_truncated} · only the end is kept{/if}</div>
    <pre class="term max-h-72" class:empty={!result.output}>{result.output || 'No output.'}</pre>
  </div>
</div>
