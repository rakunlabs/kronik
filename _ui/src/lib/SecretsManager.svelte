<script lang="ts">
  import { KeyRound, Plus, Pencil, Trash2, RefreshCw, ShieldAlert, Lock } from '@lucide/svelte'
  import { api, errorMessage, type Secret, type SecretsPage } from './api'
  import { app } from './app.svelte'
  import { href } from './router.svelte'
  import { fmtTime, relative } from './format'
  import { clock } from './live.svelte'
  import { toast } from './toast.svelte'
  import { confirm } from './confirm.svelte'

  // Secrets set from the UI are encrypted in PostgreSQL; their values are
  // written once and never shown again. Secrets from the environment are
  // listed read-only.
  let page = $state<SecretsPage | null>(null)
  let error = $state('')

  async function load() {
    try {
      page = await api.secrets()
      error = ''
    } catch (e) {
      error = errorMessage(e)
    }
  }

  load()

  // The editor: a new secret, or another value / description for one.
  let editing = $state<{ name: string; description: string; value: string; isNew: boolean; changeValue: boolean } | null>(null)
  let saving = $state(false)
  let formError = $state('')

  function add() {
    editing = { name: '', description: '', value: '', isNew: true, changeValue: true }
    formError = ''
  }

  function edit(s: Secret) {
    editing = { name: s.name, description: s.description, value: '', isNew: false, changeValue: false }
    formError = ''
  }

  async function save(e: SubmitEvent) {
    e.preventDefault()
    if (!editing) return
    const name = editing.name.trim()
    if (editing.isNew && page?.secrets.some((s) => s.name === name && s.source === 'ui')) {
      formError = `${name} exists already; edit it instead`
      return
    }
    saving = true
    formError = ''
    try {
      page = await api.putSecret(name, {
        description: editing.description,
        ...(editing.changeValue ? { value: editing.value } : {}),
      })
      toast(editing.isNew ? `${name} added` : `${name} saved`)
      editing = null
      refreshInfo()
    } catch (err) {
      formError = errorMessage(err)
    } finally {
      saving = false
    }
  }

  async function remove(s: Secret) {
    const ok = await confirm({
      title: `Delete ${s.name}?`,
      body: s.shadows
        ? 'Jobs then get the value from the environment again.'
        : 'Jobs that use it fail until it is set again: scripts read undefined, configs with {{secret.' + s.name + '}} stop with an error.',
      action: 'Delete',
      danger: true,
    })
    if (!ok) return
    try {
      page = await api.deleteSecret(s.name)
      toast(`${s.name} deleted`)
      refreshInfo()
    } catch (e) {
      toast(errorMessage(e), { tone: 'danger' })
    }
  }

  let rotating = $state(false)
  async function rotate() {
    const ok = await confirm({
      title: 'Rotate the data key?',
      body: 'A new data key is made, every secret is encrypted again with it and old keys no secret uses are deleted. Jobs keep working.',
      action: 'Rotate',
    })
    if (!ok) return
    rotating = true
    try {
      const r = await api.rotateSecrets()
      toast(`New key; ${r.reencrypted} secret${r.reencrypted === 1 ? '' : 's'} encrypted again`)
      await load()
    } catch (e) {
      toast(errorMessage(e), { tone: 'danger' })
    } finally {
      rotating = false
    }
  }

  // Jobs and the editor list the names from /info.
  function refreshInfo() {
    api.info().then((i) => (app.info = i)).catch(() => {})
  }

  const active = $derived(page?.keys.find((k) => k.active))
</script>

