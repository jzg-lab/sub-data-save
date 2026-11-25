package archive

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const dailyReserve = uint64(20 << 30)

var dailyDatePattern = regexp.MustCompile(`^[0-9]{4}/[0-9]{2}/[0-9]{2}/[A-Za-z0-9][A-Za-z0-9._-]{0,127}/[A-Za-z0-9][A-Za-z0-9._-]{0,127}\.json$`)

// BuildDailyZIP publishes all eligible raw records not already present in an earlier daily pack.
func BuildDailyZIP(root string, cutoff time.Time, hasRoom func(string, uint64) (bool, error)) error {
	if hasRoom == nil {
		return errors.New("daily ZIP disk-space check is required")
	}
	beijing := time.FixedZone("UTC+8", 8*60*60)
	day := cutoff.In(beijing).Format("2006-01-02")
	if err := requireRealDirectory(root); err != nil {
		return err
	}
	dailyDir := filepath.Join(root, "daily")
	if err := os.MkdirAll(dailyDir, 0700); err != nil {
		return fmt.Errorf("create daily ZIP directory: %w", err)
	}
	if err := requireRealDirectory(dailyDir); err != nil {
		return err
	}
	lock, err := os.Open(dailyDir)
	if err != nil {
		return fmt.Errorf("open daily ZIP directory lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock daily ZIP directory: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	output := filepath.Join(dailyDir, day+".zip")
	if info, err := os.Lstat(output); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("daily ZIP destination is not a regular file")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect daily ZIP destination: %w", err)
	}
	if err := cleanDailyTemps(dailyDir); err != nil {
		return err
	}
	if err := requireRealDirectory(root); err != nil {
		return err
	}

	covered := make(map[string]struct{})
	entries, err := os.ReadDir(dailyDir)
	if err != nil {
		return fmt.Errorf("list prior daily ZIPs: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !dailyPackDate(name) || name >= day+".zip" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect prior daily ZIP %q: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		reader, err := zip.OpenReader(filepath.Join(dailyDir, name))
		if err != nil {
			return fmt.Errorf("read prior daily ZIP %q: %w", name, err)
		}
		for _, file := range reader.File {
			if validDailyEntry(file.Name) {
				covered[file.Name] = struct{}{}
			}
		}
		if err := reader.Close(); err != nil {
			return fmt.Errorf("close prior daily ZIP %q: %w", name, err)
		}
	}

	var candidates []dailyFile
	var estimate uint64
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "daily" || rel == "jsonl" {
			return filepath.SkipDir
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !validDailyEntry(rel) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			return nil
		}
		if _, ok := covered[rel]; ok {
			return nil
		}
		size := uint64(info.Size())
		maxUint := ^uint64(0)
		if size > (maxUint-1024)/2 || estimate > maxUint-(2*size+1024) {
			return errors.New("daily ZIP size estimate overflow")
		}
		estimate += 2*size + 1024
		candidates = append(candidates, dailyFile{path: path, name: rel, size: info.Size(), cutoff: cutoff})
		return nil
	})
	if err != nil {
		return fmt.Errorf("scan raw records for daily ZIP: %w", err)
	}
	if estimate > ^uint64(0)-dailyReserve {
		return errors.New("daily ZIP space estimate overflow")
	}
	room, err := hasRoom(dailyDir, dailyReserve+estimate)
	if err != nil {
		return fmt.Errorf("check daily ZIP disk space: %w", err)
	}
	if !room {
		return errors.New("insufficient disk space for daily ZIP")
	}

	tmp, err := os.CreateTemp(dailyDir, ".daily-*.tmp")
	if err != nil {
		return fmt.Errorf("create daily ZIP temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	zw := zip.NewWriter(tmp)
	for _, candidate := range candidates {
		if err := writeDailyFile(zw, candidate); err != nil {
			_ = zw.Close()
			_ = tmp.Close()
			return err
		}
	}
	if err := zw.Close(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("finish daily ZIP: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync daily ZIP: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close daily ZIP: %w", err)
	}
	if err := os.Rename(tmpPath, output); err != nil {
		return fmt.Errorf("publish daily ZIP: %w", err)
	}
	dir, err := os.Open(dailyDir)
	if err != nil {
		return fmt.Errorf("open daily ZIP directory for sync: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("sync daily ZIP directory: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close daily ZIP directory: %w", err)
	}
	return nil
}

type dailyFile struct {
	path   string
	name   string
	size   int64
	cutoff time.Time
}

func validDailyEntry(name string) bool {
	if !dailyDatePattern.MatchString(name) {
		return false
	}
	parts := strings.Split(name, "/")
	day, err := time.Parse("2006/01/02", strings.Join(parts[:3], "/"))
	return err == nil && day.Format("2006/01/02") == strings.Join(parts[:3], "/")
}

func dailyPackDate(name string) bool {
	if len(name) != len("2006-01-02.zip") || !strings.HasSuffix(name, ".zip") {
		return false
	}
	day, err := time.Parse("2006-01-02", strings.TrimSuffix(name, ".zip"))
	return err == nil && day.Format("2006-01-02")+".zip" == name
}

func cleanDailyTemps(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("list abandoned daily ZIP temporaries: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".daily-") || !strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect abandoned daily ZIP temporary %q: %w", entry.Name(), err)
		}
		if info.Mode().IsRegular() {
			if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil {
				return fmt.Errorf("remove abandoned daily ZIP temporary %q: %w", entry.Name(), err)
			}
		}
	}
	return nil
}

func requireRealDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect archive directory %q: %w", path, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("archive directory %q is not a real directory", path)
	}
	return nil
}

func writeDailyFile(writer *zip.Writer, file dailyFile) error {
	input, err := os.OpenFile(file.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open raw record %q: %w", file.name, err)
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return fmt.Errorf("inspect raw record %q: %w", file.name, err)
	}
	if !info.Mode().IsRegular() || info.Size() != file.size || !info.ModTime().Before(file.cutoff) {
		return fmt.Errorf("raw record %q changed while building daily ZIP", file.name)
	}
	header := &zip.FileHeader{Name: file.name, Method: zip.Deflate}
	header.SetMode(0600)
	out, err := writer.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("create ZIP entry %q: %w", file.name, err)
	}
	if _, err := io.Copy(out, input); err != nil {
		return fmt.Errorf("copy raw record %q into daily ZIP: %w", file.name, err)
	}
	return nil
}
