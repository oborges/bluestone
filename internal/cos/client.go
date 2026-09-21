package cos

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/IBM/ibm-cos-sdk-go/aws"
	"github.com/IBM/ibm-cos-sdk-go/aws/awserr"
	"github.com/IBM/ibm-cos-sdk-go/aws/credentials"
	"github.com/IBM/ibm-cos-sdk-go/aws/credentials/ibmiam"
	"github.com/IBM/ibm-cos-sdk-go/aws/session"
	"github.com/IBM/ibm-cos-sdk-go/service/s3"
	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/internal/logging"
	"github.com/oborges/bluestone/pkg/types"
	"go.uber.org/zap"
)

// Client wraps the IBM Cloud COS SDK client
type Client struct {
	s3Client *s3.S3
	bucket   string
	config   *config.COSConfig
}

// NewClient creates a new COS client
func NewClient(cfg *config.COSConfig) (*Client, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}

	// Create AWS config
	awsConfig := &aws.Config{
		Endpoint: aws.String(cfg.Endpoint),
		Region:   aws.String(cfg.Region),
	}

	// Set timeout - use longer timeout for large file operations
	timeout, err := cfg.GetTimeout()
	if err != nil {
		return nil, fmt.Errorf("invalid timeout: %w", err)
	}

	// For large files, we need a much longer timeout
	// Default is 30s, but large files need more time
	if timeout < 5*time.Minute {
		timeout = 5 * time.Minute
	}

	var transport http.RoundTripper = &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 10 * time.Second,
			// TCP keepalives hold NAT/load-balancer state for pooled
			// connections so idle-but-open connections stay usable.
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        128,
		MaxIdleConnsPerHost: 128,
		// Longer than the default 90s: bursty gateway workloads with
		// idle gaps otherwise drain the pool and pay TLS handshakes on
		// the next burst.
		IdleConnTimeout:       4 * time.Minute,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true, // Disable compression for better performance
		// The 4KB defaults throttle large object transfers; sync
		// uploads and range reads move MBs per request.
		WriteBufferSize: 128 << 10,
		ReadBufferSize:  128 << 10,
		TLSClientConfig: &tls.Config{
			// Session resumption cuts reconnect handshakes to one round
			// trip when connections are re-established after idling out
			// or during burst fan-out beyond the warm pool.
			ClientSessionCache: tls.NewLRUClientSessionCache(256),
		},
	}

	// Fail fast during established outages instead of paying dial/retry
	// timeouts on every operation; the gateway's staging and stale-cache
	// fallbacks then answer immediately.
	if cfg.CircuitBreakerEnabled == nil || *cfg.CircuitBreakerEnabled {
		transport = newBreakerTransport(transport)
	}

	awsConfig.HTTPClient = &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}

	// Configure authentication
	var creds *credentials.Credentials
	switch cfg.AuthType {
	case "iam":
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("api_key is required for IAM authentication")
		}
		creds = ibmiam.NewStaticCredentials(
			aws.NewConfig(),
			"https://iam.cloud.ibm.com/identity/token",
			cfg.APIKey,
			cfg.ServiceID,
		)
	case "hmac":
		if cfg.AccessKey == "" || cfg.SecretKey == "" {
			return nil, fmt.Errorf("access_key and secret_key are required for HMAC authentication")
		}
		creds = credentials.NewStaticCredentials(
			cfg.AccessKey,
			cfg.SecretKey,
			"",
		)
	default:
		return nil, fmt.Errorf("invalid auth_type: %s", cfg.AuthType)
	}

	awsConfig.Credentials = creds

	// Create session
	sess, err := session.NewSession(awsConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	// Create S3 client
	s3Client := s3.New(sess)

	client := &Client{
		s3Client: s3Client,
		bucket:   cfg.Bucket,
		config:   cfg,
	}

	// Verify connectivity. An unreachable COS must not prevent startup: the
	// gateway can serve cached reads and accept staged writes during an
	// object-store outage, and crash recovery must be able to bring the
	// export back while COS is still down. Misconfiguration surfaces here
	// too, so log loudly either way; operations retry until COS responds.
	if err := client.ping(); err != nil {
		logging.Error("COS not reachable at startup; starting in degraded mode. "+
			"Staged writes and cached reads continue; sync retries until COS responds. "+
			"If this persists, verify endpoint, bucket, and credentials.",
			zap.String("endpoint", cfg.Endpoint),
			zap.String("bucket", cfg.Bucket),
			zap.Error(err))
	} else {
		logging.Info("COS client initialized",
			zap.String("endpoint", cfg.Endpoint),
			zap.String("bucket", cfg.Bucket),
			zap.String("region", cfg.Region),
		)
	}

	return client, nil
}

