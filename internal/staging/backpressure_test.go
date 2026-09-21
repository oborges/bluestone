package staging

import (
	"context"
	"errors"
	"io"
	"syscall"
	"testing"
	"time"

	"github.com/IBM/ibm-cos-sdk-go/service/s3"
	"github.com/oborges/bluestone/internal/config"
)

const mib = int64(1024 * 1024)

type noReadCOSClient struct{}

func (noReadCOSClient) PutObject(context.Context, string, []byte, map[string]string) error {
	return nil
}

func (noReadCOSClient) PutObjectStream(context.Context, string, io.ReadSeeker, map[string]string) error {
	return nil
}

func (noReadCOSClient) GetObjectStream(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(&emptyReader{}), nil
}

func (noReadCOSClient) CreateMultipartUpload(context.Context, string, map[string]string) (string, error) {
	return "test-upload", nil
}

func (noReadCOSClient) UploadPart(context.Context, string, string, int64, io.ReadSeeker) (string, error) {
	return "test-etag", nil
}

func (noReadCOSClient) CompleteMultipartUpload(context.Context, string, string, []*s3.CompletedPart) error {
	return nil
}

func (noReadCOSClient) AbortMultipartUpload(context.Context, string, string) error {
	return nil
}

func (noReadCOSClient) DeleteObject(context.Context, string) error {
	return nil
}

type emptyReader struct{}

func (*emptyReader) Read([]byte) (int, error) {
	return 0, io.EOF
}

func backpressureTestConfig(t *testing.T) *config.StagingConfig {
	t.Helper()

	return &config.StagingConfig{
		Enabled:                      true,
		RootDir:                      t.TempDir(),
		SyncInterval:                 "50ms",
		SyncThresholdMB:              1,
		MaxDirtyAge:                  "5m",
		MaxStagingSizeGB:             1,
		MaxDirtyFiles:                100,
		SyncWorkerCount:              1,
		SyncQueueSize:                10,
		MaxSyncRetries:               1,
		RetryBackoffInit:             "1ms",
		RetryBackoffMax:              "10ms",
		CleanAfterSync:               true,
		StaleFileAge:                 "24h",
		BackpressureEnabled:          true,
		BackpressureMode:             BackpressureModeBlock,
		BackpressureHighWatermarkPct: 80,
		BackpressureCritWatermarkPct: 95,
		BackpressureWaitTimeout:      "150ms",
		BackpressureCheckInterval:    "25ms",
	}
}

func TestStagingBackpressureBelowWatermarkSucceeds(t *testing.T) {
	manager, session := newBackpressureSession(t, "/below-watermark.bin")
	defer manager.Shutdown()

	session.Size = 100 * mib
	if _, err := session.Write([]byte("ok"), session.Size); err != nil {
		t.Fatalf("write below high watermark failed: %v", err)
	}

	if state := manager.CurrentPressure(); state.Level != PressureLevelNormal {
		t.Fatalf("pressure level = %s, want %s", state.Level, PressureLevelNormal)
	}
}

func TestStagingBackpressureAboveHighWatermarkBlocksThenFails(t *testing.T) {
	manager, session := newBackpressureSession(t, "/above-high.bin")
	defer manager.Shutdown()

	session.Size = 850 * mib
	start := time.Now()
	_, err := session.Write([]byte("x"), session.Size)
	elapsed := time.Since(start)

	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("write above high watermark error = %v, want ENOSPC", err)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("write returned too quickly: %s, want blocking behavior", elapsed)
	}
}

func TestStagingBackpressureAboveCriticalWatermarkFailsEarly(t *testing.T) {
	manager, session := newBackpressureSession(t, "/above-critical.bin")
	defer manager.Shutdown()

	session.Manager.config.BackpressureMode = BackpressureModeFailFast
	session.Size = 980 * mib
	start := time.Now()
	_, err := session.Write([]byte("x"), session.Size)
	elapsed := time.Since(start)

	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("write above critical watermark error = %v, want ENOSPC", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("critical write took %s, want fail-fast behavior", elapsed)
	}

	if stats := manager.Stats(); stats["staging_pressure_level"] != PressureLevelCritical {
		t.Fatalf("pressure level after rejection = %v, want %s", stats["staging_pressure_level"], PressureLevelCritical)
	}
}

func TestStagingBackpressureSyncDrainReleasesPressure(t *testing.T) {
	manager, session := newBackpressureSession(t, "/drain.bin")
	defer manager.Shutdown()

	session.Size = 850 * mib
	manager.ReleaseSession(session.Path)
	manager.MarkDirty(session.Path, session.Size)

	worker := NewSyncWorker(manager, noReadCOSClient{}, manager.config)
	if err := worker.syncFile(session.Path); err != nil {
		t.Fatalf("syncFile failed: %v", err)
	}

	if _, exists := manager.GetSession(session.Path); exists {
		t.Fatal("synced idle session still exists; pressure was not released")
	}
	if state := manager.CurrentPressure(); state.Level != PressureLevelNormal || state.UsedBytes != 0 {
		t.Fatalf("pressure after drain = level %s used %d, want normal/0", state.Level, state.UsedBytes)
	}

	next, err := manager.GetOrCreateSession("/after-drain.bin")
	if err != nil {
		t.Fatalf("create session after drain: %v", err)
	}
	if _, err := next.Write([]byte("ok"), 0); err != nil {
		t.Fatalf("write after sync drain failed: %v", err)
	}
}

func newBackpressureSession(t *testing.T, path string) (*StagingManager, *WriteSession) {
	t.Helper()

	cfg := backpressureTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}

	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		manager.Shutdown()
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}

	return manager, session
}

// While the bucket refuses writes for its quota, staging refuses writes too,
// rather than accepting data it cannot upload; shrinking a file, which frees
// space, is still allowed.
func TestStagingRefusesWritesWhileBucketFull(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()
	full := true
	manager.SetBucketFullCheck(func() bool { return full })

	if _, err := manager.ReserveWrite("/a.txt", 10, 10); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("write while the bucket is full: %v, want ENOSPC", err)
	}
	if release, err := manager.ReserveWrite("/a.txt", 0, 0); err != nil {
		t.Fatalf("shrinking while the bucket is full: %v", err)
	} else {
		release()
	}
	full = false
	release, err := manager.ReserveWrite("/a.txt", 10, 10)
	if err != nil {
		t.Fatalf("write once the bucket has room: %v", err)
	}
	release()
}
