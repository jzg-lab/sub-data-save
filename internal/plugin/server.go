package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jzg-lab/sub-data-save/internal/archive"
	pluginv1 "github.com/jzg-lab/sub-data-save/pkg/pluginapi/v1"
)

const (
	ID      = "jzg-lab.sub-data-save"
	Version = "0.2.0"
)

type config struct {
	Directory string   `json:"directory"`
	Bucket    string   `json:"bucket"`
	Region    string   `json:"region"`
	GroupIDs  []int64  `json:"group_ids"`
	Models    []string `json:"models"`
}

type objectUploader interface {
	Upload(context.Context, *s3.PutObjectInput) error
}

type s3ObjectUploader struct{ uploader *manager.Uploader }

func (u s3ObjectUploader) Upload(ctx context.Context, input *s3.PutObjectInput) error {
	_, err := u.uploader.Upload(ctx, input)
	return err
}

type Server struct {
	pluginv1.UnimplementedTransportPluginServer
	mu          sync.RWMutex
	store       *archive.Store
	jsonl       *archive.JSONLWriter
	cfg         config
	s3          objectUploader
	uploadMu    sync.Mutex
	uploadTimer *time.Timer
}

func New() *Server { return &Server{} }

func (s *Server) GetInfo(context.Context, *pluginv1.GetInfoRequest) (*pluginv1.GetInfoResponse, error) {
	return &pluginv1.GetInfoResponse{
		PluginId: ID, PluginVersion: Version,
		ProtocolVersion: pluginv1.ProtocolVersion, TransportApiVersion: pluginv1.TransportAPIVersion,
		Capabilities: []string{"data.capture.v1"},
	}, nil
}

func (s *Server) Health(ctx context.Context, _ *pluginv1.HealthRequest) (*pluginv1.HealthResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	ready := s.store != nil
	s.mu.RUnlock()
	message := "awaiting configuration"
	if ready {
		message = "archive ready"
	}
	return &pluginv1.HealthResponse{Healthy: true, Message: message}, nil
}

func (s *Server) ValidateConfig(_ context.Context, req *pluginv1.ValidateConfigRequest) (*pluginv1.ValidateConfigResponse, error) {
	cfg, err := parseConfig(req.GetConfigJson())
	if err != nil {
		return &pluginv1.ValidateConfigResponse{Valid: false, Message: err.Error()}, nil
	}
	raw, _ := json.Marshal(cfg)
	return &pluginv1.ValidateConfigResponse{Valid: true, NormalizedConfigJson: raw}, nil
}

func (s *Server) ApplyConfig(ctx context.Context, req *pluginv1.ApplyConfigRequest) (*pluginv1.ApplyConfigResponse, error) {
	cfg, err := parseConfig(req.GetConfigJson())
	if err != nil {
		return &pluginv1.ApplyConfigResponse{Applied: false, Message: "invalid archive or S3 configuration"}, nil
	}
	var uploader objectUploader
	if cfg.Bucket != "" {
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
		if err != nil {
			return &pluginv1.ApplyConfigResponse{Applied: false, Message: "cannot load AWS credentials or region"}, nil
		}
		uploader = s3ObjectUploader{manager.NewUploader(s3.NewFromConfig(awsCfg))}
	}
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store == nil || s.cfg.Directory != cfg.Directory {
		store, err := archive.Open(cfg.Directory, archive.Limits{MaxRecords: 100_000, MaxBytes: 10 << 30, MaxRecordBytes: 60 << 20})
		if err != nil {
			return &pluginv1.ApplyConfigResponse{Applied: false, Message: "cannot open archive directory"}, nil
		}
		jsonl, err := archive.OpenJSONL(filepath.Join(cfg.Directory, "jsonl"), archive.DefaultShardLimit)
		if err != nil {
			return &pluginv1.ApplyConfigResponse{Applied: false, Message: "cannot open JSONL directory"}, nil
		}
		if s.jsonl != nil {
			if _, err := s.jsonl.SealBefore(time.Time{}); err != nil {
				return &pluginv1.ApplyConfigResponse{Applied: false, Message: "cannot finalize previous archive"}, nil
			}
		}
		s.store, s.jsonl = store, jsonl
	}
	s.cfg, s.s3 = cfg, uploader
	s.scheduleUploadLocked(time.Second)
	return &pluginv1.ApplyConfigResponse{Applied: true}, nil
}