// ping verifies connectivity to COS
func (c *Client) ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := c.s3Client.HeadBucketWithContext(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(c.bucket),
	})
	if err != nil {
		return fmt.Errorf("bucket not accessible: %w", err)
	}

	return nil
}

// GetObject retrieves an object from COS with retry logic
func (c *Client) GetObject(ctx context.Context, key string) ([]byte, error) {
	log := logging.WithOperation("GetObject").With(zap.String("key", key))
	log.Debug("getting object")

	var lastErr error
	maxRetries := c.config.MaxRetries
	if maxRetries == 0 {
		maxRetries = 3
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt*attempt) * time.Second
			log.Warn("retrying GetObject", zap.Int("attempt", attempt), zap.Duration("backoff", backoff))
			time.Sleep(backoff)
		}

		data, err := c.getObjectAttempt(ctx, key)
		if err == nil {
			if attempt > 0 {
				log.Info("GetObject succeeded after retry", zap.Int("attempt", attempt))
			}
			return data, nil
		}

		lastErr = err

		// Don't retry on certain errors
		if isCircuitOpenError(err) || isNotFoundError(err) || strings.Contains(err.Error(), "too large") {
			break
		}
	}

	if isNotFoundError(lastErr) {
		// Typed miss so callers can distinguish a missing object from an
		// unreachable object store.
		return nil, fmt.Errorf("object %s: %w", key, os.ErrNotExist)
	}
	log.Error("GetObject failed after retries", zap.Error(lastErr), zap.Int("maxRetries", maxRetries))
	return nil, lastErr
}

// getObjectAttempt performs a single attempt to get an object
func (c *Client) getObjectAttempt(ctx context.Context, key string) ([]byte, error) {
	log := logging.WithOperation("GetObject").With(zap.String("key", key))

	input := &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}

	result, err := c.s3Client.GetObjectWithContext(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to get object: %w", err)
	}
	defer result.Body.Close()

	// Pre-allocate buffer if content length is known
	var data []byte
	var totalSize int64
	if result.ContentLength != nil && *result.ContentLength > 0 {
		totalSize = *result.ContentLength
		// Sanity check: don't allocate more than 5GB at once
		if totalSize > 5*1024*1024*1024 {
			log.Warn("object too large for single read", zap.Int64("size", totalSize))
			return nil, fmt.Errorf("object too large: %d bytes (max 5GB)", totalSize)
		}
		data = make([]byte, 0, totalSize)
		log.Info("Starting object download",
			zap.String("key", key),
			zap.Int64("totalSize", totalSize),
			zap.String("sizeMB", fmt.Sprintf("%.2f MB", float64(totalSize)/(1024*1024))))
	}

	// Use a buffer to read in chunks to handle large files better
	buf := make([]byte, 128*1024) // 128KB buffer for better performance
	totalRead := 0
	lastLogTime := time.Now()
	lastLogBytes := 0

	for {
		n, err := result.Body.Read(buf)
		if n > 0 {
			data = append(data, buf[:n]...)
			totalRead += n

			// Log progress every 10MB or every 5 seconds
			if totalRead-lastLogBytes >= 10*1024*1024 || time.Since(lastLogTime) >= 5*time.Second {
				percentComplete := float64(0)
				if totalSize > 0 {
					percentComplete = float64(totalRead) / float64(totalSize) * 100
				}

				throughputMBps := float64(totalRead-lastLogBytes) / (1024 * 1024) / time.Since(lastLogTime).Seconds()

				log.Info("Download progress",
					zap.String("key", key),
					zap.Int("bytesRead", totalRead),
					zap.String("readMB", fmt.Sprintf("%.2f MB", float64(totalRead)/(1024*1024))),
					zap.String("progress", fmt.Sprintf("%.1f%%", percentComplete)),
					zap.String("throughput", fmt.Sprintf("%.2f MB/s", throughputMBps)))

				lastLogTime = time.Now()
				lastLogBytes = totalRead
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Error("failed to read object body",
				zap.Error(err),
				zap.Int("bytesRead", totalRead),
				zap.String("readMB", fmt.Sprintf("%.2f MB", float64(totalRead)/(1024*1024))),
				zap.String("key", key))
			return nil, fmt.Errorf("failed to read object body: %w", err)
		}
	}

	log.Info("Object download complete",
		zap.String("key", key),
		zap.Int("totalBytes", len(data)),
		zap.String("sizeMB", fmt.Sprintf("%.2f MB", float64(len(data))/(1024*1024))))
	return data, nil
}

