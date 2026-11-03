<script lang="ts">
  import type { Snippet } from 'svelte'
  import { ArrowLeft } from '@lucide/svelte'

  let {
    title,
    back,
    backLabel = 'Back',
    badges,
    actions,
    subtitle,
  }: {
    title: string
    back?: string
    backLabel?: string
    badges?: Snippet
    actions?: Snippet
    subtitle?: Snippet
  } = $props()
</script>

<header class="mb-5">
  {#if back}
    <a href={back} class="back mb-1.5"><ArrowLeft size={13} /> {backLabel}</a>
  {/if}
  <div class="flex flex-wrap items-center justify-between gap-3 min-h-[34px]">
    <div class="flex items-center gap-2.5 flex-wrap min-w-0">
      <h1 class="truncate max-w-full">{title}</h1>
      {@render badges?.()}
    </div>
    {#if actions}
      <div class="flex gap-2 flex-wrap">{@render actions()}</div>
    {/if}
  </div>
  {#if subtitle}<div class="muted mt-1">{@render subtitle()}</div>{/if}
</header>
