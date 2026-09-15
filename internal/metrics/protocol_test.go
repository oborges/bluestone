package metrics

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func counterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	var metric dto.Metric
	if err := counter.Write(&metric); err != nil {
		t.Fatalf("counter.Write() error = %v", err)
	}
	return metric.GetCounter().GetValue()
}

func TestProtocolFromContext(t *testing.T) {
	if got := ProtocolFrom(context.Background()); got != ProtocolInternal {
		t.Fatalf("ProtocolFrom(background) = %q, want %q", got, ProtocolInternal)
	}
	if got := ProtocolFrom(nil); got != ProtocolInternal {
		t.Fatalf("ProtocolFrom(nil) = %q, want %q", got, ProtocolInternal)
	}
	ctx := WithProtocol(context.Background(), ProtocolNFS)
	if got := ProtocolFrom(ctx); got != ProtocolNFS {
		t.Fatalf("ProtocolFrom(nfs) = %q, want %q", got, ProtocolNFS)
	}
	if got := ProtocolFrom(WithProtocol(ctx, "")); got != ProtocolNFS {
		t.Fatalf("empty protocol must not override: got %q", got)
	}
}

func TestRecordRequestLabelsProtocolAndOutcome(t *testing.T) {
	op := "test-" + t.Name()
	nfsCtx := WithProtocol(context.Background(), ProtocolNFS)

	RecordRequest(nfsCtx, op, nil, time.Millisecond)
	RecordRequest(nfsCtx, op, fmt.Errorf("stat /x: %w", os.ErrNotExist), time.Millisecond)
	RecordRequest(context.Background(), op, errors.New("dial tcp: connection refused"), time.Millisecond)

	for _, tc := range []struct {
		protocol, status string
	}{
		{ProtocolNFS, StatusSuccess},
		{ProtocolNFS, StatusNotFound},
		{ProtocolInternal, StatusError},
	} {
		if got := counterValue(t, filesystemRequestsTotal.WithLabelValues(tc.protocol, op, tc.status)); got != 1 {
			t.Errorf("filesystem_requests_total{protocol=%q,status=%q} = %v, want 1", tc.protocol, tc.status, got)
		}
	}

	// The deprecated metric keeps counting every request, without protocol.
	for _, status := range []string{StatusSuccess, StatusNotFound, StatusError} {
		if got := counterValue(t, nfsRequestsTotal.WithLabelValues(op, status)); got != 1 {
			t.Errorf("nfs_requests_total{status=%q} = %v, want 1", status, got)
		}
	}
}
