package cos

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/IBM/ibm-cos-sdk-go/aws/awserr"
)

// IBM COS answers a write to a bucket over its hard quota with 422
// BucketQuotaExceeded (checked against a real bucket).
func quotaFailure() error {
	return fmt.Errorf("failed to put object: %w", awserr.NewRequestFailure(
		awserr.New("BucketQuotaExceeded", "The specified bucket hard quota has been exceeded.", nil), 422, "req-1"))
}

func TestQuotaExceededIsNoSpace(t *testing.T) {
	c := &Client{bucket: "b"}
	err := c.noteWrite(quotaFailure())
	if !errors.Is(err, syscall.ENOSPC) || !errors.Is(err, ErrBucketQuotaExceeded) {
		t.Fatalf("quota refusal = %v, want it to match ENOSPC and ErrBucketQuotaExceeded", err)
	}
	if !c.BucketFull() {
		t.Fatal("bucket not marked full after a quota refusal")
	}

	// Other failures are left alone.
	other := c.noteWrite(awserr.New("InternalError", "boom", nil))
	if errors.Is(other, syscall.ENOSPC) {
		t.Fatal("an unrelated failure was reported as no space")
	}

	// A write that succeeds means the bucket has room.
	if err := c.noteWrite(nil); err != nil || c.BucketFull() {
		t.Fatalf("after a successful write: err %v, full %v", err, c.BucketFull())
	}
}

// The full state lapses, so a bucket whose quota was raised is tried again
// even if no write happens to clear it.
func TestBucketFullLapses(t *testing.T) {
	c := &Client{bucket: "b"}
	_ = c.noteWrite(quotaFailure())
	c.fullUntil.Store(time.Now().Add(-time.Second).UnixNano())
	if c.BucketFull() {
		t.Fatal("bucket still full after the state lapsed")
	}
}
