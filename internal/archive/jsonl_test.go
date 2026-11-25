package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJSONLAppendGroupsModelsAndRollsAtLimit(t *testing.T) {
	root := t.TempDir()
	line := []byte(`{"requested_model":"Opus5.5","model":"claude-opus","n":1}` + "\n")
	writer, err := OpenJSONL(root, int64(len(line)))
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 6, 25, 12, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	closed, err := writer.Append(line[:len(line)-1], stamp)
	if err != nil || len(closed) != 1 {
		t.Fatalf("exact-limit append = %v, %v; want finalized shard", closed, err)
	}
	name := filepath.Base(closed[0])
	if !strings.HasPrefix(name, "Opus5.5-2026-06-25-") || !strings.HasSuffix(name, "-000001.jsonl") {
		t.Fatalf("unexpected shard name %q", name)

	}
	data, err := os.ReadFile(closed[0])
	if err != nil || string(data) != string(line) {
		t.Fatalf("JSONL contents = %q, %v", data, err)
	}
}

func TestJSONLModelFallbackAndS3Key(t *testing.T) {
	root := t.TempDir()
	writer, err := OpenJSONL(root, 1024)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	if _, err := writer.Append([]byte(`{"model":"claude-opus"}`), stamp); err != nil {
		t.Fatal(err)
	}
	pending, err := writer.Pending()
	if err != nil || len(pending) != 0 {
		t.Fatalf("active shard should not be pending: %v, %v", pending, err)
	}
	if _, err := writer.Append([]byte(`{"requested_model":"claude-opus","model":"claude-opus"}`), stamp.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	pending, err = writer.Pending()
	if err != nil || len(pending) != 1 {
		t.Fatalf("date change should finalize prior shard: %v, %v", pending, err)
	}
	key, err := S3ObjectKey(root, pending[0])
	if err != nil || !strings.HasPrefix(key, "2026-06-25/claude-opus/claude-opus-2026-06-25-") || !strings.HasSuffix(key, "-000001.jsonl") {
		t.Fatalf("S3 key = %q, %v", key, err)
	}
}

func TestSealBeforeKeepsCurrentDateOpen(t *testing.T) {
	writer, err := OpenJSONL(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	if _, err := writer.Append([]byte(`{"requested_model":"opus"}`), stamp); err != nil {
		t.Fatal(err)
	}
	closed, err := writer.SealBefore(stamp.Add(time.Hour))
	if err != nil || len(closed) != 0 {
		t.Fatalf("current date sealed: %v %v", closed, err)
	}
	closed, err = writer.SealBefore(stamp.AddDate(0, 0, 1))
	if err != nil || len(closed) != 1 {
		t.Fatalf("past date not sealed: %v %v", closed, err)
	}
}

func TestOpenJSONLTruncatesPartialTrailingRecord(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "2026-06-25", "Opus5.5", "Opus5.5-2026-06-25-000001.open")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	complete := []byte(`{"requested_model":"Opus5.5"}` + "\n")
	if err := os.WriteFile(path, append(append([]byte(nil), complete...), []byte(`{"requested_model":"partial`)...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJSONL(root, 1024); err != nil {
		t.Fatal(err)
	}
	recovered := strings.TrimSuffix(path, ".open") + ".jsonl"
	data, err := os.ReadFile(recovered)
	if err != nil || string(data) != string(complete) {
		t.Fatalf("recovered shard = %q, %v; want complete lines only", data, err)
	}
}

func TestOpenJSONLRemovesShardWithoutCompleteRecord(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "2026-06-25", "Opus5.5", "Opus5.5-2026-06-25-000001.open")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"requested_model":"partial`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJSONL(root, 1024); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("incomplete shard remains: %v", err)
	}
	if _, err := os.Stat(strings.TrimSuffix(path, ".open") + ".jsonl"); !os.IsNotExist(err) {
		t.Fatalf("incomplete shard was finalized: %v", err)
	}
}