func parseConfig(raw []byte) (config, error) {
	if !json.Valid(raw) {
		return config{}, errors.New("invalid plugin configuration JSON")
	}
	var cfg config
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return config{}, errors.New("invalid archive configuration")
	}
	if strings.TrimSpace(cfg.Directory) == "" {
		state := os.Getenv("SUB2API_PLUGIN_STATE_DIR")
		if !filepath.IsAbs(state) {
			return config{}, errors.New("host did not provide a persistent state directory")
		}
		cfg.Directory = filepath.Join(state, "archive")
	}
	if !filepath.IsAbs(cfg.Directory) {
		return config{}, errors.New("invalid archive directory")
	}
	cfg.Directory = filepath.Clean(cfg.Directory)
	if (cfg.Bucket == "") != (cfg.Region == "") || strings.ContainsAny(cfg.Bucket, "/\\") {
		return config{}, errors.New("bucket and region must be configured together")
	}
	seenGroups := make(map[int64]struct{}, len(cfg.GroupIDs))
	for _, id := range cfg.GroupIDs {
		if id <= 0 {
			return config{}, errors.New("group IDs must be positive")
		}
		seenGroups[id] = struct{}{}
	}
	cfg.GroupIDs = cfg.GroupIDs[:0]
	for id := range seenGroups {
		cfg.GroupIDs = append(cfg.GroupIDs, id)
	}
	seenModels := make(map[string]struct{}, len(cfg.Models))
	for _, model := range cfg.Models {
		model = strings.TrimSpace(model)
		if model != "" {
			seenModels[model] = struct{}{}
		}
	}
	cfg.Models = cfg.Models[:0]
	for model := range seenModels {
		cfg.Models = append(cfg.Models, model)
	}
	sort.Slice(cfg.GroupIDs, func(i, j int) bool { return cfg.GroupIDs[i] < cfg.GroupIDs[j] })
	sort.Strings(cfg.Models)
	if cfg.GroupIDs == nil {
		cfg.GroupIDs = []int64{}
	}
	if cfg.Models == nil {
		cfg.Models = []string{}
	}
	return cfg, nil
}

func (s *Server) TestConfig(ctx context.Context, req *pluginv1.TestConfigRequest) (*pluginv1.TestConfigResponse, error) {
	result, err := s.ValidateConfig(ctx, &pluginv1.ValidateConfigRequest{ConfigJson: req.GetConfigJson()})
	return &pluginv1.TestConfigResponse{Success: result.GetValid(), Message: result.GetMessage()}, err
}

func (s *Server) CaptureRecord(ctx context.Context, req *pluginv1.CaptureRecordRequest) (*pluginv1.CaptureRecordResponse, error) {
	s.mu.RLock()
	store, jsonl := s.store, s.jsonl
	cfg := s.cfg
	if store == nil {
		s.mu.RUnlock()
		return &pluginv1.CaptureRecordResponse{Accepted: false, Message: "archive is not configured"}, nil
	}
	if !matchesFilters(cfg, req.GetRecordJson()) {
		s.mu.RUnlock()
		return &pluginv1.CaptureRecordResponse{Accepted: false, Message: "record is outside configured filters"}, nil
	}
	if req.GetCompletedAtUnixNano() <= 0 {
		s.mu.RUnlock()
		return &pluginv1.CaptureRecordResponse{Accepted: false, Message: "invalid completion time"}, nil
	}
	completedAt := time.Unix(0, req.GetCompletedAtUnixNano())
	if _, err := store.Save(ctx, req.GetInstanceId(), req.GetRecordId(), completedAt, req.GetRecordJson()); err != nil && err != archive.ErrLimitReached {
		s.mu.RUnlock()
		if errors.Is(err, archive.ErrDuplicateID) {
			return &pluginv1.CaptureRecordResponse{Accepted: false, Message: "archive record already exists"}, nil
		}
		return &pluginv1.CaptureRecordResponse{Accepted: false, Message: "archive write failed"}, nil
	}
	_, err := jsonl.Append(req.GetRecordJson(), completedAt)
	if err != nil {
		s.mu.RUnlock()
		return &pluginv1.CaptureRecordResponse{Accepted: false, Message: "JSONL write failed"}, nil
	}
	s.mu.RUnlock()
	return &pluginv1.CaptureRecordResponse{Accepted: true}, nil
}

