package cos

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/IBM/ibm-cos-sdk-go/aws"
	"github.com/IBM/ibm-cos-sdk-go/aws/credentials"
	"github.com/IBM/ibm-cos-sdk-go/aws/session"
	"github.com/IBM/ibm-cos-sdk-go/service/s3"
)

// newLocalTestClient returns a client that talks to handler through the real
// SDK, so request and response encoding are exercised end to end.
func newLocalTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	sess, err := session.NewSession(&aws.Config{
		Endpoint:         aws.String(server.URL),
		Region:           aws.String("us-south"),
		Credentials:      credentials.NewStaticCredentials("access", "secret", ""),
		S3ForcePathStyle: aws.Bool(true),
		DisableSSL:       aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("session.NewSession() error = %v", err)
	}
	return &Client{s3Client: s3.New(sess), bucket: "bucket"}
}

func TestMetadataKeysRoundTripThroughSDK(t *testing.T) {
	var mu sync.Mutex
	var putHeaders http.Header
	client := newLocalTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			mu.Lock()
			putHeaders = r.Header.Clone()
			mu.Unlock()
			w.Header().Set("ETag", `"etag"`)
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			w.Header().Set("Content-Length", "4")
			w.Header().Set("ETag", `"etag"`)
			w.Header().Set("Last-Modified", "Tue, 15 Sep 2026 12:00:00 GMT")
			w.Header().Set("X-Amz-Meta-Mode", "750")
			// As stored by earlier gateway versions, which passed prefixed
			// keys that the SDK prefixed again.
			w.Header().Set("X-Amz-Meta-X-Amz-Meta-Uid", "42")
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	ctx := context.Background()

	if err := client.PutObject(ctx, "file.txt", []byte("data"), map[string]string{"mode": "750"}); err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}
	mu.Lock()
	sent, doubled := putHeaders.Get("X-Amz-Meta-Mode"), putHeaders.Get("X-Amz-Meta-X-Amz-Meta-Mode")
	mu.Unlock()
	if sent != "750" || doubled != "" {
		t.Fatalf("PUT metadata headers: x-amz-meta-mode=%q x-amz-meta-x-amz-meta-mode=%q; want the single-prefixed header only", sent, doubled)
	}

	meta, err := client.HeadObject(ctx, "file.txt")
	if err != nil {
		t.Fatalf("HeadObject() error = %v", err)
	}
	if meta.Metadata["mode"] != "750" || meta.Metadata["x-amz-meta-uid"] != "42" {
		t.Fatalf("HeadObject metadata = %v, want lowercase keys mode and x-amz-meta-uid", meta.Metadata)
	}
}