// GetObjectRange retrieves a range of bytes from an object
func (c *Client) GetObjectRange(ctx context.Context, key string, offset, length int64) ([]byte, error) {
	log := logging.WithOperation("GetObjectRange").With(
		zap.String("key", key),
		zap.Int64("offset", offset),
		zap.Int64("length", length),
	)

	if length > 10*1024*1024 { // Log for ranges > 10MB
		log.Info("Starting range download",
			zap.String("key", key),
			zap.Int64("offset", offset),
			zap.Int64("length", length),
			zap.String("sizeMB", fmt.Sprintf("%.2f MB", float64(length)/(1024*1024))))
	} else {
		log.Debug("getting object range")
	}

	rangeHeader := fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)

	input := &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
		Range:  aws.String(rangeHeader),
	}

	result, err := c.s3Client.GetObjectWithContext(ctx, input)
	if err != nil {
		log.Error("failed to get object range", zap.Error(err))
		return nil, fmt.Errorf("failed to get object range: %w", err)
	}
	defer result.Body.Close()

	// Pre-allocate buffer for expected length
	data := make([]byte, 0, length)

	// Use buffered reading for better performance and error handling
	buf := make([]byte, 128*1024) // 128KB buffer
	totalRead := 0
	lastLogTime := time.Now()

	for {
		n, err := result.Body.Read(buf)
		if n > 0 {
			data = append(data, buf[:n]...)
			totalRead += n

			// Log progress for large ranges every 5 seconds
			if length > 10*1024*1024 && time.Since(lastLogTime) >= 5*time.Second {
				percentComplete := float64(totalRead) / float64(length) * 100
				log.Info("Range download progress",
					zap.String("key", key),
					zap.Int("bytesRead", totalRead),
					zap.String("progress", fmt.Sprintf("%.1f%%", percentComplete)))
				lastLogTime = time.Now()
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Error("failed to read object body",
				zap.Error(err),
				zap.Int("bytesRead", totalRead),
				zap.String("readMB", fmt.Sprintf("%.2f MB", float64(totalRead)/(1024*1024))))
			return nil, fmt.Errorf("failed to read object body: %w", err)
		}
	}

	if length > 10*1024*1024 {
		log.Info("Range download complete",
			zap.String("key", key),
			zap.Int("totalBytes", len(data)),
			zap.String("sizeMB", fmt.Sprintf("%.2f MB", float64(len(data))/(1024*1024))))
	} else {
		log.Debug("object range retrieved", zap.Int("size", len(data)))
	}
	return data, nil
}

// PutObject uploads an object to COS
func (c *Client) PutObject(ctx context.Context, key string, data []byte, metadata map[string]string) error {
	log := logging.WithOperation("PutObject").With(
		zap.String("key", key),
		zap.Int("size", len(data)),
	)
	log.Debug("putting object")

	// Convert metadata to AWS format
	awsMetadata := make(map[string]*string)
	for k, v := range metadata {
		awsMetadata[k] = aws.String(v)
	}

	input := &s3.PutObjectInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(key),
		Body:     bytes.NewReader(data),
		Metadata: awsMetadata,
	}

	_, err := c.s3Client.PutObjectWithContext(ctx, input)
	if err != nil {
		log.Error("failed to put object", zap.Error(err))
		return fmt.Errorf("failed to put object: %w", err)
	}

	log.Debug("object uploaded")
	return nil
}

