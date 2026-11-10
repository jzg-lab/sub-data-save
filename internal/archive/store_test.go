package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testLimits() Limits {
	return Limits{MaxRecords: 4, MaxBytes: 1024, MaxRecordBytes: 512}
}

func TestSavePreservesJSONAndRebuildsUsage(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	content := "{ \"request\": \"raw\\nvalue\", \"n\": 9007199254740993 }"
	completedAt := time.Date(2026, 11, 11, 0, 30, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	path, err := store.Save(context.Background(), "instance-1", "record-1", completedAt, []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("archive changed JSON bytes: got %q", got)
	}
	if want := filepath.Join(root, "2026", "11", "10", "instance-1", "record-1.json"); path != want {
		t.Fatalf("unexpected archive path: got %q want %q", path, want)
	}

	store, err = Open(root, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	count, size := store.Usage()
	if count != 1 || size != int64(len(content)) {
		t.Fatalf("usage after reopen = (%d, %d)", count, size)
	}
}

func TestSaveRejectsIncompleteAndOversizedRecords(t *testing.T) {
	store, err := Open(t.TempDir(), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{`{"partial":`, `[]`, `{} {}`} {
		if _, err := store.Save(context.Background(), "instance", "record", time.Now(), []byte(content)); !errors.Is(err, ErrInvalidJSON) {
			t.Fatalf("Save(%q) error = %v, want ErrInvalidJSON", content, err)
		}
	}
	large := `{"data":"` + strings.Repeat("x", 510) + `"}`
	if _, err := store.Save(context.Background(), "instance", "large", time.Now(), []byte(large)); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("oversized record error = %v, want ErrLimitReached", err)
	}
	count, size := store.Usage()
	if count != 0 || size != 0 {
		t.Fatalf("failed writes affected usage: (%d, %d)", count, size)
	}
	files := 0
	if err := filepath.WalkDir(store.root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			files++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if files != 0 {
		t.Fatalf("failed writes created %d formal files", files)
	}
}

func TestSaveEnforcesConcurrentRecordLimit(t *testing.T) {
	limits := Limits{MaxRecords: 2, MaxBytes: 1024, MaxRecordBytes: 512}
	store, err := Open(t.TempDir(), limits)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.Save(context.Background(), "instance", fmtRecordID(i), time.Now(), []byte(`{"ok":true}`))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successes++
			} else if !errors.Is(err, ErrLimitReached) {
				t.Errorf("Save error = %v", err)
			}
		}(i)
	}
	wg.Wait()
	if successes != limits.MaxRecords {
		t.Fatalf("successful commits = %d, want %d", successes, limits.MaxRecords)
	}
}

func TestOpenRemovesPendingFilesAndRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	pendingDir := filepath.Join(root, "2026", "11", "11", "instance")
	if err := os.MkdirAll(pendingDir, 0700); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(pendingDir, ".pending-leftover")
	if err := os.WriteFile(pending, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, testLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pending); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending file still exists: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, testLimits()); err == nil {
		t.Fatal("Open accepted a symlink in archive tree")
	}
}

func TestListAndReadRecordsByDate(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	completedAt := time.Date(2026, 11, 11, 8, 30, 0, 0, time.UTC)
	content := []byte(`{"request":"kept"}`)
	if _, err := store.Save(context.Background(), "instance-1", "record-1", completedAt, content); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), "instance-2", "record-2", completedAt, []byte(`{"request":"also kept"}`)); err != nil {
		t.Fatal(err)
	}
	records, err := store.List("2026-11-11")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Date != "2026-11-11" || records[0].Size == 0 {
		t.Fatalf("unexpected records: %+v", records)
	}
	got, err := store.Read("2026-11-11", "instance-1", "record-1")
	if err != nil || string(got) != string(content) {
		t.Fatalf("Read() = (%q, %v), want (%q, nil)", got, err, content)
	}
	if _, err := store.Read("../2026-11-11", "instance-1", "record-1"); err == nil {
		t.Fatal("Read accepted an invalid date")
	}
	if _, err := store.List("../../etc"); err == nil {
		t.Fatal("List accepted an invalid date")
	}
}

func fmtRecordID(i int) string {
	return "record-" + string(rune('a'+i))
}
