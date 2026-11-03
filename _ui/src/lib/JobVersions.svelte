<script lang="ts">
  import { History, RotateCcw } from '@lucide/svelte'
  import { api, errorMessage, type AuditEntry, type Job, type JobInput } from './api'
  import { fmtTime, relative } from './format'
  import { clock } from './live.svelte'
  import { toast } from './toast.svelte'
  import { confirm } from './confirm.svelte'

  // Versions of a job, from the audit log: what each change changed, field
  // by field, and a button to put an older version back.
  let { job, onrestore }: { job: Job; onrestore: (j: Job) => void } = $props()

  let entries = $state<AuditEntry[]>([])
  let open = $state<string | null>(null)
  let error = $state('')

  async function load() {
    try {
      entries = await api.jobHistory(job.id)
      error = ''
    } catch (e) {
      error = errorMessage(e)
    }
  }

  $effect(() => {
    void job.updated_at
    load()
  })

  type Version = Record<string, unknown>

  const labels: Record<string, string> = {
    name: 'Name',
    description: 'Description',
    tags: 'Tags',
    schedule: 'Schedule',
    timezone: 'Time zone',
    enabled: 'Active',
    executor: 'Executor',
    config: 'Config',
    concurrency: 'When still running',
    catch_up: 'Missed times',
    timeout_seconds: 'Timeout',
    retries: 'Retries',
    retry_delay_seconds: 'Retry after',
    jitter_seconds: 'Random delay',
    alert_after_seconds: 'Did-not-run alert',
    notify: 'Notify',
    after_jobs: 'Run after',
    after_on: 'Run after on',
  }

  const show = (v: unknown) => (typeof v === 'string' ? v : JSON.stringify(v, null, 2))

  // Config changes are shown key by key; other fields as a whole.
  function diff(before: Version, after: Version, fields: string[]) {
    const rows: { field: string; from: string; to: string }[] = []
    for (const f of fields) {
      if (f === 'config' && before.config && after.config && typeof before.config === 'object') {
        const a = before.config as Record<string, unknown>
        const b = after.config as Record<string, unknown>
        for (const k of new Set([...Object.keys(a), ...Object.keys(b)])) {
          if (JSON.stringify(a[k]) !== JSON.stringify(b[k])) rows.push({ field: `config.${k}`, from: show(a[k] ?? ''), to: show(b[k] ?? '') })
        }
      } else {
        rows.push({ field: labels[f] ?? f, from: show(before[f] ?? ''), to: show(after[f] ?? '') })
      }
    }
    return rows
  }

  async function restore(e: AuditEntry, which: 'before' | 'after') {
    const v = e.detail[which] as Version | undefined
    if (!v) return
    const ok = await confirm({
      title: 'Put this version back?',
      body: `The job gets the settings it had ${which === 'before' ? 'before' : 'after'} the change of ${fmtTime(e.at)}. Pausing and resuming stay as they are now.`,
      action: 'Restore',
    })
    if (!ok) return
    try {
      const { enabled: _e, ...rest } = v as unknown as JobInput
      const j = await api.updateJob(job.id, { ...(rest as JobInput), updated_at: job.updated_at })
      toast('Version restored')
      onrestore(j)
    } catch (err) {
      toast(errorMessage(err), { tone: 'danger' })
    }
  }
</script>

<div class="flex items-center justify-between mb-2.5">
  <h2 class="flex items-center gap-2"><History size={16} /> Versions</h2>
</div>

{#if error}<div class="alert">{error}</div>{/if}

<div class="panel divide-y divide-line">
  {#each entries as e, i (e.id)}
    {@const before = e.detail.before as Version | undefined}
    {@const after = e.detail.after as Version | undefined}
    {@const fields = (e.detail.changed as string[] | undefined) ?? []}
    <div class="px-4 py-2.5">
      <button
        type="button"
        class="ghost w-full justify-between! px-0! py-0! font-normal! text-left whitespace-normal!"
        aria-expanded={open === e.id}
        onclick={() => (open = open === e.id ? null : e.id)}
      >
        <span class="flex flex-wrap items-baseline gap-x-2">
          <b class="font-semibold">{e.action === 'job.create' ? 'Created' : fields.length ? fields.map((f) => labels[f] ?? f).join(', ') : 'Saved, nothing changed'}</b>
          <span class="faint small">
            {e.actor || 'someone'} · <span title={fmtTime(e.at)}>{relative(e.at, clock.now)}</span>{#if i === 0}{' '}· current{/if}
          </span>
        </span>
        <span class="faint small">{open === e.id ? 'hide' : 'show'}</span>
      </button>

      {#if open === e.id}
        <div class="mt-2.5 grid gap-2">
          {#if before && after && fields.length}
            <div class="overflow-x-auto">
              <table class="text-[12.5px]">
                <thead><tr><th>Field</th><th>Before</th><th>After</th></tr></thead>
                <tbody>
                  {#each diff(before, after, fields) as r}
                    <tr>
                      <td class="align-top font-medium">{r.field}</td>
                      <td class="align-top"><pre class="m-0 max-w-[44ch] whitespace-pre-wrap wrap-anywhere text-danger font-mono text-[12px]">{r.from}</pre></td>
                      <td class="align-top"><pre class="m-0 max-w-[44ch] whitespace-pre-wrap wrap-anywhere text-ok font-mono text-[12px]">{r.to}</pre></td>
                    </tr>
                  {/each}
                </tbody>
              </table>
            </div>
          {:else if !before && !after}
            <p class="faint small">This change was recorded before versions were kept.</p>
          {/if}
          <div class="flex flex-wrap gap-2">
            {#if before}
              <button type="button" class="sm" onclick={() => restore(e, 'before')}><RotateCcw size={12} /> Restore the version before this change</button>
            {/if}
            {#if after && i > 0}
              <button type="button" class="sm" onclick={() => restore(e, 'after')}><RotateCcw size={12} /> Restore this version</button>
            {/if}
          </div>
        </div>
      {/if}
    </div>
  {:else}
    <p class="faint small px-4 py-3">No versions recorded.</p>
  {/each}
</div>