// PutObjectStream uploads an object to COS from an io.ReadSeeker
func (c *Client) PutObjectStream(ctx context.Context, key string, body io.ReadSeeker, metadata map[string]string) error {
	log := logging.WithOperation("PutObjectStream").With(
		zap.String("key", key),
	)
	log.Debug("putting object stream")

	awsMetadata := make(map[string]*string)
	for k, v := range metadata {
		awsMetadata[k] = aws.String(v)
	}

	input := &s3.PutObjectInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(key),
		Body:     body,
		Metadata: awsMetadata,
	}

	_, err := c.s3Client.PutObjectWithContext(ctx, input)
	if err != nil {
		log.Error("failed to put object stream", zap.Error(err))
		return fmt.Errorf("failed to put object stream: %w", err)
	}

	log.Debug("object stream uploaded")
	return nil
}

// GetObjectStream streams an object from COS
func (c *Client) GetObjectStream(ctx context.Context, key string) (io.ReadCloser, error) {
	input := &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}
	result, err := c.s3Client.GetObjectWithContext(ctx, input)
	if err != nil {
		return nil, err
	}
	return result.Body, nil
}

// DeleteObject deletes an object from COS
func (c *Client) DeleteObject(ctx context.Context, key string) error {
	log := logging.WithOperation("DeleteObject").With(zap.String("key", key))
	log.Debug("deleting object")

	input := &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}

	_, err := c.s3Client.DeleteObjectWithContext(ctx, input)
	if err != nil {
		log.Error("failed to delete object", zap.Error(err))
		return fmt.Errorf("failed to delete object: %w", err)
	}

	log.Debug("object deleted")
	return nil
}

// HeadObject retrieves object metadata
func (c *Client) HeadObject(ctx context.Context, key string) (*types.ObjectMetadata, error) {
	log := logging.WithOperation("HeadObject").With(zap.String("key", key))
	log.Debug("getting object metadata")

	input := &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}

	result, err := c.s3Client.HeadObjectWithContext(ctx, input)
	if err != nil {
		log.Debug("object not found or error", zap.Error(err))
		if isNotFoundError(err) {
			// Typed miss so callers can distinguish "object does not exist"
			// from "object store unreachable"; conflating them turns backend
			// outages into false ENOENT answers.
			return nil, fmt.Errorf("object %s: %w", key, os.ErrNotExist)
		}
		return nil, fmt.Errorf("failed to get object metadata: %w", err)
	}

	// Convert metadata. Keys are lowercased: header names are
	// case-insensitive, and the SDK otherwise returns them canonicalized
	// ("Mode" for x-amz-meta-mode), which callers would not match.
	metadata := make(map[string]string)
	for k, v := range result.Metadata {
		if v != nil {
			metadata[strings.ToLower(k)] = *v
		}
	}

	objectMeta := &types.ObjectMetadata{
		Key:          key,
		Size:         aws.Int64Value(result.ContentLength),
		LastModified: aws.TimeValue(result.LastModified),
		ETag:         aws.StringValue(result.ETag),
		ContentType:  aws.StringValue(result.ContentType),
		Metadata:     metadata,
	}

	log.Debug("object metadata retrieved", zap.Int64("size", objectMeta.Size))
	return objectMeta, nil
}

// ListObjects lists objects with a given prefix up to maxKeys limit (or all if maxKeys <= 0)
func (c *Client) ListObjects(ctx context.Context, prefix string, maxKeys int) ([]*types.ObjectMetadata, error) {
	log := logging.WithOperation("ListObjects").With(
		zap.String("prefix", prefix),
		zap.Int("maxKeys", maxKeys),
	)
	log.Debug("listing objects")

	var objects []*types.ObjectMetadata
	var continuationToken *string

	reqMaxKeys := int64(1000)
	if maxKeys > 0 && maxKeys < 1000 {
		reqMaxKeys = int64(maxKeys)
	}

	for {
		input := &s3.ListObjectsV2Input{
			Bucket:            aws.String(c.bucket),
			Prefix:            aws.String(prefix),
			MaxKeys:           aws.Int64(reqMaxKeys),
			ContinuationToken: continuationToken,
		}

		result, err := c.s3Client.ListObjectsV2WithContext(ctx, input)
		if err != nil {
			log.Error("failed to list objects", zap.Error(err))
			return nil, fmt.Errorf("failed to list objects: %w", err)
		}

		for _, obj := range result.Contents {
			objects = append(objects, &types.ObjectMetadata{
				Key:          aws.StringValue(obj.Key),
				Size:         aws.Int64Value(obj.Size),
				LastModified: aws.TimeValue(obj.LastModified),
				ETag:         aws.StringValue(obj.ETag),
			})
			if maxKeys > 0 && len(objects) >= maxKeys {
				log.Debug("objects listed", zap.Int("count", len(objects)))
				return objects, nil
			}
		}

		if aws.BoolValue(result.IsTruncated) {
			continuationToken = result.NextContinuationToken
			if maxKeys > 0 {
				rem := int64(maxKeys - len(objects))
				if rem < reqMaxKeys {
					reqMaxKeys = rem
				}
			}
		} else {
			break
		}
	}

	log.Debug("objects listed", zap.Int("count", len(objects)))
	return objects, nil
}

