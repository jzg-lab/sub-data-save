package archive

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestBuildDailyZIPAcrossBeijingNoons(t *testing.T) {
	root := t.TempDir()
	beijing := time.FixedZone("Beijing", 8*60*60)
	firstCutoff := time.Date(2026, 5, 2, 12, 0, 0, 0, beijing)
	firstContents := []byte("{ \"first\": true }\n")
	exactContents := []byte("{\"at_boundary\":true}")
	lateContents := []byte("{\"late\":true}")

	writeDailyRecord(t, root, "2026/05/01/instance-a/first.json", firstContents, firstCutoff.Add(-time.Nanosecond))
	writeDailyRecord(t, root, "2026/05/01/instance-a/exact.json", exactContents, firstCutoff)
	writeDailyNoise(t, root)

	if err := BuildDailyZIP(root, firstCutoff, func(string, uint64) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	firstZIP := filepath.Join(root, "daily", "2026-05-02.zip")
	assertDailyZIP(t, firstZIP, map[string][]byte{
		"2026/05/01/instance-a/first.json": firstContents,
	})
	firstPack, err := os.ReadFile(firstZIP)
	if err != nil {
		t.Fatal(err)
	}

	secondCutoff := firstCutoff.AddDate(0, 0, 1)
	// This saved record has an older logical UTC path, but was not committed by the first noon.
	writeDailyRecord(t, root, "2026/04/30/instance-b/late.json", lateContents, firstCutoff.Add(time.Second))
	secondBoundary := []byte(`{"second_boundary":true}`)
	writeDailyRecord(t, root, "2026/05/02/instance-a/second-boundary.json", secondBoundary, secondCutoff)
	secondZIP := filepath.Join(root, "daily", "2026-05-03.zip")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- BuildDailyZIP(root, secondCutoff, func(string, uint64) (bool, error) { return true, nil })
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertDailyZIP(t, secondZIP, map[string][]byte{
		"2026/05/01/instance-a/exact.json": exactContents,
		"2026/04/30/instance-b/late.json":  lateContents,
	})

	packAfterSecond, err := os.ReadFile(firstZIP)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packAfterSecond, firstPack) {
		t.Fatal("earlier daily ZIP changed while building the next day's pack")
	}

	secondPack, err := os.ReadFile(secondZIP)
	if err != nil {
		t.Fatal(err)
	}
	packAfterRerun, err := os.ReadFile(firstZIP)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packAfterRerun, firstPack) {
		t.Fatal("rerunning the next day's builder changed the earlier ZIP")
	}
	secondPackAfterRerun, err := os.ReadFile(secondZIP)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(secondPackAfterRerun, secondPack) {
		t.Fatal("rerunning the same cutoff changed its daily ZIP")
	}
}

func TestBuildDailyZIPRetriesAfterDiskBudgetFailure(t *testing.T) {
	root := t.TempDir()
	cutoff := time.Date(2026, 5, 2, 12, 0, 0, 0, time.FixedZone("Beijing", 8*60*60))
	contents := []byte(`{"retry":true}`)
	writeDailyRecord(t, root, "2026/05/01/instance-a/record.json", contents, cutoff.Add(-time.Minute))

	denied := errors.New("disk guard denied archive reserve")
	if err := BuildDailyZIP(root, cutoff, func(string, uint64) (bool, error) { return false, denied }); err == nil {
		t.Fatal("BuildDailyZIP succeeded while the disk guard denied space")
	}
	path := filepath.Join(root, "daily", "2026-05-02.zip")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ZIP after denied attempt: stat error = %v, want not-exist", err)
	}
	if err := BuildDailyZIP(root, cutoff, func(string, uint64) (bool, error) { return true, nil }); err != nil {
		t.Fatalf("retry after disk guard permits writes: %v", err)
	}
	assertDailyZIP(t, path, map[string][]byte{
		"2026/05/01/instance-a/record.json": contents,
	})
}

func TestBuildDailyZIPAllowsEmptyDay(t *testing.T) {
	root := t.TempDir()
	cutoff := time.Date(2026, 5, 2, 12, 0, 0, 0, time.FixedZone("Beijing", 8*60*60))
	if err := BuildDailyZIP(root, cutoff, func(string, uint64) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	assertDailyZIP(t, filepath.Join(root, "daily", "2026-05-02.zip"), map[string][]byte{})
}

func writeDailyRecord(t *testing.T, root, relative string, contents []byte, modified time.Time) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

func writeDailyNoise(t *testing.T, root string) {
	t.Helper()
	modified := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for relative, contents := range map[string]string{
		"jsonl/2026-05-01/model/shard.jsonl":             "{\"ignored\":true}\n",
		"2026/05/01/instance-a/.pending-record.json":     "{\"temporary\":true}",
		"2026/05/01/instance-a/.pending-record.json.tmp": "{\"temporary\":true}",
		"2026/05/01/instance-a/notes.txt":                "not a record",
		"2026/05/01/invalid instance/record.json":        "{\"invalid_path\":true}",
		"2026/05/01/instance-a/not-a-record.json.backup": "{\"backup\":true}",
	} {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
}

func assertDailyZIP(t *testing.T, path string, want map[string][]byte) {
	t.Helper()
	reader, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	got := make(map[string][]byte, len(reader.File))
	for _, file := range reader.File {
		if _, duplicate := got[file.Name]; duplicate {
			t.Fatalf("duplicate ZIP entry %q", file.Name)
		}
		entry, err := file.Open()
		if err != nil {
			t.Fatalf("open ZIP entry %q: %v", file.Name, err)
		}
		contents, readErr := io.ReadAll(entry)
		closeErr := entry.Close()
		if readErr != nil {
			t.Fatalf("read ZIP entry %q: %v", file.Name, readErr)
		}
		if closeErr != nil {
			t.Fatalf("close ZIP entry %q: %v", file.Name, closeErr)
		}
		got[file.Name] = contents
	}
	if len(got) != len(want) {
		gotNames := make([]string, 0, len(got))
		wantNames := make([]string, 0, len(want))
		for name := range got {
			gotNames = append(gotNames, name)
		}
		for name := range want {
			wantNames = append(wantNames, name)
		}
		sort.Strings(gotNames)
		sort.Strings(wantNames)
		t.Fatalf("ZIP entries = %v, want exactly %v", gotNames, wantNames)
	}
	for name, contents := range want {
		if !reflect.DeepEqual(got[name], contents) {
			t.Errorf("ZIP entry %q contents = %q, want exact bytes %q", name, got[name], contents)
		}
	}
}
