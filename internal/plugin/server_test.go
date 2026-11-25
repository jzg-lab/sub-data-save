package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jzg-lab/sub-data-save/internal/archive"
	pluginv1 "github.com/jzg-lab/sub-data-save/pkg/pluginapi/v1"
)

func TestCaptureRecordSavesJSONAndRejectsInvalidJSON(t *testing.T) {
	directory := t.TempDir()
	server := New()
	config := []byte(`{"directory":"` + directory + `","group_ids":[7],"models":["Opus5.5"]}`)
	if response, err := server.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: config}); err != nil || !response.GetApplied() {
		t.Fatalf("ApplyConfig() = (%v, %v)", response, err)
	}
	completedAt := time.Date(2026, 11, 11, 1, 0, 0, 0, time.UTC)
	request := &pluginv1.CaptureRecordRequest{
		RecordId: "record-1", InstanceId: "instance-1", CompletedAtUnixNano: completedAt.UnixNano(),
		RecordJson: []byte(`{"group_id":7,"requested_model":"Opus5.5","model":"claude-opus","response":"kept exactly"}`),
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

func TestParseConfigPreservesAndNormalizesCaptureFilters(t *testing.T) {
	cfg, err := parseConfig([]byte(`{"directory":"/tmp/archive","group_ids":[9,7,9],"models":[" Opus5.5 ","Haiku","Opus5.5"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(cfg.GroupIDs) != "[7 9]" || fmt.Sprint(cfg.Models) != "[Haiku Opus5.5]" {
		t.Fatalf("normalized filters = groups %v models %v", cfg.GroupIDs, cfg.Models)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := parseConfig(raw)
	if err != nil || fmt.Sprint(reloaded.GroupIDs) != "[7 9]" || fmt.Sprint(reloaded.Models) != "[Haiku Opus5.5]" {
		t.Fatalf("filter config round trip = %+v, %v", reloaded, err)
	}
}

func TestParseConfigKeepsMissingFiltersEmpty(t *testing.T) {
	cfg, err := parseConfig([]byte(`{"directory":"/tmp/archive"}`))
	if err != nil || len(cfg.GroupIDs) != 0 || len(cfg.Models) != 0 {
		t.Fatalf("legacy config filters = %+v, %v", cfg, err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil || !strings.Contains(string(raw), `"group_ids":[]`) || !strings.Contains(string(raw), `"models":[]`) {
		t.Fatalf("normalized legacy config = %s, %v", raw, err)
	}
}

func TestCaptureRecordRequiresBothConfiguredFilters(t *testing.T) {
	directory := t.TempDir()
	server := New()
	configuration := &pluginv1.ApplyConfigRequest{ConfigJson: []byte(`{"directory":"` + directory + `","group_ids":[7],"models":["Opus5.5"]}`)}
	if response, err := server.ApplyConfig(context.Background(), configuration); err != nil || !response.GetApplied() {
		t.Fatalf("ApplyConfig() = (%v, %v)", response, err)
	}
	stamp := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		json string
		want bool
	}{
		{name: "matching group and model", json: `{"group_id":7,"requested_model":"Opus5.5"}`, want: true},
		{name: "other group", json: `{"group_id":8,"requested_model":"Opus5.5"}`},
		{name: "other model", json: `{"group_id":7,"requested_model":"Haiku"}`},
		{name: "missing group", json: `{"requested_model":"Opus5.5"}`},
		{name: "missing model", json: `{"group_id":7}`},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := server.CaptureRecord(context.Background(), &pluginv1.CaptureRecordRequest{
				RecordId: fmt.Sprintf("filter-%d", i), InstanceId: "gateway", CompletedAtUnixNano: stamp.Add(time.Duration(i)).UnixNano(), RecordJson: []byte(tc.json),
			})
			if err != nil || reply.GetAccepted() != tc.want {
				t.Fatalf("CaptureRecord() = (%v, %v), want accepted=%t", reply, err, tc.want)
			}
		})
	}
	listed, err := server.ListCaptureRecords(context.Background(), &pluginv1.ListCaptureRecordsRequest{Date: "2026-11-20"})
	if err != nil || len(listed.GetRecords()) != 1 || listed.GetRecords()[0].GetRecordId() != "filter-0" {
		t.Fatalf("records = (%v, %v), want only the matching group/model record", listed, err)
	}
}

func TestMissingCaptureFiltersFailClosed(t *testing.T) {
	directory := t.TempDir()
	server := New()
	if response, err := server.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: []byte(`{"directory":"` + directory + `"}`)}); err != nil || !response.GetApplied() {
		t.Fatalf("ApplyConfig() = (%v, %v)", response, err)
	}
	reply, err := server.CaptureRecord(context.Background(), &pluginv1.CaptureRecordRequest{
		RecordId: "legacy-empty-filters", InstanceId: "gateway", CompletedAtUnixNano: time.Now().UnixNano(),
		RecordJson: []byte(`{"group_id":7,"requested_model":"Opus5.5"}`),
	})
	if err != nil || reply.GetAccepted() {
		t.Fatalf("CaptureRecord() = (%v, %v), want missing filters rejected", reply, err)
	}
}

func TestParseConfigRejectsInvalidGroupIDs(t *testing.T) {
	if _, err := parseConfig([]byte(`{"directory":"/tmp/archive","group_ids":[0],"models":["Opus5.5"]}`)); err == nil {
		t.Fatal("accepted non-positive group ID")
	}
}

func TestS3ConfigRequiresBucketAndRegionTogether(t *testing.T) {
	for _, raw := range []string{
		`{"directory":"/tmp/archive","bucket":"incoming"}`,
		`{"directory":"/tmp/archive","region":"ap-east-1"}`,
	} {
		if _, err := parseConfig([]byte(raw)); err == nil {
			t.Fatalf("accepted incomplete S3 configuration: %s", raw)
		}
	}
	cfg, err := parseConfig([]byte(`{"directory":"/tmp/archive","bucket":"incoming-04-20260624","region":"ap-east-1"}`))
	if err != nil || cfg.Bucket != "incoming-04-20260624" || cfg.Region != "ap-east-1" {
		t.Fatalf("valid S3 configuration = %+v, %v", cfg, err)
	}
}

func TestRawArchiveLimitDoesNotBlockJSONL(t *testing.T) {
	directory := t.TempDir()
	store, err := archive.Open(directory, archive.Limits{MaxRecords: 1, MaxBytes: 1024, MaxRecordBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	jsonl, err := archive.OpenJSONL(filepath.Join(directory, "jsonl"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, jsonl: jsonl, cfg: config{GroupIDs: []int64{7}, Models: []string{"opus"}}}
	for i := range 2 {
		reply, err := server.CaptureRecord(context.Background(), &pluginv1.CaptureRecordRequest{
			RecordId: fmt.Sprintf("quota-%d", i), InstanceId: "gateway", CompletedAtUnixNano: time.Now().UnixNano(),
			RecordJson: []byte(`{"group_id":7,"requested_model":"opus","model":"opus"}`),
		})
		if err != nil || !reply.GetAccepted() {
			t.Fatalf("capture %d = %v, %v", i, reply, err)
		}
	}
	if count, _ := store.Usage(); count != 1 {
		t.Fatalf("raw archive count = %d, want quota of 1", count)
	}
	closed, err := jsonl.SealBefore(time.Time{})
	if err != nil || len(closed) != 1 {
		t.Fatalf("JSONL shard close = %v, %v", closed, err)
	}
	data, err := os.ReadFile(closed[0])
	if err != nil || strings.Count(string(data), "\n") != 2 {
		t.Fatalf("JSONL lines after raw quota = %q, %v", data, err)
	}
}

func TestReapplyingConfigDoesNotRemoveInflightArchives(t *testing.T) {
	t.Setenv("SUB2API_PLUGIN_STATE_DIR", t.TempDir())
	server := New()
	configuration := &pluginv1.ApplyConfigRequest{ConfigJson: []byte(`{"group_ids":[7],"models":["test-model"]}`)}
	result, err := server.ApplyConfig(context.Background(), configuration)
	if err != nil || !result.GetApplied() {
		t.Fatalf("initial config: %v %v", result, err)
	}
	var wg sync.WaitGroup
	stamp := time.Now()
	for i := range 12 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			reply, err := server.CaptureRecord(context.Background(), &pluginv1.CaptureRecordRequest{RecordId: fmt.Sprintf("record-%d", i), InstanceId: "gateway", CompletedAtUnixNano: stamp.UnixNano(), RecordJson: []byte(`{"group_id":7,"response":"ok","requested_model":"test-model","model":"test-model"}`)})
			if err != nil || !reply.GetAccepted() {
				t.Errorf("capture: %v %v", reply, err)
			}
		}()
		go func() {
			defer wg.Done()
			reply, err := server.ApplyConfig(context.Background(), configuration)
			if err != nil || !reply.GetApplied() {
				t.Errorf("config: %v %v", reply, err)
			}
		}()
	}
	wg.Wait()
	records, err := server.ListCaptureRecords(context.Background(), &pluginv1.ListCaptureRecordsRequest{Date: stamp.UTC().Format("2006-01-02")})
	if err != nil || len(records.GetRecords()) != 12 {
		t.Fatalf("lost archives: %v %v", records, err)
	}
	closed, err := server.jsonl.SealBefore(time.Time{})
	if err != nil || len(closed) != 1 {
		t.Fatalf("reloading split or lost the active shard: %v, %v", closed, err)
	}
	data, err := os.ReadFile(closed[0])
	if err != nil || strings.Count(string(data), "\n") != 12 {
		t.Fatalf("lost JSONL records: %q, %v", data, err)
	}
}

type uploaderStub struct {
	err  error
	keys []string
}

func (u *uploaderStub) Upload(_ context.Context, input *s3.PutObjectInput) error {
	u.keys = append(u.keys, aws.ToString(input.Key))
	return u.err
}

func TestFlushAndUploadRemovesOnlySuccessfulShards(t *testing.T) {
	root := t.TempDir()
	jsonl, err := archive.OpenJSONL(filepath.Join(root, "jsonl"), 128)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jsonl.Append([]byte(`{"requested_model":"Opus5.5"}`), time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	closed, err := jsonl.SealBefore(time.Time{})
	if err != nil || len(closed) != 1 {
		t.Fatalf("seal shard = %v, %v", closed, err)
	}
	server := &Server{jsonl: jsonl, cfg: config{Directory: root, Bucket: "incoming-04-20260624"}}
	t.Cleanup(func() { server.uploadTimer.Stop() })
	failed := &uploaderStub{err: fmt.Errorf("upload failed")}
	server.s3 = failed
	server.flushAndUpload()
	if _, err := os.Stat(closed[0]); err != nil {
		t.Fatalf("failed upload removed local shard: %v", err)
	}
	uploaded := &uploaderStub{}
	server.s3 = uploaded
	server.flushAndUpload()
	if _, err := os.Stat(closed[0]); !os.IsNotExist(err) {
		t.Fatalf("successful upload retained local shard: %v", err)
	}
	if len(uploaded.keys) != 1 || !strings.HasPrefix(uploaded.keys[0], "2026-06-25/Opus5.5/Opus5.5-2026-06-25-") || !strings.HasSuffix(uploaded.keys[0], "-000001.jsonl") {
		t.Fatalf("uploaded keys = %v", uploaded.keys)
	}
}
