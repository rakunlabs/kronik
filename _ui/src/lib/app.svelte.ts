import { api, type Info } from './api'

// The server's info, loaded once and shared. A failed load is retried, so a
// page opened while the server restarts fills in once it is back.
export const app = $state<{ info: Info | null }>({ info: null })

let delay = 1000

function load() {
  api
    .info()
    .then((i) => (app.info = i))
    .catch(() => {
      setTimeout(load, delay)
      delay = Math.min(delay * 2, 30_000)
    })
}

load()
