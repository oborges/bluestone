package buffer

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/oborges/bluestone/internal/logging"
	"go.uber.org/zap"
)

// BufferSegment represents a contiguous chunk of buffered data
type BufferSegment struct {
	Offset int64
	Data   []byte
}

// WriteBuffer manages buffered writes for a single file
type WriteBuffer struct {
	mu           sync.RWMutex
	segments     map[int64]*BufferSegment // offset -> segment
	totalSize    int64                     // Total bytes in buffer
	maxSize      int64                     // Flush threshold
	dirty        bool                      // Has unflushed data
	lastWrite    time.Time                 // Last write timestamp
	minOffset    int64                     // Minimum offset in buffer
	maxOffset    int64                     // Maximum offset in buffer
	flushCount   int64                     // Number of flushes performed
	bytesWritten int64                     // Total bytes written to buffer
}

// NewWriteBuffer creates a new write buffer
func NewWriteBuffer(maxSize int64) *WriteBuffer {
	return &WriteBuffer{
		segments:  make(map[int64]*BufferSegment),
		maxSize:   maxSize,
		minOffset: -1,
		maxOffset: -1,
	}
}

// Write appends data at the specified offset
func (wb *WriteBuffer) Write(offset int64, data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}

	wb.mu.Lock()
	defer wb.mu.Unlock()

	// Create new segment
	segment := &BufferSegment{
		Offset: offset,
		Data:   make([]byte, len(data)),
	}
	copy(segment.Data, data)

	// Add to segments
	wb.segments[offset] = segment
	wb.totalSize += int64(len(data))
	wb.bytesWritten += int64(len(data))
	wb.dirty = true
	wb.lastWrite = time.Now()

	// Update offset range
	if wb.minOffset == -1 || offset < wb.minOffset {
		wb.minOffset = offset
	}
	endOffset := offset + int64(len(data))
	if wb.maxOffset == -1 || endOffset > wb.maxOffset {
		wb.maxOffset = endOffset
	}

	logging.Debug("Data written to buffer",
		zap.Int64("offset", offset),
		zap.Int("bytes", len(data)),
		zap.Int64("totalSize", wb.totalSize),
		zap.Int("segments", len(wb.segments)))

	return len(data), nil
}

// Read reads data from the buffer at the specified offset
// Returns nil if data not in buffer
func (wb *WriteBuffer) Read(offset int64, length int64) []byte {
	wb.mu.RLock()
	defer wb.mu.RUnlock()

	// Check if offset is in buffered range
	if wb.minOffset == -1 || offset < wb.minOffset || offset >= wb.maxOffset {
		return nil
	}

	// Find segment containing this offset
	for segOffset, segment := range wb.segments {
		segEnd := segOffset + int64(len(segment.Data))
		if offset >= segOffset && offset < segEnd {
			// Calculate how much data we can return from this segment
			startInSeg := offset - segOffset
			available := int64(len(segment.Data)) - startInSeg
			toRead := length
			if toRead > available {
				toRead = available
			}
			
			result := make([]byte, toRead)
			copy(result, segment.Data[startInSeg:startInSeg+toRead])
			return result
		}
	}

	return nil
}

// ShouldFlush returns true if buffer should be flushed
func (wb *WriteBuffer) ShouldFlush() bool {
	wb.mu.RLock()
	defer wb.mu.RUnlock()
	return wb.totalSize >= wb.maxSize
}

// GetFlushData returns merged data for flushing and clears the buffer
// Returns: data, startOffset, error
func (wb *WriteBuffer) GetFlushData() ([]byte, int64, error) {
	wb.mu.Lock()
	defer wb.mu.Unlock()

	if !wb.dirty || len(wb.segments) == 0 {
		return nil, 0, nil
	}

	// Sort segments by offset
	offsets := make([]int64, 0, len(wb.segments))
	for offset := range wb.segments {
		offsets = append(offsets, offset)
	}
	sort.Slice(offsets, func(i, j int) bool {
		return offsets[i] < offsets[j]
	})

	// Merge segments into contiguous data
	startOffset := offsets[0]
	endOffset := wb.maxOffset

	// Calculate total size needed
	totalSize := endOffset - startOffset
	if totalSize <= 0 {
		return nil, 0, fmt.Errorf("invalid buffer state: totalSize=%d", totalSize)
	}

	// Allocate buffer
	data := make([]byte, totalSize)

	// Copy segments into buffer
	for _, offset := range offsets {
		segment := wb.segments[offset]
		copyOffset := offset - startOffset
		if copyOffset < 0 || copyOffset >= totalSize {
			logging.Warn("Segment offset out of range",
				zap.Int64("offset", offset),
				zap.Int64("startOffset", startOffset),
				zap.Int64("copyOffset", copyOffset),
				zap.Int64("totalSize", totalSize))
			continue
		}
		copy(data[copyOffset:], segment.Data)
	}

	// Clear buffer
	wb.segments = make(map[int64]*BufferSegment)
	wb.totalSize = 0
	wb.dirty = false
	wb.minOffset = -1
	wb.maxOffset = -1
	wb.flushCount++

	logging.Info("Buffer flushed",
		zap.Int64("startOffset", startOffset),
		zap.Int("bytes", len(data)),
		zap.Int64("flushCount", wb.flushCount))

	return data, startOffset, nil
}

// IsDirty returns true if buffer has unflushed data
func (wb *WriteBuffer) IsDirty() bool {
	wb.mu.RLock()
	defer wb.mu.RUnlock()
	return wb.dirty
}

// Size returns current buffer size
func (wb *WriteBuffer) Size() int64 {
	wb.mu.RLock()
	defer wb.mu.RUnlock()
	return wb.totalSize
}

// Stats returns buffer statistics
func (wb *WriteBuffer) Stats() BufferStats {
	wb.mu.RLock()
	defer wb.mu.RUnlock()

	return BufferStats{
		TotalSize:    wb.totalSize,
		SegmentCount: len(wb.segments),
		IsDirty:      wb.dirty,
		FlushCount:   wb.flushCount,
		BytesWritten: wb.bytesWritten,
		LastWrite:    wb.lastWrite,
	}
}

// BufferStats contains buffer statistics
type BufferStats struct {
	TotalSize    int64
	SegmentCount int
	IsDirty      bool
	FlushCount   int64
	BytesWritten int64
	LastWrite    time.Time
}

// Clear clears the buffer without flushing
func (wb *WriteBuffer) Clear() {
	wb.mu.Lock()
	defer wb.mu.Unlock()

	wb.segments = make(map[int64]*BufferSegment)
	wb.totalSize = 0
	wb.dirty = false
	wb.minOffset = -1
	wb.maxOffset = -1
}

// Made with Bob
