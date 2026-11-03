<script lang="ts">
  import { CalendarClock, History, Settings as SettingsIcon, Sun, Moon, Monitor, Menu, X } from '@lucide/svelte'
  import { app } from './lib/app.svelte'
  import { router, href, nav } from './lib/router.svelte'
  import Overlays from './lib/Overlays.svelte'
  import Jobs from './routes/Jobs.svelte'
  import Job from './routes/Job.svelte'
  import JobForm from './routes/JobForm.svelte'
  import Runs from './routes/Runs.svelte'
  import Run from './routes/Run.svelte'
  import Settings from './routes/Settings.svelte'

  const route = $derived(router.route)

  // Each nav entry owns the sections opened from it.
  const menu = [
    { label: 'Jobs', icon: CalendarClock, to: href('jobs'), sections: ['jobs', 'job', 'new', 'edit'] },
    { label: 'Runs', icon: History, to: href('runs'), sections: ['runs', 'run'] },
    { label: 'Settings', icon: SettingsIcon, to: href('settings'), sections: ['settings'] },
  ]

  const titles: Record<string, string> = {
    jobs: 'Jobs',
    job: 'Job',
    new: 'New job',
    edit: 'Edit job',
    runs: 'Runs',
    run: 'Run',
    settings: 'Settings',
  }
  const env = $derived(app.info?.environment?.name ? app.info.environment : null)

  $effect(() => {
    const prefix = env ? `[${env.name}] ` : ''
    document.title = `${prefix}${titles[route.section] ?? 'Not found'} · kronik`
  })

  // A new page starts at the top and takes the focus, for keyboard and screen
  // reader users; Back and Forward keep the browser's scroll position.
  $effect(() => {
    void route.section
    void route.id
    if (nav.fromHistory) {
      nav.fromHistory = false
      return
    }
    window.scrollTo(0, 0)
    queueMicrotask(() => mainEl?.focus({ preventScroll: true }))
  })
  let mainEl = $state<HTMLElement>()

  type Theme = 'system' | 'light' | 'dark'
  const themeKey = 'kronik.theme'
  const stored = (() => {
    try {
      return localStorage.getItem(themeKey)
    } catch {
      return null
    }
  })()
  let theme = $state<Theme>(stored === 'light' || stored === 'dark' ? stored : 'system')
  $effect(() => {
    if (theme === 'system') delete document.documentElement.dataset.theme
    else document.documentElement.dataset.theme = theme
    try {
      localStorage.setItem(themeKey, theme)
    } catch {}
  })
  const nextTheme: Record<Theme, Theme> = { system: 'light', light: 'dark', dark: 'system' }
  const themeLabel: Record<Theme, string> = { system: 'System theme', light: 'Light theme', dark: 'Dark theme' }

  // The sidebar folds into a menu on narrow screens.
  let menuOpen = $state(false)
  $effect(() => {
    void route.section
    void route.id
    menuOpen = false
  })
</script>

