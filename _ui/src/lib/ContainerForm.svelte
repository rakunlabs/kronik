<script lang="ts">
  import { Plus, X } from '@lucide/svelte'
  import type { ContainerConfig } from './api'
  import KeyValues from './KeyValues.svelte'
  import ListEditor from './ListEditor.svelte'

  // The fields of a container job; kind decides which runtime-only fields
  // are shown.
  let { value = $bindable(), kind }: { value: ContainerConfig; kind: 'docker' | 'swarm' | 'kubernetes' } = $props()

  // command and args are edited as one line each, split like a shell would
  // for simple cases; JSON arrays are accepted for anything trickier.
  function split(line: string): string[] {
    const t = line.trim()
    if (!t) return []
    if (t.startsWith('[')) {
      try {
        const a = JSON.parse(t)
        if (Array.isArray(a)) return a.map(String)
      } catch {}
    }
    const out: string[] = []
    let cur = ''
    let quote = ''
    let has = false
    for (const ch of t) {
      if (quote) {
        if (ch === quote) quote = ''
        else cur += ch
      } else if (ch === '"' || ch === "'") {
        quote = ch
        has = true
      } else if (/\s/.test(ch)) {
        if (cur || has) out.push(cur)
        cur = ''
        has = false
      } else cur += ch
    }
    if (cur || has) out.push(cur)
    return out
  }

  function join(a: string[]): string {
    return (a ?? []).map((x) => (x === '' || /[\s"']/.test(x) ? JSON.stringify(x) : x)).join(' ')
  }

  let commandText = $state(join(value.command))
  let argsText = $state(join(value.args))

  const pullHelp: Record<string, string> = {
    '': 'Use the setting',
    missing: 'Pull when the image is not there',
    always: 'Always pull',
    never: 'Never pull',
  }

  function addMount() {
    value.mounts = [...(value.mounts ?? []), { type: 'bind', source: '', target: '', read_only: true }]
  }
</script>

<div class="grid gap-3">
  <div class="grid md:grid-cols-[1fr_180px] gap-3">
    <label>
      Image
      <input class="mono" required bind:value={value.image} placeholder="alpine:3.22 or ghcr.io/acme/tool:1.4" spellcheck="false" />
    </label>
    <label>
      Pull
      <select bind:value={value.pull}>
        {#each Object.entries(pullHelp) as [k, v]}<option value={k}>{v}</option>{/each}
      </select>
    </label>
  </div>

  <div class="grid md:grid-cols-2 gap-3">
    <label>
      Command
      <input
        class="mono"
        bind:value={commandText}
        oninput={() => (value.command = split(commandText))}
        placeholder="the image's ENTRYPOINT"
        spellcheck="false"
      />
    </label>
    <label>
      Arguments
      <input class="mono" bind:value={argsText} oninput={() => (value.args = split(argsText))} placeholder="the image's CMD" spellcheck="false" />
    </label>
  </div>
  <p class="hint">
    Split like a shell: <code>sh -c "echo $HOME"</code>. For anything trickier give a JSON list: <code>["sh", "-c", "…"]</code>.
    {#if value.command.length || value.args.length}
      Runs: <code>{JSON.stringify([...value.command, ...value.args])}</code>
    {/if}
  </p>

  <div class="grid gap-1">
    <span class="text-[12px] font-medium muted">Environment</span>
    <KeyValues value={value.env} onchange={(v) => (value.env = v)} keyLabel="NAME" />
    <span class="hint">Also set: <code>KRONIK_JOB_ID</code>, <code>KRONIK_JOB_NAME</code>, <code>KRONIK_RUN_ID</code>, <code>KRONIK_ATTEMPT</code>, <code>KRONIK_SCHEDULED_AT</code>.</span>
  </div>

  {#if kind === 'kubernetes'}
    <div class="grid gap-1">
      <span class="text-[12px] font-medium muted">Environment from Secrets</span>
      <KeyValues value={value.secret_env} onchange={(v) => (value.secret_env = v)} keyLabel="NAME" valueLabel="secret-name/key" />
      <span class="hint">Read from a Kubernetes Secret in the Job's namespace; the value never passes through kronik.</span>
    </div>
  {/if}

  <div class="grid md:grid-cols-4 gap-3">
    <label>CPU <input class="mono" bind:value={value.cpu} placeholder="no limit" /><span class="hint">0.5, 2 or 500m</span></label>
    <label>Memory <input class="mono" bind:value={value.memory} placeholder="no limit" /><span class="hint">256Mi, 1Gi</span></label>
    <label>Working directory <input class="mono" bind:value={value.workdir} placeholder="the image's" spellcheck="false" /><span class="hint">&nbsp;</span></label>
    <label>
      User
      <input class="mono" bind:value={value.user} placeholder="the image's" spellcheck="false" />
      <span class="hint">{kind === 'kubernetes' ? 'uid or uid:gid' : 'uid, uid:gid or a name'}</span>
    </label>
  </div>

  {#if kind === 'docker'}
    <label>Network <input class="mono" bind:value={value.network} placeholder="the setting's, else bridge" spellcheck="false" /></label>
  {:else if kind === 'swarm'}
    <div class="grid md:grid-cols-2 gap-3">
      <div class="grid gap-1">
        <span class="text-[12px] font-medium muted">Networks</span>
        <ListEditor value={value.networks} onchange={(v) => (value.networks = v)} placeholder="an overlay network" label="Network" />
      </div>
      <div class="grid gap-1">
        <span class="text-[12px] font-medium muted">Placement constraints</span>
        <ListEditor value={value.constraints} onchange={(v) => (value.constraints = v)} placeholder="node.role==worker" label="Constraint" />
      </div>
    </div>
  {:else}
    <div class="grid md:grid-cols-2 gap-3">
      <label>Namespace <input class="mono" bind:value={value.namespace} placeholder="the default namespace" spellcheck="false" /></label>
      <label>Service account <input class="mono" bind:value={value.service_account} placeholder="the default" spellcheck="false" /></label>
    </div>
    <div class="grid gap-1">
      <span class="text-[12px] font-medium muted">Node selector</span>
      <KeyValues value={value.node_selector} onchange={(v) => (value.node_selector = v)} keyLabel="label" />
    </div>
  {/if}

  {#if kind !== 'kubernetes'}
    <div class="grid gap-1.5">
      <span class="text-[12px] font-medium muted">Mounts</span>
      {#each value.mounts ?? [] as m, i}
        <div class="grid grid-cols-[100px_1fr_1fr_auto_auto] gap-1.5 items-center">
          <select bind:value={m.type} aria-label="Mount type">
            <option value="bind">host path</option>
            <option value="volume">volume</option>
          </select>
          <input class="mono" bind:value={m.source} placeholder={m.type === 'bind' ? '/srv/data' : 'volume name'} aria-label="Source" />
          <input class="mono" bind:value={m.target} placeholder="/data in the container" aria-label="Target" />
          <label class="check text-[12px]!"><input type="checkbox" bind:checked={m.read_only} /> read-only</label>
          <button type="button" class="ghost icon" aria-label="Remove mount" onclick={() => (value.mounts = value.mounts.filter((_, j) => j !== i))}>
            <X size={14} />
          </button>
        </div>
      {/each}
      <div><button type="button" class="ghost sm" onclick={addMount}><Plus size={13} /> Add a mount</button></div>
      <span class="hint">Refused unless mounts are allowed in Settings → Configuration.</span>
    </div>
  {/if}

  {#if kind === 'docker'}
    <label class="check"><input type="checkbox" bind:checked={value.privileged} /> Privileged <span class="faint">(full access to the host; must be allowed in the settings)</span></label>
  {/if}

  <div class="grid gap-1">
    <span class="text-[12px] font-medium muted">Labels</span>
    <KeyValues value={value.labels} onchange={(v) => (value.labels = v)} keyLabel="label" />
  </div>
</div>
