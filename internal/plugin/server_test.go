package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/jzg-lab/sub-data-save/pkg/pluginapi/v1"
)

func TestCaptureRecordSavesJSONAndRejectsInvalidJSON(t *testing.T) {
	directory := t.TempDir()
	server := New()
	config := []byte(`{"directory":"` + directory + `"}`)
	if response, err := server.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: config}); err != nil || !response.GetApplied() {
		t.Fatalf("ApplyConfig() = (%v, %v)", response, err)
	}
	completedAt := time.Date(2026, 11, 11, 1, 0, 0, 0, time.UTC)
	request := &pluginv1.CaptureRecordRequest{
		RecordId: "record-1", InstanceId: "instance-1", CompletedAtUnixNano: completedAt.UnixNano(),
		RecordJson: []byte(`{ "response": "kept exactly" }`),
	}
	if response, err := server.CaptureRecord(context.Background(), request); err != nil || !response.GetAccepted() {
		t.Fatalf("CaptureRecord() = (%v, %v)", response, err)
	}
	path := filepath.Join(directory, "2026", "11", "11", "instance-1", "record-1.json")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(request.GetRecordJson()) {
		t.Fatalf("stored bytes = %q, want %q", got, request.GetRecordJson())
	}
	listed, err := server.ListCaptureRecords(context.Background(), &pluginv1.ListCaptureRecordsRequest{Date: "2026-11-11"})
	if err != nil || len(listed.GetRecords()) != 1 || listed.GetRecords()[0].GetRecordId() != "record-1" {
		t.Fatalf("ListCaptureRecords() = (%v, %v)", listed, err)
	}
	downloaded, err := server.DownloadCaptureRecord(context.Background(), &pluginv1.DownloadCaptureRecordRequest{
		Date: "2026-11-11", InstanceId: "instance-1", RecordId: "record-1",
	})
	if err != nil || string(downloaded.GetRecordJson()) != string(request.GetRecordJson()) {
		t.Fatalf("DownloadCaptureRecord() = (%v, %v)", downloaded, err)
	}
	request.RecordId = "record-2"
	request.RecordJson = []byte(`{"partial":`)
	if response, err := server.CaptureRecord(context.Background(), request); err != nil || response.GetAccepted() {
		t.Fatalf("CaptureRecord(invalid JSON) = (%v, %v), want rejected", response, err)
	}
}

func TestDefaultConfigurationAndHandshakeHealth(t *testing.T) {
	state := t.TempDir()
	t.Setenv("SUB2API_PLUGIN_STATE_DIR", state)
	server := New()
	health, err := server.Health(context.Background(), &pluginv1.HealthRequest{})
	if err != nil || !health.GetHealthy() {
		t.Fatalf("pre-config handshake: %v %v", health, err)
	}
	response, err := server.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: []byte(`{}`)})
	if err != nil || !response.GetApplied() {
		t.Fatalf("default config: %v %v", response, err)
	}
	info, err := os.Stat(filepath.Join(state, "archive"))
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("persistent default directory: %v %v", info, err)
	}
	if _, err := parseConfig([]byte(`{"unknown":true}`)); err == nil {
		t.Fatal("accepted unknown config field")
	}
	t.Setenv("SUB2API_PLUGIN_STATE_DIR", "")
	if _, err := parseConfig([]byte(`{}`)); err == nil {
		t.Fatal("silently chose a nonpersistent working directory")
	}
}

func TestReapplyingConfigDoesNotRemoveInflightArchives(t *testing.T) {
	t.Setenv("SUB2API_PLUGIN_STATE_DIR", t.TempDir())
	server := New()
	configuration := &pluginv1.ApplyConfigRequest{ConfigJson: []byte(`{}`)}
	result, err := server.ApplyConfig(context.Background(), configuration)
	if err != nil || !result.GetApplied() {
		t.Fatalf("initial config: %v %v", result, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			reply, err := server.CaptureRecord(context.Background(), &pluginv1.CaptureRecordRequest{RecordId: fmt.Sprintf("record-%d", i), InstanceId: "gateway", CompletedAtUnixNano: time.Now().UnixNano(), RecordJson: []byte(`{"response":"ok"}`)})
			if err != nil || !reply.GetAccepted() {
				t.Errorf("capture: %v %v", reply, err)
			}
		}(i)
		go func() {
			defer wg.Done()
			reply, err := server.ApplyConfig(context.Background(), configuration)
			if err != nil || !reply.GetApplied() {
				t.Errorf("config: %v %v", reply, err)
			}
		}()
	}
	wg.Wait()
	records, err := server.ListCaptureRecords(context.Background(), &pluginv1.ListCaptureRecordsRequest{Date: time.Now().UTC().Format("2006-01-02")})
	if err != nil || len(records.GetRecords()) != 12 {
		t.Fatalf("lost archives: %v %v", records, err)
	}
}
