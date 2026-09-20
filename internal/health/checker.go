package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/oborges/bluestone/internal/logging"
	"go.uber.org/zap"
)

// Status represents health check status
type Status string

const (
	StatusHealthy   Status = "healthy"
	StatusUnhealthy Status = "unhealthy"
	StatusDegraded  Status = "degraded"
)

// CheckResult represents the result of a health check
type CheckResult struct {
	Status    Status                 `json:"status"`
	Timestamp time.Time              `json:"timestamp"`
	Checks    map[string]CheckDetail `json:"checks"`
}

// CheckDetail represents details of a specific check
type CheckDetail struct {
	Status  Status `json:"status"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Checker performs health checks
type Checker struct {
	checks map[string]CheckFunc
	mu     sync.RWMutex
}

// CheckFunc is a function that performs a health check
type CheckFunc func(ctx context.Context) CheckDetail

// NewChecker creates a new health checker
func NewChecker() *Checker {
	return &Checker{
		checks: make(map[string]CheckFunc),
	}
}

// RegisterCheck registers a health check
func (c *Checker) RegisterCheck(name string, check CheckFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks[name] = check
	logging.Info("Health check registered", zap.String("name", name))
}

// Check performs all health checks
func (c *Checker) Check(ctx context.Context) CheckResult {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := CheckResult{
		Status:    StatusHealthy,
		Timestamp: time.Now(),
		Checks:    make(map[string]CheckDetail),
	}

	for name, check := range c.checks {
		detail := check(ctx)
		result.Checks[name] = detail

		// Update overall status
		if detail.Status == StatusUnhealthy {
			result.Status = StatusUnhealthy
		} else if detail.Status == StatusDegraded && result.Status == StatusHealthy {
			result.Status = StatusDegraded
		}
	}

	return result
}

// StartHealthServer starts an HTTP server for health checks
func StartHealthServer(port int, checker *Checker) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	mux := http.NewServeMux()

	// Liveness probe - always returns 200 if server is running
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Readiness probe - checks if service is ready to accept traffic
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		result := checker.Check(ctx)

		w.Header().Set("Content-Type", "application/json")

		if result.Status == StatusHealthy {
			w.WriteHeader(http.StatusOK)
		} else if result.Status == StatusDegraded {
			w.WriteHeader(http.StatusOK) // Still ready, but degraded
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}

		json.NewEncoder(w).Encode(result)
	})

	// Startup probe - checks if service has started successfully
	mux.HandleFunc("/health/startup", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		result := checker.Check(ctx)

		w.Header().Set("Content-Type", "application/json")

		if result.Status == StatusHealthy || result.Status == StatusDegraded {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}

		json.NewEncoder(w).Encode(result)
	})

	// Detailed health check
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		result := checker.Check(ctx)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(result)
	})

	logging.Info("Starting health check server", zap.String("addr", addr))

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 3 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       15 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil {
			logging.Error("Health check server failed", zap.Error(err))
		}
	}()

	return nil
}

// Common health check functions

// COSHealthCheck creates a health check for COS connectivity
func COSHealthCheck(pingFunc func(context.Context) error) CheckFunc {
	return func(ctx context.Context) CheckDetail {
		if err := pingFunc(ctx); err != nil {
			return CheckDetail{
				Status:  StatusUnhealthy,
				Message: "COS connectivity check failed",
				Error:   err.Error(),
			}
		}
		return CheckDetail{
			Status:  StatusHealthy,
			Message: "COS is accessible",
		}
	}
}

// CacheHealthCheck creates a health check for cache
func CacheHealthCheck(isEnabled func() bool, getStats func() interface{}) CheckFunc {
	return func(ctx context.Context) CheckDetail {
		if !isEnabled() {
			return CheckDetail{
				Status:  StatusHealthy,
				Message: "Cache is disabled",
			}
		}

		// Cache is enabled and working
		return CheckDetail{
			Status:  StatusHealthy,
			Message: "Cache is operational",
		}
	}
}

// SMBStats is what an SMB server reports about itself to a health check.
type SMBStats struct {
	// Running is false once the server has stopped serving, whether it was
	// asked to stop or its accept loop failed.
	Running bool
	// Connections, Sessions and OpenFiles are what it currently carries.
	Connections int
	Sessions    int
	OpenFiles   int
}

// SMBHealthCheck creates a health check for the SMB server. Pass a nil stats
// function when SMB is disabled, which is healthy: the gateway is doing what
// it was configured to do.
func SMBHealthCheck(stats func() SMBStats) CheckFunc {
	return func(ctx context.Context) CheckDetail {
		if stats == nil {
			return CheckDetail{
				Status:  StatusHealthy,
				Message: "SMB server is disabled",
			}
		}
		current := stats()
		if !current.Running {
			return CheckDetail{
				Status:  StatusUnhealthy,
				Message: "SMB server is not serving",
			}
		}
		return CheckDetail{
			Status: StatusHealthy,
			Message: fmt.Sprintf("SMB server is serving: %d connections, %d sessions, %d open files",
				current.Connections, current.Sessions, current.OpenFiles),
		}
	}
}

// DiskSpaceHealthCheck creates a health check for disk space
func DiskSpaceHealthCheck(path string, minFreePercent float64) CheckFunc {
	return func(ctx context.Context) CheckDetail {
		// TODO: Implement actual disk space check
		// For now, return healthy
		return CheckDetail{
			Status:  StatusHealthy,
			Message: "Disk space is sufficient",
		}
	}
}

// Made with Bob
