// A confirmation asked in the app's own dialog instead of window.confirm.
export type Confirm = {
  title: string
  body: string
  action: string
  danger: boolean
  resolve: (ok: boolean) => void
}

export const pending = $state<{ current: Confirm | null }>({ current: null })

export function confirm(opts: { title: string; body: string; action?: string; danger?: boolean }): Promise<boolean> {
  pending.current?.resolve(false)
  return new Promise((resolve) => {
    pending.current = { action: 'Confirm', danger: false, ...opts, resolve }
  })
}

export function answer(ok: boolean) {
  const c = pending.current
  pending.current = null
  c?.resolve(ok)
}
