package staging

import (
	"sync"
	"testing"
	"time"
)

func TestDirtyFileIndex_MarkDirty(t *testing.T) {
	index := NewDirtyFileIndex()

	// Test marking a file as dirty
	path := "/test/file.txt"
	size := int64(1024)

	index.MarkDirty(path, size)

	if !index.IsDirty(path) {
		t.Error("File should be marked as dirty")
	}

	if index.Count() != 1 {
		t.Errorf("Expected count 1, got %d", index.Count())
	}
}

func TestDirtyFileIndex_MarkClean(t *testing.T) {
	index := NewDirtyFileIndex()

	path := "/test/file.txt"
	index.MarkDirty(path, 1024)

	// Mark as clean
	index.MarkClean(path)

	if index.IsDirty(path) {
		t.Error("File should not be dirty after marking clean")
	}

	if index.Count() != 0 {
		t.Errorf("Expected count 0, got %d", index.Count())
	}
}

func TestDirtyFileIndex_GetDirtyFiles(t *testing.T) {
	index := NewDirtyFileIndex()

	// Mark multiple files as dirty
	files := map[string]int64{
		"/test/file1.txt": 1024,
		"/test/file2.txt": 2048,
		"/test/file3.txt": 4096,
	}

	for path, size := range files {
		index.MarkDirty(path, size)
	}

	dirtyFiles := index.GetDirtyFiles()

	if len(dirtyFiles) != len(files) {
		t.Errorf("Expected %d dirty files, got %d", len(files), len(dirtyFiles))
	}

	// Verify all files are present
	foundPaths := make(map[string]bool)
	for _, metadata := range dirtyFiles {
		foundPaths[metadata.Path] = true

		expectedSize := files[metadata.Path]
		if metadata.Size != expectedSize {
			t.Errorf("Expected size %d for %s, got %d",
				expectedSize, metadata.Path, metadata.Size)
		}
	}

	for path := range files {
		if !foundPaths[path] {
			t.Errorf("Path %s not found in dirty files", path)
		}
	}
}

func TestDirtyFileIndex_UpdateSize(t *testing.T) {
	index := NewDirtyFileIndex()

	path := "/test/file.txt"
	index.MarkDirty(path, 1024)

	// Update size
	index.MarkDirty(path, 2048)

	dirtyFiles := index.GetDirtyFiles()
	if len(dirtyFiles) != 1 {
		t.Fatalf("Expected 1 dirty file, got %d", len(dirtyFiles))
	}

	if dirtyFiles[0].Size != 2048 {
		t.Errorf("Expected size 2048, got %d", dirtyFiles[0].Size)
	}
}

func TestDirtyFileIndex_IncrementSyncAttempts(t *testing.T) {
	index := NewDirtyFileIndex()

	path := "/test/file.txt"
	index.MarkDirty(path, 1024)

	// Increment sync attempts
	index.IncrementSyncAttempts(path, nil)
	index.IncrementSyncAttempts(path, nil)

	dirtyFiles := index.GetDirtyFiles()
	if len(dirtyFiles) != 1 {
		t.Fatalf("Expected 1 dirty file, got %d", len(dirtyFiles))
	}

	if dirtyFiles[0].SyncAttempts != 2 {
		t.Errorf("Expected 2 sync attempts, got %d", dirtyFiles[0].SyncAttempts)
	}
}

func TestDirtyFileIndex_ThreadSafety(t *testing.T) {
	index := NewDirtyFileIndex()

	// Run concurrent operations
	var wg sync.WaitGroup
	numGoroutines := 100
	numOperations := 100

	// Concurrent MarkDirty
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				path := "/test/file.txt"
				index.MarkDirty(path, int64(id*numOperations+j))
			}
		}(i)
	}

	// Concurrent IsDirty
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				index.IsDirty("/test/file.txt")
			}
		}()
	}

	// Concurrent GetDirtyFiles
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				index.GetDirtyFiles()
			}
		}()
	}

	wg.Wait()

	// Verify no race conditions (test should not panic)
	if !index.IsDirty("/test/file.txt") {
		t.Error("File should be dirty after concurrent operations")
	}
}

