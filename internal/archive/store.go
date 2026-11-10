package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrLimitReached = errors.New("archive limit reached")
	ErrDuplicateID  = errors.New("archive record already exists")
	ErrInvalidJSON  = errors.New("archive record is not a complete JSON object")
	idPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

type Limits struct {
	MaxRecords     int
	MaxBytes       int64
	MaxRecordBytes int64
}

type Record struct {
	Date       string `json:"date"`
	InstanceID string `json:"instance_id"`
	RecordID   string `json:"record_id"`
	Size       int64  `json:"size"`
}

type Store struct {
	root   string
	limits Limits
	mu     sync.Mutex
	count  int
	bytes  int64
}

func Open(root string, limits Limits) (*Store, error) {
	if limits.MaxRecords < 1 || limits.MaxBytes < 1 || limits.MaxRecordBytes < 1 || limits.MaxRecordBytes > limits.MaxBytes {
		return nil, errors.New("archive limits must be positive and per-record bytes cannot exceed total bytes")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, fmt.Errorf("create archive directory: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect archive directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("archive path must be a real directory")
	}
	if err := os.Chmod(root, 0700); err != nil {
		return nil, fmt.Errorf("restrict archive directory permissions: %w", err)
	}
	store := &Store{root: root, limits: limits}
	if err := store.rebuildUsage(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Save(ctx context.Context, instanceID, recordID string, completedAt time.Time, data []byte) (string, error) {
	if !idPattern.MatchString(instanceID) || !idPattern.MatchString(recordID) {
		return "", errors.New("invalid archive instance or record ID")
	}
	if ctx == nil {
		return "", errors.New("archive context is required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if int64(len(data)) > s.limits.MaxRecordBytes {
		return "", ErrLimitReached
	}
	if len(data) == 0 || !json.Valid(data) || bytes.TrimSpace(data)[0] != '{' {
		return "", ErrInvalidJSON
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.count >= s.limits.MaxRecords || s.bytes+int64(len(data)) > s.limits.MaxBytes {
		return "", ErrLimitReached
	}
	date := completedAt.UTC()
	dir := filepath.Join(s.root, date.Format("2006"), date.Format("01"), date.Format("02"), instanceID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create archive record directory: %w", err)
	}
	path := filepath.Join(dir, recordID+".json")
	if _, err := os.Lstat(path); err == nil {
		return "", ErrDuplicateID
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect archive destination: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".pending-*")
	if err != nil {
		return "", fmt.Errorf("create temporary archive: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write temporary archive: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("sync temporary archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close temporary archive: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", fmt.Errorf("commit archive record: %w", err)
	}
	if dir, err := os.Open(dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	s.count++
	s.bytes += int64(len(data))
	return path, nil
}

func (s *Store) Usage() (records int, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count, s.bytes
}

func (s *Store) List(date string) ([]Record, error) {
	day, err := time.Parse("2006-01-02", date)
	if err != nil || day.Format("2006-01-02") != date {
		return nil, errors.New("invalid archive date")
	}
	root := filepath.Join(s.root, day.Format("2006"), day.Format("01"), day.Format("02"))
	var records []Record
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return filepath.SkipAll
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if len(parts) != 2 || !idPattern.MatchString(parts[0]) {
			return nil
		}
		recordID := strings.TrimSuffix(parts[1], ".json")
		if !idPattern.MatchString(recordID) {
			return nil
		}
		records = append(records, Record{Date: date, InstanceID: parts[0], RecordID: recordID, Size: info.Size()})
		return nil
	})
	sort.Slice(records, func(i, j int) bool {
		if records[i].InstanceID != records[j].InstanceID {
			return records[i].InstanceID < records[j].InstanceID
		}
		return records[i].RecordID < records[j].RecordID
	})
	return records, err
}

func (s *Store) Read(date, instanceID, recordID string) ([]byte, error) {
	day, err := time.Parse("2006-01-02", date)
	if err != nil || day.Format("2006-01-02") != date || !idPattern.MatchString(instanceID) || !idPattern.MatchString(recordID) {
		return nil, errors.New("invalid archive record identity")
	}
	path := filepath.Join(s.root, day.Format("2006"), day.Format("01"), day.Format("02"), instanceID, recordID+".json")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("archive record is not a regular file")
	}
	return os.ReadFile(path)
}

func (s *Store) rebuildUsage() error {
	return filepath.WalkDir(s.root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == s.root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive directory contains a symlink: %s", path)
		}
		if entry.IsDir() {
			if err := os.Chmod(path, 0700); err != nil {
				return fmt.Errorf("restrict archive directory permissions: %w", err)
			}
			return nil
		}
		if strings.HasPrefix(entry.Name(), ".pending-") {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove incomplete archive: %w", err)
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("archive entry is not a regular file: %s", path)
		}
		if err := os.Chmod(path, 0600); err != nil {
			return fmt.Errorf("restrict archive file permissions: %w", err)
		}
		s.count++
		s.bytes += info.Size()
		return nil
	})
}
