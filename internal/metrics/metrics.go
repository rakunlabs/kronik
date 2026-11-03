// Package metrics records kronik's metrics through OpenTelemetry: the meter
// provider tell sets up, which sends them to the collector. With no
// collector (or metrics turned off in the settings) nothing is recorded.
package metrics

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/rakunlabs/kronik/internal/store"
)

// Options are read on every record.
type Options struct {
	Enabled bool
	PerJob  bool
}

// Metrics records runs, notifications and the state of jobs and the queue.
type Metrics struct {
	options func() Options
	node    string

	runs          metric.Int64Counter
	duration      metric.Float64Histogram
	notifications metric.Int64Counter

	stats atomic.Pointer[gauges]
}

// gauges are read from the database on a timer by one place and observed
// by the callback.
type gauges struct {
	jobs, enabled, failing, queued, running int64
}

// New registers the instruments on mp. options may turn recording off at
// any time.
func New(mp metric.MeterProvider, node string, options func() Options) (*Metrics, error) {
	meter := mp.Meter("github.com/rakunlabs/kronik")

	m := &Metrics{options: options, node: node}

	var err error

	if m.runs, err = meter.Int64Counter("kronik.runs", metric.WithDescription("Runs that ended, by status"), metric.WithUnit("{run}")); err != nil {
		return nil, err
	}

	if m.duration, err = meter.Float64Histogram("kronik.run.duration", metric.WithDescription("How long runs took"), metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.1, 0.5, 1, 5, 10, 30, 60, 300, 900, 1800, 3600)); err != nil {
		return nil, err
	}

	if m.notifications, err = meter.Int64Counter("kronik.notifications", metric.WithDescription("Notifications sent or failed"),
		metric.WithUnit("{notification}")); err != nil {
		return nil, err
	}

	jobs, err := meter.Int64ObservableGauge("kronik.jobs", metric.WithDescription("Jobs, by state: active, paused, failing"))
	if err != nil {
		return nil, err
	}

	queue, err := meter.Int64ObservableGauge("kronik.queue", metric.WithDescription("Runs queued and running, all replicas"))
	if err != nil {
		return nil, err
	}

	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		g := m.stats.Load()
		if g == nil || !m.on().Enabled {
			return nil
		}

		o.ObserveInt64(jobs, g.enabled, metric.WithAttributes(attribute.String("state", "active")))
		o.ObserveInt64(jobs, g.jobs-g.enabled, metric.WithAttributes(attribute.String("state", "paused")))
		o.ObserveInt64(jobs, g.failing, metric.WithAttributes(attribute.String("state", "failing")))
		o.ObserveInt64(queue, g.queued, metric.WithAttributes(attribute.String("status", "queued")))
		o.ObserveInt64(queue, g.running, metric.WithAttributes(attribute.String("status", "running")))

		return nil
	}, jobs, queue)
	if err != nil {
		return nil, err
	}

	return m, nil
}

func (m *Metrics) on() Options {
	if m.options == nil {
		return Options{Enabled: true, PerJob: true}
	}

	return m.options()
}

// RunFinished records a run that ended.
func (m *Metrics) RunFinished(job *store.Job, run *store.Run, status string, d time.Duration) {
	o := m.on()
	if !o.Enabled {
		return
	}

	attrs := []attribute.KeyValue{
		attribute.String("status", status),
		attribute.String("executor", job.Executor),
		attribute.String("trigger", run.Trigger),
		attribute.String("node", m.node),
	}

	if o.PerJob {
		attrs = append(attrs, attribute.String("job", job.Name))
	}

	set := metric.WithAttributes(attrs...)
	ctx := context.Background()

	m.runs.Add(ctx, 1, set)
	m.duration.Record(ctx, d.Seconds(), set)
}

// NotificationSent records a notification sent or failed.
func (m *Metrics) NotificationSent(channelType string, ok bool) {
	if !m.on().Enabled {
		return
	}

	result := "sent"
	if !ok {
		result = "failed"
	}

	m.notifications.Add(context.Background(), 1, metric.WithAttributes(attribute.String("type", channelType), attribute.String("result", result)))
}

// Gauges is where the job and queue numbers come from.
type Gauges interface {
	Gauges(ctx context.Context) (jobs, enabled, failing, queued, running int64, err error)
}

// Watch reads the job and queue numbers every interval until ctx ends.
func (m *Metrics) Watch(ctx context.Context, src Gauges, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()

	for {
		if m.on().Enabled {
			var g gauges

			var err error

			g.jobs, g.enabled, g.failing, g.queued, g.running, err = src.Gauges(ctx)
			if err != nil {
				if ctx.Err() == nil {
					slog.Warn("read metrics", "error", err)
				}
			} else {
				m.stats.Store(&g)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
