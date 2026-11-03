import { untrack } from 'svelte'

// A ticking clock shared by components showing relative times.
export const clock = $state({ now: Date.now() })

setInterval(() => {
  clock.now = Date.now()
}, 1000)

// poll calls fn now and then every ms while the page is visible, and right
// away when the page becomes visible again. It returns a stop function for
// $effect. Stopping aborts the signal handed to fn, so a reply that arrives
// after the page moved on (another filter, another page) can be dropped:
// callers check signal.aborted before using a result. fn runs untracked, so
// the state it reads does not restart the effect that started the poll.
export function poll(fn: (signal: AbortSignal) => unknown, ms: number): () => void {
  const ctl = new AbortController()
  let running = false
  let timer: ReturnType<typeof setTimeout> | undefined

  const tick = async () => {
    clearTimeout(timer)
    if (ctl.signal.aborted || running) return
    if (document.visibilityState === 'visible') {
      running = true
      try {
        await untrack(() => fn(ctl.signal))
      } catch {
        // The caller shows its own errors; the poll keeps going.
      } finally {
        running = false
      }
    }
    if (!ctl.signal.aborted) timer = setTimeout(tick, ms)
  }

  const onVisible = () => {
    if (document.visibilityState === 'visible') tick()
  }

  document.addEventListener('visibilitychange', onVisible)
  tick()

  return () => {
    ctl.abort()
    clearTimeout(timer)
    document.removeEventListener('visibilitychange', onVisible)
  }
}

// debounced follows get() after it has been still for ms. Values are
// compared by content, so returning a new object with the same fields does
// not count as a change.
export function debounced<T>(get: () => T, ms: number): { readonly value: T } {
  const initial = untrack(get)
  let value = $state(initial)
  let key = JSON.stringify(initial)

  $effect(() => {
    const next = get()
    const nextKey = JSON.stringify(next)
    if (nextKey === key) return
    const t = setTimeout(() => {
      key = nextKey
      value = next
    }, ms)
    return () => clearTimeout(t)
  })

  return {
    get value() {
      return value
    },
  }
}
