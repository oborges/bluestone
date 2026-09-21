package cos

import (
	"errors"
	"net/http"
	"syscall"
	"time"

	"github.com/IBM/ibm-cos-sdk-go/aws/awserr"
	"github.com/oborges/bluestone/internal/logging"
	"github.com/oborges/bluestone/internal/metrics"
	"go.uber.org/zap"
)

// A bucket with a hard quota refuses writes once it is full: IBM COS
// answers 422 BucketQuotaExceeded to every upload, copy, metadata update and
// multipart part, while reads and deletes still work. Enforcement trails the
// bucket's usage by a few minutes, so some writes past the quota succeed.

// ErrBucketQuotaExceeded reports a write the bucket refused for its quota.
// Errors carrying it also match syscall.ENOSPC, so they reach clients as
// "no space left" (NFS) and STATUS_DISK_FULL (SMB).
var ErrBucketQuotaExceeded = errors.New("cos: bucket hard quota exceeded")

type quotaError struct{ err error }

func (e quotaError) Error() string   { return "bucket hard quota exceeded: " + e.err.Error() }
func (e quotaError) Unwrap() []error { return []error{ErrBucketQuotaExceeded, syscall.ENOSPC, e.err} }

// bucketFullFor is how long the bucket counts as full after a write is
// refused. Only a write that succeeds shows the bucket has room again: a
// delete does not, since the bucket's usage is counted minutes behind, and
// one delete may not bring it under the quota anyway. Uploads of staged files
// are retried and clear the state when they succeed, but a refused folder or
// metadata change leaves nothing to retry, so the state lapses and the next
// write finds out. Checking sooner than COS counts usage would gain nothing.
const bucketFullFor = 2 * time.Minute

// isQuotaExceeded reports whether err is the object store refusing a write
// because the bucket is over its hard quota.
func isQuotaExceeded(err error) bool {
	var aerr awserr.Error
	if !errors.As(err, &aerr) {
		return false
	}
	switch aerr.Code() {
	case "BucketQuotaExceeded", "QuotaExceeded":
		return true
	}
	var failure awserr.RequestFailure
	return errors.As(err, &failure) && failure.StatusCode() == http.StatusUnprocessableEntity &&
		aerr.Code() == "BucketQuotaExceeded"
}

// noteWrite records how a write went: a write refused for the quota marks
// the bucket full and comes back matching ENOSPC; a write that succeeds
// clears the state.
func (c *Client) noteWrite(err error) error {
	if err == nil {
		c.setBucketFull(false)
		return nil
	}
	if isQuotaExceeded(err) {
		c.setBucketFull(true)
		return quotaError{err}
	}
	return err
}

func (c *Client) setBucketFull(full bool) {
	if full {
		was := c.BucketFull()
		c.fullUntil.Store(time.Now().Add(bucketFullFor).UnixNano())
		if !was {
			logging.Warn("Bucket is over its hard quota: refusing new writes until it has room",
				zap.String("bucket", c.bucket))
			metrics.SetCOSBucketFull(true)
		}
		return
	}
	if c.fullUntil.Swap(0) != 0 {
		logging.Info("Bucket has room again", zap.String("bucket", c.bucket))
		metrics.SetCOSBucketFull(false)
	}
}

// BucketFull reports whether the bucket recently refused a write for its
// quota. Staging refuses new writes while it is, rather than accepting data
// it cannot upload, and free space is reported as none.
func (c *Client) BucketFull() bool {
	until := c.fullUntil.Load()
	return until != 0 && time.Now().UnixNano() < until
}
