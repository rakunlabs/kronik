package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/rakunlabs/ada"
)

// Live tells the browsers following a run that it changed, as soon as a
// replica says so on the bus. A browser then reads the run again.
type Live struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

func NewLive() *Live {
	return &Live{subs: map[string]map[chan struct{}]struct{}{}}
}

// Publish tells the followers of run id that it changed.
func (l *Live) Publish(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for ch := range l.subs[id] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (l *Live) subscribe(id string) (chan struct{}, func()) {
	ch := make(chan struct{}, 1)

	l.mu.Lock()
	if l.subs[id] == nil {
		l.subs[id] = map[chan struct{}]struct{}{}
	}

	l.subs[id][ch] = struct{}{}
	l.mu.Unlock()

	return ch, func() {
		l.mu.Lock()
		delete(l.subs[id], ch)

		if len(l.subs[id]) == 0 {
			delete(l.subs, id)
		}
		l.mu.Unlock()
	}
}

const (
	// livePing keeps proxies from closing a quiet stream.
	livePing = 20 * time.Second
	// liveCheck reads the run now and then even without a message, in case
	// the bus is down.
	liveCheck = 5 * time.Second
	// liveMax ends a stream; the browser opens a new one.
	liveMax = 10 * time.Minute
)

// runEvents streams a run as Server-Sent Events: an "run" event with the run
// (output included) whenever it changes, until it has finished.
func (s *Server) runEvents(c *ada.Context) error {
	id := c.Request.PathValue("id")
	ctx := c.Request.Context()

	run, err := s.svc.Store().GetRun(ctx, id)
	if err != nil {
		return apiErr(err)
	}

	w := c.Response
	rc := http.NewResponseController(w)
	// A stream lives longer than the server's write timeout.
	_ = rc.SetWriteDeadline(time.Time{})

	flush := func() error { return rc.Flush() }

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// nginx and friends must not buffer the stream.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if err := flush(); err != nil {
		return nil
	}

	changed, stop := s.live.subscribe(id)
	defer stop()

	send := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}

		if _, err := fmt.Fprintf(w, "event: run\ndata: %s\n\n", b); err != nil {
			return err
		}

		return flush()
	}

	if err := send(run); err != nil {
		return nil
	}

	last := run.Status + fmt.Sprint(len(run.Output), run.CancelRequested)

	ping := time.NewTicker(livePing)
	defer ping.Stop()

	check := time.NewTicker(liveCheck)
	defer check.Stop()

	end := time.After(liveMax)

	for !run.Finished() {
		select {
		case <-ctx.Done():
			return nil
		case <-end:
			return nil
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || flush() != nil {
				return nil
			}

			continue
		case <-changed:
		case <-check.C:
		}

		next, err := s.svc.Store().GetRun(ctx, id)
		if err != nil {
			return nil
		}

		key := next.Status + fmt.Sprint(len(next.Output), next.CancelRequested)
		if key == last && next.Output == run.Output {
			continue
		}

		run, last = next, key

		if err := send(run); err != nil {
			return nil
		}
	}

	return nil
}
