package metrics

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rakunlabs/kronik/internal/store"
)

type fixed struct{}

func (fixed) Gauges(context.Context) (int64, int64, int64, int64, int64, error) {
	return 5, 4, 1, 2, 3, nil
}

func TestMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	opts := Options{Enabled: true, PerJob: true}

	m, err := New(mp, "n1", func() Options { return opts })
	if err != nil {
		t.Fatal(err)
	}

	job := &store.Job{Name: "backup", Executor: "http"}
	m.RunFinished(job, &store.Run{Trigger: "schedule"}, "failed", 2*time.Second)
	m.NotificationSent("slack", true)

	ctx, cancel := context.WithCancel(context.Background())
	go m.Watch(ctx, fixed{}, time.Hour)
	time.Sleep(50 * time.Millisecond)
	cancel()

	opts.Enabled = false
	m.RunFinished(job, &store.Run{}, "succeeded", time.Second)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}

	opts.Enabled = true

	found := map[string]bool{}

	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			found[mt.Name] = true

			if mt.Name == "kronik.runs" {
				sum := mt.Data.(metricdata.Sum[int64])
				if len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != 1 {
					t.Fatalf("runs: %+v", sum.DataPoints)
				}

				if v, _ := sum.DataPoints[0].Attributes.Value(attribute.Key("job")); v.AsString() != "backup" {
					t.Fatalf("job attribute: %v", v)
				}
			}
		}
	}

	for _, want := range []string{"kronik.runs", "kronik.run.duration", "kronik.notifications"} {
		if !found[want] {
			t.Errorf("no %s in %v", want, found)
		}
	}
}
