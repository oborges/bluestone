package cos

import (
	"testing"

	"github.com/oborges/bluestone/internal/config"
)

func TestNewClientStartsDegradedWhenCOSUnreachable(t *testing.T) {
	// An unreachable object store must not prevent client construction:
	// the gateway serves cached reads and staged writes during a COS outage,
	// and crash recovery must work while COS is still down.
	cfg := &config.COSConfig{
		// Reserved TEST-NET-1 address: connection will fail fast or time out.
		Endpoint:   "https://192.0.2.1",
		Bucket:     "unreachable-bucket",
		Region:     "test",
		AuthType:   "hmac",
		AccessKey:  "test-access",
		SecretKey:  "test-secret",
		MaxRetries: 1,
		Timeout:    "2s",
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient() with unreachable COS error = %v, want degraded-mode startup", err)
	}
	if client == nil {
		t.Fatal("NewClient() returned nil client")
	}
	client.Close()
}

// Made with Bob
