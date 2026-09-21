package cos

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/IBM/ibm-cos-sdk-go/aws"
	"github.com/IBM/ibm-cos-sdk-go/service/s3"
	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/internal/logging"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const (
	// MinPartSize is the minimum size for a multipart upload part (5MB)
	MinPartSize = 5 * 1024 * 1024
	// MaxParts is the maximum number of parts in a multipart upload
	MaxParts = 10000
)

// MultipartUpload handles multipart uploads for large files
type MultipartUpload struct {
	client     *Client
	key        string
	uploadID   string
	parts      []*s3.CompletedPart
	partSize   int64
	metadata   map[string]string
	mu         sync.Mutex
}

// NewMultipartUpload creates a new multipart upload
func (c *Client) NewMultipartUpload(ctx context.Context, key string, metadata map[string]string, partSize int64) (*MultipartUpload, error) {
	log := logging.WithOperation("NewMultipartUpload").With(
		zap.String("key", key),
		zap.Int64("partSize", partSize),
	)
	log.Debug("initiating multipart upload")

	// Validate part size
	if partSize < MinPartSize {
		return nil, fmt.Errorf("part size must be at least %d bytes", MinPartSize)
	}

	// Convert metadata to AWS format
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
		return nil, fmt.Errorf("failed to initiate multipart upload: %w", err)
	}

	uploadID := aws.StringValue(result.UploadId)
	log.Info("multipart upload initiated", zap.String("uploadId", uploadID))

	return &MultipartUpload{
		client:   c,
		key:      key,
		uploadID: uploadID,
		parts:    make([]*s3.CompletedPart, 0),
		partSize: partSize,
		metadata: metadata,
	}, nil
}

// UploadPart uploads a single part
func (m *MultipartUpload) UploadPart(ctx context.Context, partNumber int, data []byte) error {
	log := logging.WithOperation("UploadPart").With(
		zap.String("key", m.key),
		zap.String("uploadId", m.uploadID),
		zap.Int("partNumber", partNumber),
		zap.Int("size", len(data)),
	)
	log.Debug("uploading part")

	input := &s3.UploadPartInput{
		Bucket:     aws.String(m.client.bucket),
		Key:        aws.String(m.key),
		UploadId:   aws.String(m.uploadID),
		PartNumber: aws.Int64(int64(partNumber)),
		Body:       bytes.NewReader(data),
	}

	result, err := m.client.s3Client.UploadPartWithContext(ctx, input)
	if err = m.client.noteWrite(err); err != nil {
		log.Error("failed to upload part", zap.Error(err))
		return fmt.Errorf("failed to upload part %d: %w", partNumber, err)
	}

	// Store completed part
	m.mu.Lock()
	m.parts = append(m.parts, &s3.CompletedPart{
		ETag:       result.ETag,
		PartNumber: aws.Int64(int64(partNumber)),
	})
	m.mu.Unlock()

	log.Debug("part uploaded", zap.String("etag", aws.StringValue(result.ETag)))
	return nil
}

// UploadParallel uploads data in parallel parts
func (m *MultipartUpload) UploadParallel(ctx context.Context, data []byte, maxConcurrency int) error {
	log := logging.WithOperation("UploadParallel").With(
		zap.String("key", m.key),
		zap.Int("totalSize", len(data)),
		zap.Int64("partSize", m.partSize),
		zap.Int("maxConcurrency", maxConcurrency),
	)
	log.Info("starting parallel upload")

	// Calculate number of parts
	totalSize := int64(len(data))
	numParts := (totalSize + m.partSize - 1) / m.partSize

	if numParts > MaxParts {
		return fmt.Errorf("file too large: would require %d parts (max %d)", numParts, MaxParts)
	}

	// Create error group for parallel uploads
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrency)

	// Upload parts in parallel
	for i := int64(0); i < numParts; i++ {
		partNumber := int(i + 1)
		start := i * m.partSize
		end := start + m.partSize
		if end > totalSize {
			end = totalSize
		}

		partData := data[start:end]

		g.Go(func() error {
			return m.UploadPart(ctx, partNumber, partData)
		})
	}

	// Wait for all uploads to complete
	if err := g.Wait(); err != nil {
		log.Error("parallel upload failed", zap.Error(err))
		// Abort the multipart upload on error
		if abortErr := m.Abort(context.Background()); abortErr != nil {
			log.Error("failed to abort multipart upload", zap.Error(abortErr))
		}
		return fmt.Errorf("parallel upload failed: %w", err)
	}

	log.Info("parallel upload completed", zap.Int("parts", int(numParts)))
	return nil
}