func TestDirtyFileIndex_DirtySince(t *testing.T) {
	index := NewDirtyFileIndex()

	path := "/test/file.txt"
	before := time.Now()
	index.MarkDirty(path, 1024)
	after := time.Now()

	dirtyFiles := index.GetDirtyFiles()
	if len(dirtyFiles) != 1 {
		t.Fatalf("Expected 1 dirty file, got %d", len(dirtyFiles))
	}

	dirtySince := dirtyFiles[0].DirtySince
	if dirtySince.Before(before) || dirtySince.After(after) {
		t.Errorf("DirtySince timestamp %v not in expected range [%v, %v]",
			dirtySince, before, after)
	}
}

func TestDirtyFileIndex_MultipleFiles(t *testing.T) {
	index := NewDirtyFileIndex()

	// Mark multiple files
	for i := 0; i < 100; i++ {
		path := "/test/file" + string(rune(i)) + ".txt"
		index.MarkDirty(path, int64(i*1024))
	}

	if index.Count() != 100 {
		t.Errorf("Expected count 100, got %d", index.Count())
	}

	// Clean half of them
	for i := 0; i < 50; i++ {
		path := "/test/file" + string(rune(i)) + ".txt"
		index.MarkClean(path)
	}

	if index.Count() != 50 {
		t.Errorf("Expected count 50 after cleaning, got %d", index.Count())
	}
}

func TestDirtyFileIndex_CleanNonExistent(t *testing.T) {
	index := NewDirtyFileIndex()

	// Cleaning a non-existent file should not panic
	index.MarkClean("/nonexistent/file.txt")

	if index.Count() != 0 {
		t.Errorf("Expected count 0, got %d", index.Count())
	}
}

func TestDirtyFileIndex_IncrementNonExistent(t *testing.T) {
	index := NewDirtyFileIndex()

		// Incrementing sync attempts for non-existent file should not panic
		index.IncrementSyncAttempts("/nonexistent/file.txt", nil)

	if index.Count() != 0 {
		t.Errorf("Expected count 0, got %d", index.Count())
	}
}

// Made with Bob

// A generation is never handed out twice, so an upload cannot take a later
// state of its path for the one it read: not after the path was dropped and
// marked dirty again, and not after another file was renamed over it.
func TestDirtyFileIndex_GenerationIsNeverReused(t *testing.T) {
	index := NewDirtyFileIndex()

	index.MarkDirty("/recreated", 1)
	uploading := index.GetMetadata("/recreated").LocalDirtyGeneration
	index.MarkClean("/recreated")
	index.MarkDirty("/recreated", 1)
	if _, clean := index.MarkCleanIfUnchanged("/recreated", uploading); clean {
		t.Fatal("upload of a dropped file marked the file that took its path clean")
	}
	if !index.IsDirty("/recreated") {
		t.Fatal("refused clean dropped the entry")
	}

	// Counting writes per file would give the renamed entry the
	// destination's count here: one write and the rename against two writes.
	index.MarkDirty("/src", 1)
	index.MarkDirty("/dst", 1)
	index.MarkDirty("/dst", 1)
	uploading = index.GetMetadata("/dst").LocalDirtyGeneration
	index.Rekey("/src", "/dst", "staged")
	if _, clean := index.MarkCleanIfUnchanged("/dst", uploading); clean {
		t.Fatal("upload of a replaced destination marked the renamed file clean")
	}

	current := index.GetMetadata("/dst").LocalDirtyGeneration
	if wasDirty, clean := index.MarkCleanIfUnchanged("/dst", current); !wasDirty || !clean {
		t.Fatalf("unchanged file: wasDirty %v clean %v, want both", wasDirty, clean)
	}
	if wasDirty, clean := index.MarkCleanIfUnchanged("/dst", current); wasDirty || !clean {
		t.Fatalf("path without an entry: wasDirty %v clean %v, want clean and not dirty", wasDirty, clean)
	}
}