func matchesFilters(cfg config, raw []byte) bool {
	if len(cfg.GroupIDs) == 0 || len(cfg.Models) == 0 {
		return false
	}
	var record struct {
		GroupID        int64  `json:"group_id"`
		RequestedModel string `json:"requested_model"`
	}
	if json.Unmarshal(raw, &record) != nil || record.GroupID <= 0 || strings.TrimSpace(record.RequestedModel) == "" {
		return false
	}
	groupMatched, modelMatched := false, false
	for _, id := range cfg.GroupIDs {
		groupMatched = groupMatched || id == record.GroupID
	}
	for _, model := range cfg.Models {
		modelMatched = modelMatched || model == record.RequestedModel
	}
	return groupMatched && modelMatched
}

func (s *Server) scheduleUploadLocked(delay time.Duration) {
	if s.s3 != nil && s.uploadTimer == nil {
		s.uploadTimer = time.AfterFunc(delay, s.flushAndUpload)
	}
}

func (s *Server) flushAndUpload() {
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.uploadTimer = nil
		s.scheduleUploadLocked(time.Minute)
	}()
	s.mu.RLock()
	jsonl, uploader, cfg := s.jsonl, s.s3, s.cfg
	if uploader == nil || jsonl == nil {
		s.mu.RUnlock()
		return
	}
	_, _ = jsonl.SealBefore(time.Now())
	s.mu.RUnlock()
	paths, err := jsonl.Pending()
	if err == nil {
		for _, path := range paths {
			key, keyErr := archive.S3ObjectKey(filepath.Join(cfg.Directory, "jsonl"), path)
			if keyErr != nil {
				continue
			}
			file, openErr := os.Open(path)
			if openErr != nil {
				continue
			}
			uploadErr := uploader.Upload(context.Background(), &s3.PutObjectInput{Bucket: aws.String(cfg.Bucket), Key: aws.String(key), Body: file})
			closeErr := file.Close()
			if uploadErr == nil && closeErr == nil {
				_ = archive.MarkUploaded(path)
			}
		}
	}
}

func (s *Server) ListCaptureRecords(ctx context.Context, req *pluginv1.ListCaptureRecordsRequest) (*pluginv1.ListCaptureRecordsResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	store := s.store
	if store == nil {
		return nil, errors.New("archive is not configured")
	}
	records, err := store.List(req.GetDate())
	if err != nil {
		return nil, err
	}
	response := &pluginv1.ListCaptureRecordsResponse{Records: make([]*pluginv1.CaptureRecordInfo, 0, len(records))}
	for _, record := range records {
		response.Records = append(response.Records, &pluginv1.CaptureRecordInfo{
			Date: record.Date, InstanceId: record.InstanceID, RecordId: record.RecordID, Size: record.Size,
		})
	}
	return response, nil
}

func (s *Server) DownloadCaptureRecord(ctx context.Context, req *pluginv1.DownloadCaptureRecordRequest) (*pluginv1.DownloadCaptureRecordResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	store := s.store
	if store == nil {
		return nil, errors.New("archive is not configured")
	}
	data, err := store.Read(req.GetDate(), req.GetInstanceId(), req.GetRecordId())
	if err != nil {
		return nil, err
	}
	return &pluginv1.DownloadCaptureRecordResponse{RecordJson: data}, nil
}