// CopyObject copies an object within COS
func (c *Client) CopyObject(ctx context.Context, sourceKey, destKey string) error {
	return c.copyObject(ctx, sourceKey, destKey, nil)
}

// CopyObjectWithMetadata copies an object within COS, giving the copy
// metadata in place of the source's.
func (c *Client) CopyObjectWithMetadata(ctx context.Context, sourceKey, destKey string, metadata map[string]string) error {
	if metadata == nil {
		metadata = map[string]string{}
	}
	return c.copyObject(ctx, sourceKey, destKey, metadata)
}

// copyObject copies sourceKey to destKey, replacing the metadata when
// metadata is not nil.
func (c *Client) copyObject(ctx context.Context, sourceKey, destKey string, metadata map[string]string) error {
	log := logging.WithOperation("CopyObject").With(
		zap.String("sourceKey", sourceKey),
		zap.String("destKey", destKey),
	)
	log.Debug("copying object")

	copySource := fmt.Sprintf("%s/%s", c.bucket, sourceKey)

	input := &s3.CopyObjectInput{
		Bucket:     aws.String(c.bucket),
		CopySource: aws.String(copySource),
		Key:        aws.String(destKey),
	}
	if metadata != nil {
		input.Metadata = make(map[string]*string, len(metadata))
		for k, v := range metadata {
			input.Metadata[k] = aws.String(v)
		}
		input.MetadataDirective = aws.String("REPLACE")
	}

	_, err := c.s3Client.CopyObjectWithContext(ctx, input)
	if err != nil {
		log.Error("failed to copy object", zap.Error(err))
		return fmt.Errorf("failed to copy object: %w", err)
	}

	log.Debug("object copied")
	return nil
}

// UpdateObjectMetadata updates object metadata without rewriting the entire object
// Uses copy-to-self with metadata-replace directive
func (c *Client) UpdateObjectMetadata(ctx context.Context, key string, metadata map[string]string) error {
	log := logging.WithOperation("UpdateObjectMetadata").With(zap.String("key", key))
	log.Debug("updating object metadata")

	// Convert metadata to AWS format
	awsMetadata := make(map[string]*string)
	for k, v := range metadata {
		awsMetadata[k] = aws.String(v)
	}

	copySource := fmt.Sprintf("%s/%s", c.bucket, key)

	input := &s3.CopyObjectInput{
		Bucket:            aws.String(c.bucket),
		CopySource:        aws.String(copySource),
		Key:               aws.String(key),
		Metadata:          awsMetadata,
		MetadataDirective: aws.String("REPLACE"),
	}

	_, err := c.s3Client.CopyObjectWithContext(ctx, input)
	if err != nil {
		log.Error("failed to update object metadata", zap.Error(err))
		return fmt.Errorf("failed to update object metadata: %w", err)
	}

	log.Debug("object metadata updated")
	return nil
}

