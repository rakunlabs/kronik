// Short messages in the corner, e.g. "Run queued".
export type Toast = { id: number; text: string; href?: string; link?: string; tone: 'ok' | 'danger' }

export const toasts = $state<Toast[]>([])

let next = 1

export function toast(text: string, opts: { href?: string; link?: string; tone?: Toast['tone'] } = {}) {
  const t: Toast = { id: next++, text, tone: opts.tone ?? 'ok', href: opts.href, link: opts.link }
  toasts.push(t)
  setTimeout(() => dismiss(t.id), 5000)
}

export function dismiss(id: number) {
  const i = toasts.findIndex((t) => t.id === id)
  if (i >= 0) toasts.splice(i, 1)
}
