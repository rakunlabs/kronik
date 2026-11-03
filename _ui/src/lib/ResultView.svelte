<script lang="ts">
  // A run's return value (JSON): pretty-printed, or one line when compact.
  let { value, compact = false }: { value: string; compact?: boolean } = $props()

  const pretty = $derived.by(() => {
    try {
      return JSON.stringify(JSON.parse(value), null, compact ? 0 : 2)
    } catch {
      return value
    }
  })
</script>

{#if compact}
  <code class="inline-block max-w-full truncate align-bottom text-ink-2" title={pretty}>{pretty}</code>
{:else}
  <pre class="term max-h-72">{pretty}</pre>
{/if}