<div class="min-h-screen">
  <!-- Fixed to the viewport: the page scrolls, the sidebar stays. On narrow
       screens it is a bar on top that opens into the menu. -->
  <aside
    class="theme-dark fixed inset-y-0 left-0 z-20 flex w-54 flex-col gap-2 overflow-y-auto border-r border-[#262623] bg-[#131311] px-2.5 py-3
      max-md:inset-x-0 max-md:bottom-auto max-md:max-h-screen max-md:w-auto max-md:border-r-0 max-md:border-b max-md:px-3 max-md:py-2
      {menuOpen ? 'max-md:pb-3' : ''} {env ? 'border-t-3 border-t-(--env)' : ''}"
    style:--env={env?.color}
  >
    {#if env}
      <!-- The environment strip: the first thing on every page, in its colour. -->
      <div
        class="-mt-0.5 mb-1 flex min-w-0 items-center gap-2 rounded-box border border-[color-mix(in_srgb,var(--env)_55%,transparent)]
          bg-[color-mix(in_srgb,var(--env)_22%,#131311)] px-2.5 py-1.5 text-xs font-bold tracking-[0.08em] text-[#f4f4f0] uppercase max-md:mt-0 max-md:mb-1.5"
        title="This kronik is the {env.name} environment"
      >
        <span class="size-2 flex-none rounded-full bg-(--env) shadow-[0_0_0_3px_color-mix(in_srgb,var(--env)_30%,transparent)]" aria-hidden="true"></span>
        <span class="truncate">{env.name}</span>
      </div>
    {/if}
    <div class="flex items-center justify-between">
      <a href={href('jobs')} class="flex items-center gap-[9px] px-2 pt-1 pb-2.5 text-[15px] font-semibold tracking-[-0.01em] text-ink hover:no-underline max-md:pb-1">
        <img src="./favicon.svg" alt="" width="20" height="20" />
        <span>kronik</span>
      </a>
      <button
        class="ghost icon hidden text-ink-2 max-md:inline-flex"
        aria-label={menuOpen ? 'Close menu' : 'Open menu'}
        aria-expanded={menuOpen}
        onclick={() => (menuOpen = !menuOpen)}
      >
        {#if menuOpen}<X size={18} />{:else}<Menu size={18} />{/if}
      </button>
    </div>

    <nav class="grid gap-0.5 {menuOpen ? '' : 'max-md:hidden'}">
      {#each menu as n}
        {@const active = n.sections.includes(route.section)}
        <a
          href={n.to}
          class="flex items-center gap-2.5 rounded-box px-2.5 py-[7px] text-[13.5px] hover:no-underline
            {active ? 'bg-[#262623] font-semibold text-ink shadow-[inset_2px_0_0_var(--cta)]' : 'text-ink-2 hover:bg-[#1f1f1c] hover:text-ink'}"
          aria-current={active ? 'page' : undefined}
        >
          <n.icon size={16} />
          {n.label}
        </a>
      {/each}
    </nav>

    <div class="mt-auto grid gap-1 px-0.5 {menuOpen ? '' : 'max-md:hidden'}">
      {#if app.info?.user}
        <div class="mb-1 flex min-w-0 items-center gap-2 border-t border-[#262623] px-2 py-1.5 text-[12.5px] text-ink-2" title="Signed in through the reverse proxy">
          <span class="grid size-5 flex-none place-items-center rounded-full bg-[#2f2f2a] text-[11px] font-semibold text-cta" aria-hidden="true">
            {app.info.user.slice(0, 1).toUpperCase()}
          </span>
          <span class="truncate">{app.info.user}</span>
        </div>
      {/if}
      <button
        class="ghost justify-start! px-2! py-[5px]! text-xs! font-normal! text-ink-3 not-disabled:hover:bg-[#1f1f1c]! not-disabled:hover:text-ink"
        title={themeLabel[theme]}
        onclick={() => (theme = nextTheme[theme])}
      >
        {#if theme === 'light'}<Sun size={14} />{:else if theme === 'dark'}<Moon size={14} />{:else}<Monitor size={14} />{/if}
        {themeLabel[theme]}
      </button>
    </div>
  </aside>

  <main
    bind:this={mainEl}
    tabindex="-1"
    class="ml-54 max-w-[calc(1800px+13.5rem)] min-w-0 px-7 pt-6 pb-12 focus:outline-none max-md:ml-0 max-md:px-4 max-md:pb-10
      {env ? 'max-md:pt-[116px]' : 'max-md:pt-[72px]'}"
  >
    {#if route.section === 'jobs'}
      <Jobs />
    {:else if route.section === 'job' && route.id}
      {#key route.id}<Job id={route.id} />{/key}
    {:else if route.section === 'new'}
      <JobForm />
    {:else if route.section === 'edit' && route.id}
      {#key route.id}<JobForm id={route.id} />{/key}
    {:else if route.section === 'runs'}
      <Runs />
    {:else if route.section === 'run' && route.id}
      {#key route.id}<Run id={route.id} />{/key}
    {:else if route.section === 'settings'}
      <Settings tab={route.id} />
    {:else}
      <p class="muted">Page not found. <a href={href('jobs')}>Back to jobs</a></p>
    {/if}
  </main>
</div>

<Overlays />

