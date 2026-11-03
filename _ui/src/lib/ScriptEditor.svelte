<script lang="ts">
  import { onMount } from 'svelte'
  import { EditorState } from '@codemirror/state'
  import { EditorView, keymap, lineNumbers, highlightActiveLine, drawSelection, placeholder as cmPlaceholder } from '@codemirror/view'
  import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands'
  import { javascript, javascriptLanguage } from '@codemirror/lang-javascript'
  import { linter, lintGutter, type Diagnostic } from '@codemirror/lint'
  import { autocompletion, closeBrackets, closeBracketsKeymap, completionKeymap, type CompletionContext } from '@codemirror/autocomplete'
  import { bracketMatching, syntaxHighlighting, HighlightStyle, indentOnInput } from '@codemirror/language'
  import { tags as t } from '@lezer/highlight'
  import { api } from './api'

  // A JavaScript editor for js jobs. Problems are reported by the server,
  // which compiles with the same engine that runs the script.
  let {
    value,
    onchange,
    onrun,
    secrets = [],
    env = [],
    label = 'Script',
    placeholder = '',
  }: {
    value: string
    onchange: (v: string) => void
    onrun?: () => void
    secrets?: string[]
    env?: string[]
    label?: string
    placeholder?: string
  } = $props()

  let host: HTMLDivElement
  let view: EditorView | undefined
  // The latest names, read by the completion source.
  const names = { secrets: [] as string[], env: [] as string[] }
  $effect(() => {
    names.secrets = secrets
    names.env = env
  })

  const highlight = HighlightStyle.define([
    { tag: t.keyword, color: '#c49bff' },
    { tag: [t.string, t.special(t.string)], color: '#86d39e' },
    { tag: [t.number, t.bool, t.null], color: '#e3b05c' },
    { tag: t.comment, color: '#8b8b85', fontStyle: 'italic' },
    { tag: t.propertyName, color: '#8cc4f0' },
    { tag: [t.function(t.variableName), t.function(t.propertyName)], color: '#f2f65c' },
    { tag: t.variableName, color: '#e4e4df' },
    { tag: [t.operator, t.punctuation, t.bracket, t.separator], color: '#b4b4ae' },
  ])

  const globals = [
    { label: 'fetch', type: 'function', detail: '(url, {method, headers, body, insecure, redirect, timeout})', info: 'HTTP request; insecure: true skips the TLS check' },
    { label: 'console', type: 'variable', detail: 'log, warn, error' },
    { label: 'sleep', type: 'function', detail: '(ms)', info: 'await sleep(1000)' },
    { label: 'secrets', type: 'variable', detail: 'from the server config' },
    { label: 'env', type: 'variable', detail: "the job's env" },
    { label: 'job', type: 'variable', detail: 'id, name, runId, attempt, scheduledAt' },
  ]
  const members: Record<string, () => { label: string; type: string; detail?: string }[]> = {
    console: () => ['log', 'info', 'warn', 'error', 'debug'].map((l) => ({ label: l, type: 'method' })),
    job: () => ['id', 'name', 'runId', 'attempt', 'scheduledAt'].map((l) => ({ label: l, type: 'property' })),
    secrets: () => names.secrets.map((l) => ({ label: l, type: 'property', detail: 'secret' })),
    env: () => names.env.map((l) => ({ label: l, type: 'property', detail: 'env' })),
  }

  function complete(ctx: CompletionContext) {
    const member = ctx.matchBefore(/\b(console|job|secrets|env)\.\w*$/)
    if (member) {
      const [obj] = member.text.split('.')
      return { from: member.from + obj.length + 1, options: members[obj](), validFor: /^\w*$/ }
    }
    const word = ctx.matchBefore(/\w+$/)
    if (!word && !ctx.explicit) return null
    return { from: word ? word.from : ctx.pos, options: globals, validFor: /^\w*$/ }
  }

  const lint = linter(
    async (v) => {
      const src = v.state.doc.toString()
      if (!src.trim()) return []
      try {
        const { problems } = await api.checkScript(src)
        return problems.map((p): Diagnostic => {
          const line = v.state.doc.line(Math.min(Math.max(p.line, 1), v.state.doc.lines))
          const from = Math.min(line.from + Math.max(p.column - 1, 0), line.to)
          return { from, to: Math.min(from + 1, line.to) > from ? Math.min(from + 1, line.to) : from, severity: 'error', message: p.message }
        })
      } catch {
        return []
      }
    },
    { delay: 400 },
  )

  onMount(() => {
    view = new EditorView({
      parent: host,
      state: EditorState.create({
        doc: value,
        extensions: [
          lineNumbers(),
          history(),
          drawSelection(),
          highlightActiveLine(),
          indentOnInput(),
          bracketMatching(),
          closeBrackets(),
          javascript(),
          javascriptLanguage.data.of({ autocomplete: complete }),
          autocompletion({ activateOnTyping: true }),
          syntaxHighlighting(highlight),
          lint,
          lintGutter(),
          cmPlaceholder(placeholder),
          EditorState.tabSize.of(2),
          EditorView.lineWrapping,
          keymap.of([
            ...(onrun ? [{ key: 'Mod-Enter', run: () => (onrun(), true) }] : []),
            ...closeBracketsKeymap,
            ...completionKeymap,
            ...defaultKeymap,
            ...historyKeymap,
            indentWithTab,
          ]),
          EditorView.updateListener.of((u) => {
            if (u.docChanged) onchange(u.state.doc.toString())
          }),
          EditorView.contentAttributes.of({ 'aria-label': label }),
        ],
      }),
    })
    return () => view?.destroy()
  })

  // A value set from outside (loading a job, an example) replaces the text.
  $effect(() => {
    const v = value
    if (view && v !== view.state.doc.toString()) {
      view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: v } })
    }
  })
