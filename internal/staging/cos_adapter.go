package staging

import (
	"context"
	"io"
	"time"

	"github.com/IBM/ibm-cos-sdk-go/service/s3"
	"github.com/oborges/bluestone/internal/cos"
	"github.com/oborges/bluestone/internal/posix"
	"github.com/oborges/bluestone/pkg/types"
)

// COSClientAdapter adapts the COS client to the COSClient interface required by SyncWorker
type COSClientAdapter struct {
	Client *cos.Client
}

// PutObject uploads data to COS
func (a *COSClientAdapter) PutObject(ctx context.Context, key string, data []byte, metadata map[string]string) error {
	// Create POSIX attributes for the file
	now := time.Now()
	attrs := &types.POSIXAttributes{
		Mode:  0644,
		UID:   1000,
		GID:   1000,
		Atime: now,
		Mtime: now,
		Ctime: now,
	}
	
	// Encode attributes to metadata
	cosMetadata := posix.EncodePOSIXAttributes(attrs)
	
	// Merge with any additional metadata provided
	for k, v := range metadata {
		cosMetadata[k] = v
	}
	
	// Upload to COS
	return a.Client.PutObject(ctx, key, data, cosMetadata)
}

// PutObjectStream uploads an object stream to COS
func (a *COSClientAdapter) PutObjectStream(ctx context.Context, key string, body io.ReadSeeker, metadata map[string]string) error {
	// Create POSIX attributes for the file
	now := time.Now()
	attrs := &types.POSIXAttributes{
		Mode:  0644,
		UID:   1000,
		GID:   1000,
		Atime: now,
		Mtime: now,
		Ctime: now,
	}
	
	cosMetadata := posix.EncodePOSIXAttributes(attrs)
	for k, v := range metadata {
		cosMetadata[k] = v
	}
	
	return a.Client.PutObjectStream(ctx, key, body, cosMetadata)
}

// GetObjectStream downloads an object stream from COS
func (a *COSClientAdapter) GetObjectStream(ctx context.Context, key string) (io.ReadCloser, error) {
	return a.Client.GetObjectStream(ctx, key)
}

// CreateMultipartUpload overrides and initiates a multipart upload stream
func (a *COSClientAdapter) CreateMultipartUpload(ctx context.Context, key string, metadata map[string]string) (string, error) {
	now := time.Now()
	attrs := &types.POSIXAttributes{
		Mode:  0644,
		UID:   1000,
		GID:   1000,
		Atime: now,
		Mtime: now,
		Ctime: now,
	}
	cosMetadata := posix.EncodePOSIXAttributes(attrs)
	for k, v := range metadata {
		cosMetadata[k] = v
	}
	return a.Client.CreateMultipartUpload(ctx, key, cosMetadata)
}

// UploadPart uploads a part in a multipart upload and returns the ETag
func (a *COSClientAdapter) UploadPart(ctx context.Context, key, uploadID string, partNumber int64, body io.ReadSeeker) (string, error) {
	return a.Client.UploadPart(ctx, key, uploadID, partNumber, body)
}

// CompleteMultipartUpload completes a multipart upload by assembling previously uploaded parts
func (a *COSClientAdapter) CompleteMultipartUpload(ctx context.Context, key, uploadID string, completedParts []*s3.CompletedPart) error {
	return a.Client.CompleteMultipartUpload(ctx, key, uploadID, completedParts)
}

// AbortMultipartUpload aborts a multipart upload
func (a *COSClientAdapter) AbortMultipartUpload(ctx context.Context, key, uploadID string) error {
	return a.Client.AbortMultipartUpload(ctx, key, uploadID)
}

// DeleteObject deletes an object from COS
func (a *COSClientAdapter) DeleteObject(ctx context.Context, key string) error {
	return a.Client.DeleteObject(ctx, key)
}

// Made with Bob
