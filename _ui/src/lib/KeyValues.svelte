<script lang="ts">
  import { untrack } from 'svelte'
  import { Plus, X } from '@lucide/svelte'

  // Edits a string map as rows. Rows are kept while editing, so an empty or
  // duplicate name does not lose what was typed; onchange gets the map.
  let {
    value,
    onchange,
    keyLabel = 'Name',
    valueLabel = 'Value',
  }: { value: Record<string, string>; onchange: (v: Record<string, string>) => void; keyLabel?: string; valueLabel?: string } = $props()

  // The initial value seeds the rows; later changes come from the rows.
  let rows = $state(untrack(() => Object.entries(value ?? {}).map(([k, v]) => ({ k, v }))))

  function emit() {
    const out: Record<string, string> = {}
    for (const r of rows) if (r.k.trim()) out[r.k.trim()] = r.v
    onchange(out)
  }

  const dupes = $derived.by(() => {
    const seen = new Set<string>()
    const d = new Set<string>()
    for (const r of rows) {
      const k = r.k.trim()
      if (k && seen.has(k)) d.add(k)
      seen.add(k)
    }
    return d
  })
</script>

<div class="grid gap-1.5">
  {#each rows as r, i}
    <div class="flex gap-1.5">
      <input class="mono flex-1 aria-invalid:border-danger" placeholder={keyLabel} aria-label={keyLabel} bind:value={r.k} oninput={emit} aria-invalid={dupes.has(r.k.trim())} />
      <input class="mono flex-[2]" placeholder={valueLabel} aria-label={valueLabel} bind:value={r.v} oninput={emit} />
      <button type="button" class="ghost icon" aria-label="Remove" onclick={() => (rows.splice(i, 1), emit())}><X size={14} /></button>
    </div>
  {/each}
  {#if dupes.size}<span class="hint text-danger!">{[...dupes].join(', ')} set twice; the last one is used.</span>{/if}
  <div>
    <button type="button" class="ghost sm" onclick={() => rows.push({ k: '', v: '' })}><Plus size={13} /> Add</button>
  </div>
</div>