</script>

<div class="cm theme-dark" bind:this={host}></div>

<style>
  .cm {
    border: 1px solid #3b3b37;
    border-radius: var(--radius);
    background: var(--term-bg);
    overflow: hidden;
  }
  .cm:focus-within {
    outline: 2px solid var(--focus);
    outline-offset: -1px;
  }
  .cm :global(.cm-editor) {
    font: 13px/1.6 var(--mono);
    min-height: 220px;
    max-height: 560px;
    color: var(--term-text);
    background: var(--term-bg);
  }
  .cm :global(.cm-editor.cm-focused) {
    outline: none;
  }
  .cm :global(.cm-scroller) {
    font-family: var(--mono);
    min-height: 220px;
  }
  .cm :global(.cm-content) {
    padding: 8px 0;
    caret-color: #f2f65c;
  }
  .cm :global(.cm-line) {
    padding: 0 12px;
  }
  .cm :global(.cm-gutters) {
    background: #1b1b18;
    border-right: 1px solid #2c2c29;
    color: #6f6f69;
  }
  .cm :global(.cm-activeLine) {
    background: rgb(255 255 255 / 0.035);
  }
  .cm :global(.cm-activeLineGutter) {
    background: transparent;
    color: #e4e4df;
  }
  .cm :global(.cm-selectionBackground),
  .cm :global(.cm-focused .cm-selectionBackground) {
    background: rgb(233 239 90 / 0.25) !important;
  }
  .cm :global(.cm-cursor) {
    border-left-color: #f2f65c;
  }
  .cm :global(.cm-placeholder) {
    color: #6f6f69;
  }
  .cm :global(.cm-matchingBracket) {
    background: rgb(233 239 90 / 0.18);
    outline: 1px solid rgb(233 239 90 / 0.4);
  }
  .cm :global(.cm-lintRange-error) {
    background-image: none;
    text-decoration: underline wavy #f0817b;
    text-underline-offset: 3px;
  }
  .cm :global(.cm-lint-marker-error) {
    content: none;
    background: #f0817b;
    width: 8px;
    height: 8px;
    margin: 6px 3px 0;
  }
  :global(.cm-tooltip) {
    font: 12px/1.45 var(--font);
    background: #1c1c19 !important;
    border: 1px solid #3b3b37 !important;
    border-radius: var(--radius);
    color: #e4e4df;
    box-shadow: 0 8px 24px -8px rgb(0 0 0 / 0.5);
  }
  :global(.cm-tooltip-autocomplete ul li[aria-selected]) {
    background: #2f2f2a !important;
    color: #e4e4df !important;
  }
  :global(.cm-completionDetail) {
    color: #8b8b85;
    font-style: normal !important;
    margin-left: 8px;
  }
  :global(.cm-diagnostic-error) {
    border-left-color: #f0817b !important;
  }
</style>
