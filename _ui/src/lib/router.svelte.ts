// Hash router: #/<section>/<id>?query. The hash keeps deep links working
// without server-side routes and under any base path.

export type Route = { section: string; id: string; query: URLSearchParams }

function parse(): Route {
  const raw = location.hash.replace(/^#\/?/, '')
  const [path, qs = ''] = raw.split('?')
  const parts = path.split('/').filter(Boolean).map((s) => {
    try {
      return decodeURIComponent(s)
    } catch {
      return s
    }
  })

  return { section: parts[0] ?? 'jobs', id: parts.slice(1).join('/'), query: new URLSearchParams(qs) }
}

export const router = $state({ route: parse() })

// nav tells whether the last route change came from Back/Forward, so the
// page can keep its scroll position.
export const nav = { fromHistory: false }

// A guard can refuse to leave the page, e.g. a form with unsaved changes.
let guard: (() => boolean) | null = null
let current = location.hash

export function setGuard(fn: (() => boolean) | null) {
  guard = fn
}

window.addEventListener('popstate', () => {
  nav.fromHistory = true
})

window.addEventListener('hashchange', () => {
  if (guard && !guard()) {
    // Put the page's address back without another hashchange.
    history.pushState(history.state, '', current)
    nav.fromHistory = false
    return
  }
  current = location.hash
  router.route = parse()
})

export function href(section: string, id = '', query?: Record<string, string>) {
  let h = `#/${section}`
  if (id) h += `/${encodeURIComponent(id)}`
  if (query) {
    const q = new URLSearchParams(Object.entries(query).filter(([, v]) => v !== '')).toString()
    if (q) h += `?${q}`
  }
  return h
}

export function go(section: string, id = '', query?: Record<string, string>) {
  nav.fromHistory = false
  location.hash = href(section, id, query)
}

// replace changes the address without a new history entry and without
// remounting the page, e.g. for a filter typed into a search box.
export function replace(section: string, id = '', query?: Record<string, string>) {
  const h = href(section, id, query)
  if (h === location.hash) return
  history.replaceState(history.state, '', h)
  current = location.hash
  router.route = parse()
}

// open goes to a page, replacing the current history entry (after a form is
// saved, Back should not return to the form).
export function open(section: string, id = '', query?: Record<string, string>) {
  nav.fromHistory = false
  location.replace(href(section, id, query))
}
