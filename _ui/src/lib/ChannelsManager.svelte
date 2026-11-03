<script lang="ts">
  import { Plus, Pencil, Trash2, Send, Check, X, Hash, Users, Webhook, Mail } from '@lucide/svelte'
  import { api, errorMessage, type Channel, type ChannelInput, type Notification } from './api'
  import { href } from './router.svelte'
  import { fmtTime, relative } from './format'
  import { clock, poll } from './live.svelte'
  import { toast } from './toast.svelte'
  import { confirm } from './confirm.svelte'
  import KeyValues from './KeyValues.svelte'
  import ListEditor from './ListEditor.svelte'

  // Where notifications go, and what was sent lately.
  let channels = $state<Channel[] | null>(null)
  let sent = $state<Notification[]>([])
  let error = $state('')

  async function loadChannels() {
    try {
      channels = await api.channels()
      error = ''
    } catch (e) {
      error = errorMessage(e)
    }
  }

  loadChannels()

  $effect(() =>
    poll(async (signal) => {
      try {
        sent = await api.notifications({ limit: 50 }, signal)
      } catch {}
    }, 10000),
  )

  const types = [
    { id: 'slack', label: 'Slack', icon: Hash, hint: 'An incoming webhook URL from a Slack app.' },
    { id: 'teams', label: 'Microsoft Teams', icon: Users, hint: 'A workflow ("When a Teams webhook request is received") or incoming webhook URL.' },
    { id: 'webhook', label: 'Webhook', icon: Webhook, hint: 'kronik POSTs JSON: event, title, text, link, job, run.' },
    { id: 'email', label: 'E-mail', icon: Mail, hint: 'Sent through the SMTP server in Settings → Configuration → Notifications.' },
  ] as const

  let editing = $state<(ChannelInput & { id?: string }) | null>(null)
  let saving = $state(false)
  let formError = $state('')

  function add() {
    editing = { name: '', type: 'slack', config: { url: '', headers: {}, to: [] } }
    formError = ''
  }

  function edit(c: Channel) {
    editing = { id: c.id, name: c.name, type: c.type, config: { url: '', headers: {}, to: [], ...$state.snapshot(c.config) } }
    formError = ''
  }

  async function save(e: SubmitEvent) {
    e.preventDefault()
    if (!editing) return
    saving = true
    formError = ''
    const { id, ...body } = $state.snapshot(editing)
    try {
      if (id) await api.updateChannel(id, body)
      else await api.createChannel(body)
      toast(id ? 'Channel saved' : 'Channel added')
      editing = null
      await loadChannels()
    } catch (err) {
      formError = errorMessage(err)
    } finally {
      saving = false
    }
  }

  async function remove(c: Channel) {
    const ok = await confirm({
      title: `Delete ${c.name}?`,
      body: 'Jobs stop notifying it, and its unsent notifications are dropped.',
      action: 'Delete',
      danger: true,
    })
    if (!ok) return
    try {
      await api.deleteChannel(c.id)
      toast(`${c.name} deleted`)
      await loadChannels()
    } catch (e) {
      toast(errorMessage(e), { tone: 'danger' })
    }
  }

  let testing = $state<Record<string, boolean>>({})
  async function test(c: Channel) {
    testing[c.id] = true
    try {
      const r = await api.testChannel(c.id)
      toast(r.ok ? `Test message sent to ${c.name}` : `${c.name}: ${r.message}`, r.ok ? undefined : { tone: 'danger' })
    } catch (e) {
      toast(errorMessage(e), { tone: 'danger' })
    } finally {
      testing[c.id] = false
    }
  }

  const typeOf = (t: string) => types.find((x) => x.id === t)
  const eventText: Record<string, string> = { failure: 'failed', recovery: 'recovered', no_run: 'did not run', success: 'succeeded', test: 'test' }
  // The URL of a Slack or Teams webhook is its credential: only its host is shown.
  const where = (c: Channel) => (c.type === 'email' ? (c.config.to ?? []).join(', ') : hostOf(c.config.url ?? ''))
  function hostOf(u: string) {
    if (u.includes('{{')) return u
    try {
      return new URL(u).host
    } catch {
      return u
    }
  }
</script>

