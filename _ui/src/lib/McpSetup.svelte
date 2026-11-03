<script lang="ts">
  import { Copy, Check } from '@lucide/svelte'

  // How to add the MCP endpoint to an agent, one tab per client. name is the
  // server name in the client, url the endpoint.
  let { name, url }: { name: string; url: string } = $props()

  type Snippet = { label: string; lang: string; code: string; note?: string }

  const clients = $derived<{ id: string; label: string; snippets: Snippet[] }[]>([
    {
      id: 'claude',
      label: 'Claude Code',
      snippets: [
        {
          label: 'Terminal',
          lang: 'sh',
          code: `claude mcp add --transport http --scope user ${name} ${url}`,
          note: 'Drop --scope user to add it to this project only. Check it with: claude mcp get ' + name,
        },
        {
          label: '.mcp.json (shared with the project)',
          lang: 'json',
          code: JSON.stringify({ mcpServers: { [name]: { type: 'http', url } } }, null, 2),
        },
      ],
    },
    {
      id: 'opencode',
      label: 'OpenCode',
      snippets: [
        {
          label: 'Terminal',
          lang: 'sh',
          code: `opencode mcp add ${name} --global --url ${url}`,
          note: 'Drop --global to add it to this project only. Check it with: opencode mcp list',
        },
        {
          label: 'opencode.jsonc',
          lang: 'jsonc',
          code: JSON.stringify(
            { $schema: 'https://opencode.ai/config.json', mcp: { servers: { [name]: { type: 'remote', url, oauth: false } } } },
            null,
            2,
          ),
          note: 'oauth: false, as the reverse proxy signs you in, not kronik.',
        },
      ],
    },
    {
      id: 'other',
      label: 'Other clients',
      snippets: [
        {
          label: 'mcpServers JSON (Claude Desktop, Cursor, Windsurf, …)',
          lang: 'json',
          code: JSON.stringify({ mcpServers: { [name]: { type: 'http', url } } }, null, 2),
        },
        { label: 'Endpoint (Streamable HTTP)', lang: 'text', code: url },
      ],
    },
  ])

  let tab = $state('claude')
  const active = $derived(clients.find((c) => c.id === tab) ?? clients[0])

  let copied = $state('')
  async function copy(code: string, key: string) {
    try {
      await navigator.clipboard.writeText(code)
      copied = key
      setTimeout(() => copied === key && (copied = ''), 1200)
    } catch {}
  }

  function onKey(e: KeyboardEvent, i: number) {
    const n = clients.length
    const to = e.key === 'ArrowRight' ? (i + 1) % n : e.key === 'ArrowLeft' ? (i + n - 1) % n : -1
    if (to < 0) return
    e.preventDefault()
    tab = clients[to].id
    ;(e.currentTarget as HTMLElement).parentElement?.querySelectorAll('button')[to]?.focus()
  }
</script>

<div class="overflow-hidden rounded-box border border-line-strong">
  <div class="flex gap-0.5 border-b border-line-strong bg-surface-2 px-1.5" role="tablist" aria-label="MCP client">
    {#each clients as c, i}
      <button
        type="button"
        role="tab"
        id="mcp-tab-{c.id}"
        aria-selected={tab === c.id}
        aria-controls="mcp-panel"
        tabindex={tab === c.id ? 0 : -1}
        class="-mb-px rounded-none! border-0! border-b-2! bg-transparent! px-3! py-2! hover:text-ink {tab === c.id
          ? 'border-ink! text-ink'
          : 'border-transparent! text-ink-2'}"
        onclick={() => (tab = c.id)}
        onkeydown={(e) => onKey(e, i)}
      >
        {c.label}
      </button>
    {/each}
  </div>

  <div class="grid gap-3.5 bg-surface p-3.5" id="mcp-panel" role="tabpanel" aria-labelledby="mcp-tab-{active.id}">
    {#each active.snippets as s, i (active.id + i)}
      {@const key = `${active.id}-${i}`}
      <div class="grid gap-1">
        <div class="flex items-center justify-between text-xs font-medium text-ink-2">
          <span>{s.label}</span>
          <button type="button" class="ghost sm" onclick={() => copy(s.code, key)} aria-label="Copy {s.label}">
            {#if copied === key}<Check size={13} /> Copied{:else}<Copy size={13} /> Copy{/if}
          </button>
        </div>
        <pre class="term"><code>{#if s.lang === 'sh'}<span class="text-term-dim select-none">$ </span>{/if}{s.code}</code></pre>
        {#if s.note}<p class="hint">{s.note}</p>{/if}
      </div>
    {/each}
  </div>
</div>

