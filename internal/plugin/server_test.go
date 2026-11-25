package plugin

import (
	"context"
	"crypto/sha256"
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
	"google.golang.org/grpc/metadata"
)

func newCaptureTestServer() *Server {
	server := New()
	server.diskHasRoom = func(string, uint64) (bool, error) { return true, nil }
	return server
}

func captureContext(model string) context.Context {
	digest := sha256.Sum256([]byte(model))
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("capture-filter-model-bin", string(digest[:])))
}

func TestCaptureRecordSavesJSONAndRejectsInvalidJSON(t *testing.T) {
	directory := t.TempDir()
	server := newCaptureTestServer()
	config := []byte(`{"directory":"` + directory + `","group_ids":[7],"models":["Opus5.5"]}`)
	if response, err := server.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: config}); err != nil || !response.GetApplied() {
		t.Fatalf("ApplyConfig() = (%v, %v)", response, err)
	}
	completedAt := time.Date(2026, 11, 11, 1, 0, 0, 0, time.UTC)
	request := &pluginv1.CaptureRecordRequest{
		RecordId: "record-1", InstanceId: "instance-1", CompletedAtUnixNano: completedAt.UnixNano(),
		RecordJson: []byte(`{"schema_version":2,"group_id":7,"requested_model":"Opus5.5","model":"claude-opus","response":"kept exactly"}`),
	}
	if response, err := server.CaptureRecord(captureContext("Opus5.5"), request); err != nil || !response.GetAccepted() {
		t.Fatalf("CaptureRecord() = (%v, %v)", response, err)
	}
	path := filepath.Join(directory, archiveGeneration, "2026", "11", "11", "instance-1", "record-1.json")
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
	info, err := os.Stat(filepath.Join(state, "archive", archiveGeneration))
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
	server := newCaptureTestServer()
	configuration := &pluginv1.ApplyConfigRequest{ConfigJson: []byte(`{"directory":"` + directory + `","group_ids":[7],"models":["Opus5.5"]}`)}
	if response, err := server.ApplyConfig(context.Background(), configuration); err != nil || !response.GetApplied() {
		t.Fatalf("ApplyConfig() = (%v, %v)", response, err)
	}
	stamp := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		json  string
		model string
		want  bool
	}{
		{name: "matching original model", model: "Opus5.5", json: `{"schema_version":2,"group_id":7,"requested_model":"中转"}`, want: true},
		{name: "other group", model: "Opus5.5", json: `{"schema_version":2,"group_id":8,"requested_model":"中转"}`},
		{name: "other original model", model: "Haiku", json: `{"schema_version":2,"group_id":7,"requested_model":"Opus5.5"}`},
		{name: "missing group", model: "Opus5.5", json: `{"schema_version":2,"requested_model":"Opus5.5"}`},
		{name: "missing archive model", model: "Opus5.5", json: `{"schema_version":2,"group_id":7}`},
		{name: "legacy producer", model: "Opus5.5", json: `{"schema_version":1,"group_id":7,"requested_model":"Opus5.5"}`},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := server.CaptureRecord(captureContext(tc.model), &pluginv1.CaptureRecordRequest{
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

func TestRawArchiveLimitStopsJSONL(t *testing.T) {
	directory := t.TempDir()
	store, err := archive.Open(directory, archive.Limits{MaxRecords: 1, MaxBytes: 1024, MaxRecordBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	jsonl, err := archive.OpenJSONL(filepath.Join(directory, "jsonl"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	server := newCaptureTestServer()
	server.store, server.jsonl = store, jsonl
	server.cfg = config{Directory: directory, GroupIDs: []int64{7}, Models: []string{"opus"}}
	for i := range 2 {
		reply, err := server.CaptureRecord(captureContext("opus"), &pluginv1.CaptureRecordRequest{
			RecordId: fmt.Sprintf("quota-%d", i), InstanceId: "gateway", CompletedAtUnixNano: time.Now().UnixNano(),
			RecordJson: []byte(`{"schema_version":2,"group_id":7,"requested_model":"opus","model":"opus"}`),
		})
		if err != nil || reply.GetAccepted() != (i == 0) {
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
	if err != nil || strings.Count(string(data), "\n") != 1 {
		t.Fatalf("JSONL lines after raw quota = %q, %v", data, err)
	}
}

func TestCapturePausesBeforeBothWritesAndResumes(t *testing.T) {
	ctx := captureContext("opus")
	directory := t.TempDir()
	server := New()
	configJSON := fmt.Sprintf(`{"directory":%q,"group_ids":[7],"models":["opus"]}`, directory)
	if reply, err := server.ApplyConfig(ctx, &pluginv1.ApplyConfigRequest{ConfigJson: []byte(configJSON)}); err != nil || !reply.GetApplied() {
		t.Fatalf("ApplyConfig() = (%v, %v)", reply, err)
	}
	request := &pluginv1.CaptureRecordRequest{
		RecordId: "disk-guard", InstanceId: "gateway", CompletedAtUnixNano: time.Now().UnixNano(),
		RecordJson: []byte(`{"schema_version":2,"group_id":7,"requested_model":"opus"}`),
	}
	const reserve = 20 << 30
	needed := uint64(reserve + 2*len(request.GetRecordJson()) + 1)
	directory = filepath.Join(directory, archiveGeneration)
	jsonlDirectory := filepath.Join(directory, "jsonl")
	available := uint64(reserve)
	var diskErr error
	server.diskHasRoom = func(path string, required uint64) (bool, error) {
		switch path {
		case directory:
			return true, nil
		case jsonlDirectory:
			return available >= required, diskErr
		default:
			t.Fatalf("checked unexpected path %q", path)
			return false, nil
		}
	}
	if reply, err := server.Health(ctx, &pluginv1.HealthRequest{}); err != nil || !reply.GetHealthy() || !strings.Contains(reply.GetMessage(), "capture paused") {
		t.Fatalf("Health() with low space = (%v, %v)", reply, err)
	}
	available = needed - 1
	if reply, err := server.CaptureRecord(ctx, request); err != nil || reply.GetAccepted() || !strings.Contains(reply.GetMessage(), "insufficient") {
		t.Fatalf("CaptureRecord() with insufficient space = (%v, %v)", reply, err)
	}
	available = needed
	diskErr = os.ErrPermission
	if reply, err := server.CaptureRecord(ctx, request); err != nil || reply.GetAccepted() || !strings.Contains(reply.GetMessage(), "cannot check") {
		t.Fatalf("CaptureRecord() with failed space check = (%v, %v)", reply, err)
	}
	if count, _ := server.store.Usage(); count != 0 {
		t.Fatalf("paused capture wrote %d raw records", count)
	}
	if files, err := os.ReadDir(jsonlDirectory); err != nil || len(files) != 0 {
		t.Fatalf("paused capture left JSONL entries: %v, %v", files, err)
	}
	diskErr = nil
	if reply, err := server.CaptureRecord(ctx, request); err != nil || !reply.GetAccepted() {
		t.Fatalf("CaptureRecord() after recovery = (%v, %v)", reply, err)
	}
	shards, err := server.jsonl.SealBefore(time.Time{})
	if err != nil || len(shards) != 1 {
		t.Fatalf("sealing resumed capture = %v, %v", shards, err)
	}
	data, err := os.ReadFile(shards[0])
	if err != nil || string(data) != string(request.GetRecordJson())+"\n" {
		t.Fatalf("resumed JSONL = %q, %v", data, err)
	}
}

func TestReapplyingConfigDoesNotRemoveInflightArchives(t *testing.T) {
	t.Setenv("SUB2API_PLUGIN_STATE_DIR", t.TempDir())
	server := newCaptureTestServer()
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
			reply, err := server.CaptureRecord(captureContext("test-model"), &pluginv1.CaptureRecordRequest{RecordId: fmt.Sprintf("record-%d", i), InstanceId: "gateway", CompletedAtUnixNano: stamp.UnixNano(), RecordJson: []byte(`{"schema_version":2,"group_id":7,"response":"ok","requested_model":"test-model","model":"test-model"}`)})
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
	if len(uploaded.keys) != 1 || !strings.HasPrefix(uploaded.keys[0], archiveGeneration+"/2026-06-25/Opus5.5/Opus5.5-2026-06-25-") || !strings.HasSuffix(uploaded.keys[0], "-000001.jsonl") {
		t.Fatalf("uploaded keys = %v", uploaded.keys)
	}
}

func TestV2UploadSkipsLegacyShards(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "jsonl", "2026-01-01", "legacy-model")
	if err := os.MkdirAll(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{".open", ".jsonl"} {
		if err := os.WriteFile(filepath.Join(legacy, "old"+ext), []byte("legacy fixture\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s := newCaptureTestServer()
	reply, err := s.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: []byte(fmt.Sprintf(`{"directory":%q,"group_ids":[7],"models":["CIYUAN.FAST"]}`, root))})
	if err != nil || !reply.GetApplied() {
		t.Fatal("archive configuration failed")
	}
	stamp := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	record := []byte(`{"schema_version":2,"group_id":7,"requested_model":"中转","request":"{}","response":"[REDACTED]"}`)
	saved, err := s.CaptureRecord(captureContext("CIYUAN.FAST"), &pluginv1.CaptureRecordRequest{InstanceId: "gateway", RecordId: "sample", CompletedAtUnixNano: stamp.UnixNano(), RecordJson: record})
	if err != nil || !saved.GetAccepted() {
		t.Fatal("matching sanitized model rejected")
	}
	shards, err := s.jsonl.SealBefore(time.Time{})
	if err != nil || len(shards) != 1 {
		t.Fatal("expected one private shard")
	}
	body, err := os.ReadFile(shards[0])
	if err != nil || string(body) != string(record)+"\n" {
		t.Fatal("unexpected shard contents")
	}
	uploader := &uploaderStub{}
	s.s3 = uploader
	s.flushAndUpload()
	s.uploadTimer.Stop()
	if len(uploader.keys) != 1 || !strings.HasPrefix(uploader.keys[0], "rawdata-v2/2026-01-01/%E4%B8%AD%E8%BD%AC/") {
		t.Fatal("unexpected upload destination")
	}
	for _, ext := range []string{".open", ".jsonl"} {
		if _, err := os.Stat(filepath.Join(legacy, "old"+ext)); err != nil {
			t.Fatalf("legacy shard was touched: %v", err)
		}
	}
}