// Complete completes the multipart upload
func (m *MultipartUpload) Complete(ctx context.Context) error {
	log := logging.WithOperation("CompleteMultipartUpload").With(
		zap.String("key", m.key),
		zap.String("uploadId", m.uploadID),
		zap.Int("parts", len(m.parts)),
	)
	log.Debug("completing multipart upload")

	// Sort parts by part number
	m.mu.Lock()
	parts := make([]*s3.CompletedPart, len(m.parts))
	copy(parts, m.parts)
	m.mu.Unlock()

	input := &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(m.client.bucket),
		Key:      aws.String(m.key),
		UploadId: aws.String(m.uploadID),
		MultipartUpload: &s3.CompletedMultipartUpload{
			Parts: parts,
		},
	}

	_, err := m.client.s3Client.CompleteMultipartUploadWithContext(ctx, input)
	if err = m.client.noteWrite(err); err != nil {
		log.Error("failed to complete multipart upload", zap.Error(err))
		return fmt.Errorf("failed to complete multipart upload: %w", err)
	}

	log.Info("multipart upload completed")
	return nil
}

// Abort aborts the multipart upload
func (m *MultipartUpload) Abort(ctx context.Context) error {
	log := logging.WithOperation("AbortMultipartUpload").With(
		zap.String("key", m.key),
		zap.String("uploadId", m.uploadID),
	)
	log.Debug("aborting multipart upload")

	input := &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(m.client.bucket),
		Key:      aws.String(m.key),
		UploadId: aws.String(m.uploadID),
	}

	_, err := m.client.s3Client.AbortMultipartUploadWithContext(ctx, input)
	if err != nil {
		log.Error("failed to abort multipart upload", zap.Error(err))
		return fmt.Errorf("failed to abort multipart upload: %w", err)
	}

	log.Info("multipart upload aborted")
	return nil
}

// PutObjectMultipart uploads a large object using multipart upload
func (c *Client) PutObjectMultipart(ctx context.Context, key string, data []byte, metadata map[string]string, perfConfig *config.PerformanceConfig) error {
	log := logging.WithOperation("PutObjectMultipart").With(
		zap.String("key", key),
		zap.Int("size", len(data)),
	)
	log.Info("starting multipart upload")

	// Calculate part size (default to 10MB)
	partSize := int64(10 * 1024 * 1024)
	if perfConfig != nil && perfConfig.MultipartChunkMB > 0 {
		partSize = int64(perfConfig.MultipartChunkMB) * 1024 * 1024
	}

	// Create multipart upload
	upload, err := c.NewMultipartUpload(ctx, key, metadata, partSize)
	if err != nil {
		return err
	}

	// Upload parts in parallel
	maxConcurrency := 5 // Default concurrency
	if perfConfig != nil && perfConfig.MaxConcurrentWrites > 0 {
		maxConcurrency = perfConfig.MaxConcurrentWrites
	}
	if err := upload.UploadParallel(ctx, data, maxConcurrency); err != nil {
		return err
	}

	// Complete the upload
	if err := upload.Complete(ctx); err != nil {
		return err
	}

	log.Info("multipart upload successful")
	return nil
}

// ShouldUseMultipart determines if multipart upload should be used
func ShouldUseMultipart(size int64, perfConfig *config.PerformanceConfig) bool {
	thresholdMB := int64(100) // Default threshold
	if perfConfig != nil && perfConfig.MultipartThresholdMB > 0 {
		thresholdMB = int64(perfConfig.MultipartThresholdMB)
	}
	threshold := thresholdMB * 1024 * 1024
	return size >= threshold
}

// Made with Bob