// ObjectExists checks if an object exists
func (c *Client) ObjectExists(ctx context.Context, key string) (bool, error) {
	_, err := c.HeadObject(ctx, key)
	if err != nil {
		// Check if it's a "not found" error
		if isNotFoundError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// isNotFoundError checks if an error is a "not found" error. errors.As is
// required: call sites wrap SDK errors with fmt.Errorf("...: %w", err) and a
// direct type assertion misses those.
func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	var aerr awserr.Error
	if errors.As(err, &aerr) {
		code := aerr.Code()
		return code == "NotFound" || code == "NoSuchKey" || code == s3.ErrCodeNoSuchKey || strings.Contains(code, "404")
	}
	return errors.Is(err, os.ErrNotExist)
}

// isCircuitOpenError follows both standard Go wrapping and the IBM SDK's
// awserr.OrigErr chain (awserr predates errors.Unwrap).
func isCircuitOpenError(err error) bool {
	for err != nil {
		if errors.Is(err, ErrCircuitOpen) {
			return true
		}

		var aerr awserr.Error
		if !errors.As(err, &aerr) {
			return false
		}
		orig := aerr.OrigErr()
		if orig == nil || orig == err {
			return false
		}
		err = orig
	}
	return false
}

// CreateMultipartUpload initiates a multipart upload and returns an upload ID
func (c *Client) CreateMultipartUpload(ctx context.Context, key string, metadata map[string]string) (string, error) {
	log := logging.WithOperation("CreateMultipartUpload").With(zap.String("key", key))
	log.Debug("initiating multipart upload")

	awsMetadata := make(map[string]*string)
	for k, v := range metadata {
		awsMetadata[k] = aws.String(v)
	}

	input := &s3.CreateMultipartUploadInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(key),
		Metadata: awsMetadata,
	}

	result, err := c.s3Client.CreateMultipartUploadWithContext(ctx, input)
	if err != nil {
		log.Error("failed to initiate multipart upload", zap.Error(err))
		return "", fmt.Errorf("failed to initiate multipart upload: %w", err)
	}

	log.Debug("multipart upload initiated", zap.String("uploadId", aws.StringValue(result.UploadId)))
	return aws.StringValue(result.UploadId), nil
}

// UploadPart uploads a part in a multipart upload and returns the ETag
func (c *Client) UploadPart(ctx context.Context, key, uploadID string, partNumber int64, body io.ReadSeeker) (string, error) {
	log := logging.WithOperation("UploadPart").With(
		zap.String("key", key),
		zap.String("uploadId", uploadID),
		zap.Int64("partNumber", partNumber),
	)
	log.Debug("uploading part")

	input := &s3.UploadPartInput{
		Bucket:     aws.String(c.bucket),
		Key:        aws.String(key),
		UploadId:   aws.String(uploadID),
		PartNumber: aws.Int64(partNumber),
		Body:       body,
	}

	result, err := c.s3Client.UploadPartWithContext(ctx, input)
	if err != nil {
		log.Error("failed to upload part", zap.Error(err))
		return "", fmt.Errorf("failed to upload part: %w", err)
	}

	log.Debug("part uploaded", zap.String("etag", aws.StringValue(result.ETag)))
	return aws.StringValue(result.ETag), nil
}

// CompleteMultipartUpload completes a multipart upload by assembling previously uploaded parts
func (c *Client) CompleteMultipartUpload(ctx context.Context, key, uploadID string, completedParts []*s3.CompletedPart) error {
	log := logging.WithOperation("CompleteMultipartUpload").With(
		zap.String("key", key),
		zap.String("uploadId", uploadID),
		zap.Int("partsCount", len(completedParts)),
	)
	log.Debug("completing multipart upload")

	input := &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &s3.CompletedMultipartUpload{
			Parts: completedParts,
		},
	}

	_, err := c.s3Client.CompleteMultipartUploadWithContext(ctx, input)
	if err != nil {
		log.Error("failed to complete multipart upload", zap.Error(err))
		return fmt.Errorf("failed to complete multipart upload: %w", err)
	}

	log.Debug("multipart upload completed")
	return nil
}

// AbortMultipartUpload aborts a multipart upload
func (c *Client) AbortMultipartUpload(ctx context.Context, key, uploadID string) error {
	log := logging.WithOperation("AbortMultipartUpload").With(
		zap.String("key", key),
		zap.String("uploadId", uploadID),
	)
	log.Debug("aborting multipart upload")

	input := &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	}

	_, err := c.s3Client.AbortMultipartUploadWithContext(ctx, input)
	if err != nil {
		log.Error("failed to abort multipart upload", zap.Error(err))
		return fmt.Errorf("failed to abort multipart upload: %w", err)
	}

	log.Debug("multipart upload aborted")
	return nil
}

// Close closes the client (cleanup if needed)
func (c *Client) Close() error {
	logging.Info("COS client closed")
	return nil
}

// Made with Bob
