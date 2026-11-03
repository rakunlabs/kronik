<script lang="ts">
  import { X } from '@lucide/svelte'
  import { pending, answer } from './confirm.svelte'
  import { toasts, dismiss } from './toast.svelte'

  let dialog = $state<HTMLDialogElement>()

  $effect(() => {
    if (!dialog) return
    if (pending.current && !dialog.open) dialog.showModal()
    else if (!pending.current && dialog.open) dialog.close()
  })
</script>

<dialog bind:this={dialog} class="panel w-[min(420px,calc(100vw-32px))] p-5 text-ink backdrop:bg-black/35" aria-labelledby="confirm-title" onclose={() => pending.current && answer(false)}>
  {#if pending.current}
    {@const c = pending.current}
    <form method="dialog" onsubmit={(e) => (e.preventDefault(), answer(true))}>
      <h2 id="confirm-title" class="mb-2">{c.title}</h2>
      <p class="muted text-[13px] mb-5">{c.body}</p>
      <!-- A dangerous action starts on Cancel, so Enter does not delete. -->
      <div class="flex justify-end gap-2">
        <!-- svelte-ignore a11y_autofocus -->
        <button type="button" onclick={() => answer(false)} autofocus={c.danger}>Cancel</button>
        <!-- svelte-ignore a11y_autofocus -->
        <button type="submit" class={c.danger ? 'danger solid' : 'primary'} autofocus={!c.danger}>{c.action}</button>
      </div>
    </form>
  {/if}
</dialog>

<div class="fixed right-4 bottom-4 z-50 grid w-[min(360px,calc(100vw-32px))] gap-2" aria-live="polite">
  {#each toasts as t (t.id)}
    <div class="panel flex items-center gap-2.5 py-1.5 pr-1.5 pl-3 text-[13px] shadow-[0_8px_24px_-10px_rgb(0_0_0/0.3)]">
      <span class="dot" style:color={t.tone === 'danger' ? 'var(--danger)' : 'var(--ok)'}></span>
      <span class="flex-1">{t.text}</span>
      {#if t.href}<a href={t.href} onclick={() => dismiss(t.id)}>{t.link ?? 'Open'}</a>{/if}
      <button class="ghost icon" aria-label="Dismiss" onclick={() => dismiss(t.id)}><X size={13} /></button>
    </div>
  {/each}
</div>

