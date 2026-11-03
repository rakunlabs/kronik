<script lang="ts">
  import { untrack } from 'svelte'
  import { Plus, X } from '@lucide/svelte'

  // Edits a list of strings as rows; empty rows are dropped from the value.
  let {
    value,
    onchange,
    placeholder = '',
    label = 'Item',
    mono = true,
    disabled = false,
  }: { value: string[]; onchange: (v: string[]) => void; placeholder?: string; label?: string; mono?: boolean; disabled?: boolean } = $props()

  // The initial value seeds the rows; later changes come from the rows,
  // unless the value is replaced from outside (a reset).
  let rows = $state(untrack(() => (value ?? []).map((v) => ({ v }))))
  let last = untrack(() => JSON.stringify(value ?? []))

  $effect(() => {
    const k = JSON.stringify(value ?? [])
    if (k !== last) {
      last = k
      rows = (value ?? []).map((v) => ({ v }))
    }
  })

  function emit() {
    const out = rows.map((r) => r.v.trim()).filter(Boolean)
    last = JSON.stringify(out)
    onchange(out)
  }
</script>

<div class="grid gap-1.5">
  {#each rows as r, i}
    <div class="flex gap-1.5">
      <input class="flex-1" class:mono {placeholder} aria-label="{label} {i + 1}" bind:value={r.v} oninput={emit} {disabled} />
      <button type="button" class="ghost icon" aria-label="Remove {label} {i + 1}" onclick={() => (rows.splice(i, 1), emit())} {disabled}>
        <X size={14} />
      </button>
    </div>
  {/each}
  <div>
    <button type="button" class="ghost sm" onclick={() => rows.push({ v: '' })} {disabled}><Plus size={13} /> Add</button>
  </div>
</div>
