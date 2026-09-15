package posix

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/oborges/bluestone/internal/cache"
	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

var registerMetricsOnce sync.Once

// requestCount reads filesystem_requests_total for one label set from the
// default registry.
func requestCount(t *testing.T, protocol, operation, status string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	for _, family := range families {
		if family.GetName() != "filesystem_requests_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := make(map[string]string)
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["protocol"] == protocol && labels["operation"] == operation && labels["status"] == status {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func TestOperationsRecordRequestOutcomeByProtocol(t *testing.T) {
	registerMetricsOnce.Do(metrics.Initialize)

	newOps := func(store ObjectStore) *OperationsHandler {
		metadataCache := cache.NewMetadataCache(&config.MetadataCacheConfig{
			Enabled:    true,
			TTLSeconds: 60,
			MaxEntries: 100,
		})
		return NewOperationsHandler(store, metadataCache, nil, &config.PerformanceConfig{
			MaxDirectoryEntries: 100,
			MaxFullObjectReadMB: 1,
		})
	}
	store := newFakeObjectStore()
	store.put("present.txt", []byte("x"), time.Unix(100, 0))
	ops := newOps(store)
	down := newOps(downObjectStore{})
	// A protocol name no other test uses keeps the counts exact.
	ctx := metrics.WithProtocol(context.Background(), "test-protocol")

	if _, err := ops.Stat(ctx, "/present.txt"); err != nil {
		t.Fatalf("Stat(present) error = %v", err)
	}
	if _, err := ops.Stat(ctx, "/missing.txt"); !os.IsNotExist(err) {
		t.Fatalf("Stat(missing) error = %v, want not-exist", err)
	}
	if _, err := down.Stat(ctx, "/unreachable.txt"); err == nil {
		t.Fatal("Stat() against a down backend succeeded")
	}
	if err := down.DeleteFile(ctx, "/unreachable.txt"); err == nil {
		t.Fatal("DeleteFile() against a down backend succeeded")
	}
	if _, err := ops.ListDirectory(ctx, "/"); err != nil {
		t.Fatalf("ListDirectory() error = %v", err)
	}

	for _, tc := range []struct {
		operation, status string
	}{
		{"stat", metrics.StatusSuccess},
		{"stat", metrics.StatusNotFound},
		{"stat", metrics.StatusError},
		{"delete", metrics.StatusError},
		{"readdir", metrics.StatusSuccess},
	} {
		if got := requestCount(t, "test-protocol", tc.operation, tc.status); got != 1 {
			t.Errorf("filesystem_requests_total{protocol=test-protocol,operation=%s,status=%s} = %v, want 1", tc.operation, tc.status, got)
		}
	}

	// Requests without a protocol are attributed to internal work.
	before := requestCount(t, metrics.ProtocolInternal, "stat", metrics.StatusSuccess)
	if _, err := ops.Stat(context.Background(), "/present.txt"); err != nil {
		t.Fatalf("Stat(no protocol) error = %v", err)
	}
	if got := requestCount(t, metrics.ProtocolInternal, "stat", metrics.StatusSuccess); got != before+1 {
		t.Fatalf("internal stat count = %v, want %v", got, before+1)
	}
}
