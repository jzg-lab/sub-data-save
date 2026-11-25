package archive

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const DefaultShardLimit int64 = 10_000_000_000

type JSONLWriter struct {
	root  string
	limit int64
	mu    sync.Mutex
	open  map[shardKey]*jsonlShard
}

type shardKey struct{ date, model string }

type jsonlShard struct {
	path string
	file *os.File
	size int64
}

func OpenJSONL(root string, limit int64) (*JSONLWriter, error) {
	if limit < 1 {
		return nil, errors.New("JSONL shard limit must be positive")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, fmt.Errorf("create JSONL directory: %w", err)
	}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".open") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("JSONL shard is not a regular file: %s", path)
		}
		if info.Size() > limit {
			return fmt.Errorf("JSONL shard exceeds limit: %s", path)
		}
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("open interrupted JSONL shard: %w", err)
		}
		lastNewline := int64(-1)
		buffer := make([]byte, 32*1024)
		for offset := int64(0); offset < info.Size(); {
			n, readErr := file.ReadAt(buffer, offset)
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				_ = file.Close()
				return fmt.Errorf("read interrupted JSONL shard: %w", readErr)
			}
			if index := bytes.LastIndexByte(buffer[:n], '\n'); index >= 0 {
				lastNewline = offset + int64(index)
			}
			offset += int64(n)
		}
		if lastNewline < 0 {
			if err := file.Close(); err != nil {
				return fmt.Errorf("close interrupted JSONL shard: %w", err)
			}
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove incomplete JSONL shard: %w", err)
			}
			return nil
		}
		if err := file.Truncate(lastNewline + 1); err != nil {
			_ = file.Close()
			return fmt.Errorf("truncate incomplete JSONL record: %w", err)
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return fmt.Errorf("sync recovered JSONL shard: %w", err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close recovered JSONL shard: %w", err)
		}
		if err := os.Rename(path, strings.TrimSuffix(path, ".open")+".jsonl"); err != nil {
			return fmt.Errorf("recover interrupted JSONL shard: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return &JSONLWriter{root: root, limit: limit, open: make(map[shardKey]*jsonlShard)}, nil
}

// Append writes each complete record as one JSONL line and returns any closed shards.
func (w *JSONLWriter) Append(record []byte, completedAt time.Time) ([]string, error) {
	var fields struct {
		RequestedModel string `json:"requested_model"`
		Model          string `json:"model"`
	}
	if err := json.Unmarshal(record, &fields); err != nil {
		return nil, err
	}
	requestedModel := fields.RequestedModel
	if strings.TrimSpace(requestedModel) == "" {
		requestedModel = fields.Model
	}
	if strings.TrimSpace(requestedModel) == "" {
		return nil, errors.New("record has missing model")
	}
	line := bytes.TrimSpace(record)
	if bytes.ContainsAny(line, "\r\n") {
		var compact bytes.Buffer
		if err := json.Compact(&compact, line); err != nil {
			return nil, err
		}
		line = compact.Bytes()
	}
	size := int64(len(line)) + 1
	if size > w.limit {
		return nil, errors.New("JSONL record exceeds shard limit")
	}
	date := completedAt.UTC().Format("2006-01-02")
	key := shardKey{date, requestedModel}
	w.mu.Lock()
	defer w.mu.Unlock()
	var closed []string
	for activeKey, shard := range w.open {
		if activeKey.date < date {
			path, err := w.closeShard(activeKey, shard)
			if err != nil {
				return closed, err
			}
			closed = append(closed, path)
		}
	}
	shard := w.open[key]
	if shard != nil && (shard.file == nil || shard.size+size > w.limit) {
		path, err := w.closeShard(key, shard)
		if err != nil {
			return closed, err
		}
		closed = append(closed, path)
		shard = nil
	}
	if shard == nil {
		var err error
		shard, err = w.createShard(date, requestedModel)
		if err != nil {
			return closed, err
		}
		w.open[key] = shard
	}
	_, err := shard.file.WriteAt(line, shard.size)
	if err == nil {
		_, err = shard.file.WriteAt([]byte{'\n'}, shard.size+int64(len(line)))
	}
	if err == nil {
		err = shard.file.Sync()
	}
	if err != nil {
		return closed, fmt.Errorf("append JSONL record: %w", errors.Join(err, shard.file.Truncate(shard.size)))
	}
	shard.size += size
	if shard.size == w.limit {
		path, err := w.closeShard(key, shard)
		if err != nil {
			return closed, err
		}
		closed = append(closed, path)
	}
	return closed, nil
}

// SealBefore finalizes UTC dates before cutoff, or all shards for a zero cutoff.
func (w *JSONLWriter) SealBefore(cutoff time.Time) ([]string, error) {
	date := cutoff.UTC().Format("2006-01-02")
	w.mu.Lock()
	defer w.mu.Unlock()
	var closed []string
	for key, shard := range w.open {
		if !cutoff.IsZero() && key.date >= date {
			continue
		}
		path, err := w.closeShard(key, shard)
		if err != nil {
			return closed, err
		}
		closed = append(closed, path)
	}
	return closed, nil
}

func (w *JSONLWriter) createShard(date, model string) (*jsonlShard, error) {
	modelDir := url.PathEscape(model)
	if modelDir == "." || modelDir == ".." {
		modelDir = strings.ReplaceAll(modelDir, ".", "%2E")
	}
	if len(modelDir) > 214 {
		return nil, errors.New("encoded model exceeds shard filename limit")
	}
	dir := filepath.Join(w.root, date, modelDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create JSONL model directory: %w", err)
	}
	created := time.Now().UTC()
	for index := 1; ; index++ {
		name := fmt.Sprintf("%s-%s-%s-%06d", modelDir, date, created.Format("150405.000000000"), index)
		path := filepath.Join(dir, name+".open")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("create JSONL shard: %w", err)
		}
		return &jsonlShard{path: path, file: file}, nil
	}
}

func (w *JSONLWriter) closeShard(key shardKey, shard *jsonlShard) (string, error) {
	if shard.file != nil {
		err := shard.file.Close()
		shard.file = nil
		if err != nil {
			return "", fmt.Errorf("close JSONL shard: %w", err)
		}
	}
	path := strings.TrimSuffix(shard.path, ".open") + ".jsonl"
	if err := os.Rename(shard.path, path); err != nil {
		return "", fmt.Errorf("finalize JSONL shard: %w", err)
	}
	delete(w.open, key)
	return path, nil
}

// Pending returns finalized shards that have not yet been removed after upload.
func (w *JSONLWriter) Pending() ([]string, error) {
	var paths []string
	err := filepath.WalkDir(w.root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	return paths, err
}

func S3ObjectKey(root, path string) (string, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", errors.New("JSONL shard is outside archive root")
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	if len(parts) != 3 || !strings.HasSuffix(parts[2], ".jsonl") {
		return "", errors.New("invalid JSONL shard path")
	}
	return strings.Join(parts, "/"), nil
}

func MarkUploaded(path string) error {
	return os.Remove(path)
}