{#if error && !page}
  <div class="alert">Could not load the secrets: {error}</div>
{:else if !page}
  <p class="faint">Loading…</p>
{:else}
  <div class="grid gap-5 2xl:grid-cols-[minmax(0,1fr)_420px] items-start">
    <div class="grid gap-3 min-w-0">
      <p class="muted text-[13px] max-w-[80ch]">
        Scripts read them as <code>secrets.NAME</code>; HTTP, command and container jobs and notification channels use
        <code>&#123;&#123;secret.NAME&#125;&#125;</code>. Their values are hidden in run output. A value is written once and never shown again.
      </p>

      {#if !page.can_store}
        <div class="note flex gap-2">
          <ShieldAlert size={16} class="shrink-0 mt-0.5 text-warn" />
          <span>
            Secrets cannot be added here: no master key is set. Give every replica the same <code>KRONIK_SECRETS_KEY</code>
            (<code>openssl rand -base64 32</code>) and restart them. Until then, secrets come only from <code>KRONIK_SECRET_&lt;NAME&gt;</code>.
          </span>
        </div>
      {/if}
      {#if page.problem}<div class="alert">{page.problem}</div>{/if}

      <div class="flex items-center justify-between gap-2">
        <h2>Secrets</h2>
        <button class="primary" disabled={!page.can_store} onclick={add}><Plus size={14} /> Add a secret</button>
      </div>

      {#if editing}
        <form class="panel grid gap-3 p-4" onsubmit={save}>
          <h3 class="font-semibold text-[14px]">{editing.isNew ? 'New secret' : editing.name}</h3>
          <div class="grid gap-3 md:grid-cols-2">
            {#if editing.isNew}
              <label>
                Name
                <input
                  class="mono"
                  required
                  bind:value={editing.name}
                  pattern="[A-Za-z_][A-Za-z0-9_]*"
                  placeholder="API_TOKEN"
                  spellcheck="false"
                  autocomplete="off"
                />
                <span class="hint">Letters, digits and _: <code>secrets.{editing.name || 'NAME'}</code>.</span>
              </label>
            {/if}
            <label class={editing.isNew ? '' : 'md:col-span-2'}>
              Description <input bind:value={editing.description} placeholder="What it is for, who owns it" />
            </label>
          </div>
          {#if !editing.isNew}
            <label class="check"><input type="checkbox" bind:checked={editing.changeValue} /> Set a new value</label>
          {/if}
          {#if editing.changeValue}
            <label>
              Value
              <textarea
                class="mono"
                rows="3"
                required
                bind:value={editing.value}
                autocomplete="off"
                spellcheck="false"
                placeholder="The value; it is encrypted and not shown again"
              ></textarea>
            </label>
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
          <thead><tr><th>Name</th><th>Description</th><th>From</th><th>Changed</th><th><span class="sr-only">Actions</span></th></tr></thead>
          <tbody>
            {#each page.secrets as s (s.name + s.source)}
              <tr>
                <td>
                  <span class="inline-flex items-center gap-1.5 font-mono text-[12.5px]"><KeyRound size={12} class="faint" />{s.name}</span>
                  {#if !s.readable}<span class="badge danger ml-1" title="This replica cannot decrypt it">unreadable</span>{/if}
                </td>
                <td class="grow small">{s.description}</td>
                <td class="small">
                  {#if s.source === 'env'}
                    <span class="badge" title="KRONIK_SECRET_{s.name} on the replicas">environment</span>
                  {:else}
                    <span class="badge ok"><Lock size={10} /> encrypted</span>
                    {#if s.shadows}<span class="badge warn" title="Replaces KRONIK_SECRET_{s.name}">overrides env</span>{/if}
                  {/if}
                </td>
                <td class="small">
                  {#if s.updated_at}
                    <span title={fmtTime(s.updated_at)}>{relative(s.updated_at, clock.now)}</span>
                    {#if s.updated_by}<span class="faint"> · {s.updated_by}</span>{/if}
                  {:else}<span class="faint">—</span>{/if}
                </td>
                <td class="shrink">
                  {#if s.source === 'ui'}
                    <div class="flex justify-end gap-1">
                      <button class="ghost sm" disabled={!page.can_store} onclick={() => edit(s)} aria-label="Edit {s.name}"><Pencil size={12} /> Edit</button>
                      <button class="ghost sm danger" onclick={() => remove(s)} aria-label="Delete {s.name}"><Trash2 size={12} /></button>
                    </div>
                  {/if}
                </td>
              </tr>
            {:else}
              <tr><td colspan="5" class="empty">No secrets yet.</td></tr>
            {/each}
          </tbody>
        </table>
      </div>
    </div>

    <section class="panel grid gap-3 p-4">
      <div class="flex items-center justify-between gap-2">
        <h2>Encryption</h2>
        <button class="sm" disabled={!page.can_store || rotating} onclick={rotate}><RefreshCw size={12} /> {rotating ? 'Rotating…' : 'Rotate now'}</button>
      </div>
      <p class="small muted">
        Each value is sealed with AES-256-GCM under a data key; data keys are sealed under the master key from
        <code>KRONIK_SECRETS_KEY</code>, which is never stored.
      </p>
      <dl class="kv">
        <dt>Master key</dt>
        <dd>{#if page.master_id}<code>{page.master_id}</code>{:else}<span class="text-warn">not set</span>{/if}</dd>
        <dt>Data key</dt>
        <dd>
          {#if active}<code>{active.id.slice(-8)}</code> <span class="faint">· {relative(active.created_at, clock.now)}</span>{:else}<span class="faint">none yet</span>{/if}
        </dd>
        <dt>Rotation</dt>
        <dd>
          {#if page.rotate && page.rotate !== '0'}every {page.rotate}{:else}by hand only{/if}
          <a class="small ml-1" href={href('settings', 'configuration', { group: 'secrets' })}>change</a>
        </dd>
      </dl>
      {#if page.keys.length}
        <table class="text-[12px]">
          <thead><tr><th>Data key</th><th>Master</th><th>Made</th><th class="text-right!">Secrets</th></tr></thead>
          <tbody>
            {#each page.keys as k (k.id)}
              <tr>
                <td class="mono">{k.id.slice(-8)}{#if k.active}<span class="badge ok ml-1">active</span>{/if}</td>
                <td class="mono {k.master !== page.master_id ? 'text-warn' : ''}" title={k.master !== page.master_id ? 'An older master key: rotate to move it' : ''}>{k.master}</td>
                <td title={fmtTime(k.created_at)}>{relative(k.created_at, clock.now)}</td>
                <td class="text-right">{k.secrets}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      {/if}
      <p class="hint">
        To change the master key: set the new one as <code>KRONIK_SECRETS_KEY</code> and the old one as <code>KRONIK_SECRETS_OLD_KEYS</code> on every
        replica, restart, press Rotate now, then remove the old key.
      </p>
    </section>
  </div>
{/if}
