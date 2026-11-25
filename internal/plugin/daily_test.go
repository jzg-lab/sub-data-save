package plugin

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	pluginv1 "github.com/jzg-lab/sub-data-save/pkg/pluginapi/v1"
)

func TestDailyPickupAtBeijingNoon(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newCaptureTestServer()
		root := t.TempDir()
		cfg := []byte(fmt.Sprintf(`{"directory":%q,"group_ids":[7],"models":["sample"]}`, root))
		reply, err := s.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: cfg})
		if err != nil || !reply.GetApplied() {
			t.Fatal("configure archive", err)
		}
		defer func() { s.mu.Lock(); defer s.mu.Unlock(); s.dailyTimer.Stop() }()
		time.Sleep(time.Second)
		synctest.Wait()
		now := time.Now()
		next := latestNoon(now).AddDate(0, 0, 1)
		if next.UTC().Hour() != 4 {
			t.Fatal("expected Beijing noon")
		}
		record := []byte(`{"schema_version":2,"group_id":7,"requested_model":"sample"}`)
		saved, err := s.CaptureRecord(captureContext("sample"), &pluginv1.CaptureRecordRequest{RecordId: "sample", InstanceId: "gateway", CompletedAtUnixNano: now.UnixNano(), RecordJson: record})
		if err != nil || !saved.GetAccepted() {
			t.Fatal("save fixture", err)
		}
		raw := filepath.Join(root, archiveGeneration, now.UTC().Format("2006/01/02"), "gateway", "sample.json")
		if err = os.Chtimes(raw, now, now); err != nil {
			t.Fatal(err)
		}
		name := filepath.Join(root, archiveGeneration, "daily", next.Format("2006-01-02")+".zip")
		if _, err = os.Stat(name); !os.IsNotExist(err) {
			t.Fatal("published before noon", err)
		}
		time.Sleep(time.Until(next))
		synctest.Wait()
		r, err := zip.OpenReader(name)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if len(r.File) != 1 || filepath.Base(r.File[0].Name) != "sample.json" {
			t.Fatal("new record missing from noon pickup")
		}
	})
}