{#if error && !channels}
  <div class="alert">Could not load the channels: {error}</div>
{:else if !channels}
  <p class="faint">Loading…</p>
{:else}
  <div class="grid gap-6 2xl:grid-cols-2 items-start">
    <div class="grid gap-3 min-w-0">
      <p class="muted text-[13px] max-w-[80ch]">
        A job notifies the channels picked on its edit page (Alerts): when it starts failing, when it recovers, when it did not run within
        its alert time, or on every success. A job that keeps failing is told once, not on every run. Sending and e-mail are set in
        <a href={href('settings', 'configuration', { group: 'notifications' })}>Configuration → Notifications</a>.
      </p>

      <div class="flex items-center justify-between gap-2">
        <h2>Channels</h2>
        <button class="primary" onclick={add}><Plus size={14} /> Add a channel</button>
      </div>

      {#if editing}
        {@const t = typeOf(editing.type)}
        <form class="panel grid gap-3 p-4" onsubmit={save}>
          <div class="grid gap-3 md:grid-cols-2">
            <label>Name <input required maxlength="100" bind:value={editing.name} placeholder="ops-alerts" /></label>
            <label>
              Type
              <select bind:value={editing.type}>
                {#each types as x}<option value={x.id}>{x.label}</option>{/each}
              </select>
            </label>
          </div>
          {#if t}<p class="hint -mt-1">{t.hint}</p>{/if}
          {#if editing.type === 'email'}
            <div class="grid gap-1">
              <span class="text-[12px] font-medium muted">Recipients</span>
              <ListEditor value={editing.config.to ?? []} onchange={(v) => (editing!.config.to = v)} placeholder="ops@example.com" label="Recipient" mono={false} />
            </div>
          {:else}
            <label>
              URL
              <input class="mono" required bind:value={editing.config.url} placeholder="https://hooks.slack.com/services/…" spellcheck="false" autocomplete="off" />
              <span class="hint">It is a credential: keep it in a secret and write <code>&#123;&#123;secret.SLACK_URL&#125;&#125;</code>.</span>
            </label>
            {#if editing.type === 'webhook'}
              <div class="grid gap-1">
                <span class="text-[12px] font-medium muted">Headers</span>
                <KeyValues value={editing.config.headers ?? {}} onchange={(v) => (editing!.config.headers = v)} keyLabel="Header" />
                <span class="hint">Values may use <code>&#123;&#123;secret.NAME&#125;&#125;</code>.</span>
              </div>
            {/if}
          {/if}
          {#if formError}<div class="alert">{formError}</div>{/if}
          <div class="flex gap-2">
            <button class="primary" type="submit" disabled={saving}>{saving ? 'Saving…' : 'Save'}</button>
            <button type="button" onclick={() => (editing = null)}>Cancel</button>
          </div>
        </form>
      {/if}

      <div class="panel table-wrap">
        <table>
          <thead><tr><th>Name</th><th>Type</th><th>To</th><th><span class="sr-only">Actions</span></th></tr></thead>
          <tbody>
            {#each channels as c (c.id)}
              {@const t = typeOf(c.type)}
              <tr>
                <td class="font-medium">{c.name}</td>
                <td class="small"><span class="inline-flex items-center gap-1.5">{#if t}<t.icon size={13} />{t.label}{:else}{c.type}{/if}</span></td>
                <td class="grow small mono">{where(c)}</td>
                <td class="shrink">
                  <div class="flex justify-end gap-1">
                    <button class="ghost sm" disabled={testing[c.id]} onclick={() => test(c)}><Send size={12} /> Test</button>
                    <button class="ghost sm" onclick={() => edit(c)} aria-label="Edit {c.name}"><Pencil size={12} /></button>
                    <button class="ghost sm danger" onclick={() => remove(c)} aria-label="Delete {c.name}"><Trash2 size={12} /></button>
                  </div>
                </td>
              </tr>
            {:else}
              <tr><td colspan="4" class="empty">No channels yet. Add Slack, Teams, a webhook or e-mail.</td></tr>
            {/each}
          </tbody>
        </table>
      </div>
    </div>

    <div class="grid gap-3 min-w-0">
      <h2>Sent lately</h2>
      <div class="panel table-wrap">
        <table>
          <thead><tr><th>When</th><th>Job</th><th>Event</th><th>Channel</th><th>Status</th></tr></thead>
          <tbody>
            {#each sent as n (n.id)}
              <tr>
                <td class="small" title={fmtTime(n.created_at)}>{relative(n.created_at, clock.now)}</td>
                <td class="small">
                  {#if n.job_id}<a href={href('job', n.job_id)}>{n.payload.job_name ?? n.job_id}</a>{:else}<span class="faint">—</span>{/if}
                  {#if n.run_id}<a class="faint ml-1" href={href('run', n.run_id)}>run</a>{/if}
                </td>
                <td class="small">{eventText[n.event] ?? n.event}</td>
                <td class="small">{n.channel_name}</td>
                <td class="small grow" title={n.error || undefined}>
                  {#if n.status === 'sent'}
                    <span class="badge ok"><Check size={10} /> sent</span>
                  {:else if n.status === 'failed'}
                    <span class="badge danger"><X size={10} /> failed</span> <span class="faint truncate">{n.error}</span>
                  {:else}
                    <span class="badge">{n.attempts ? `retrying (${n.attempts})` : 'pending'}</span>
                    {#if n.error}<span class="faint"> {n.error}</span>{/if}
                  {/if}
                </td>
              </tr>
            {:else}
              <tr><td colspan="5" class="empty">Nothing sent yet.</td></tr>
            {/each}
          </tbody>
        </table>
      </div>
    </div>
  </div>
{/if}
