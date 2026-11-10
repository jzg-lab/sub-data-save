package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jzg-lab/sub-data-save/internal/archive"
	pluginv1 "github.com/jzg-lab/sub-data-save/pkg/pluginapi/v1"
)

const (
	ID      = "jzg-lab.sub-data-save"
	Version = "0.1.1"
)

type config struct {
	Directory string `json:"directory"`
}

type Server struct {
	pluginv1.UnimplementedTransportPluginServer
	mu    sync.RWMutex
	store *archive.Store
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
		return &pluginv1.ValidateConfigResponse{Valid: false, Message: "directory must be an absolute path"}, nil
	}
	raw, _ := json.Marshal(cfg)
	return &pluginv1.ValidateConfigResponse{Valid: true, NormalizedConfigJson: raw}, nil
}

func (s *Server) ApplyConfig(_ context.Context, req *pluginv1.ApplyConfigRequest) (*pluginv1.ApplyConfigResponse, error) {
	cfg, err := parseConfig(req.GetConfigJson())
	if err != nil {
		return &pluginv1.ApplyConfigResponse{Applied: false, Message: "directory must be an absolute path"}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	store, err := archive.Open(cfg.Directory, archive.Limits{MaxRecords: 100_000, MaxBytes: 10 << 30, MaxRecordBytes: 60 << 20})
	if err != nil {
		return &pluginv1.ApplyConfigResponse{Applied: false, Message: "cannot open archive directory"}, nil
	}
	s.store = store
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
	return cfg, nil
}

func (s *Server) TestConfig(ctx context.Context, req *pluginv1.TestConfigRequest) (*pluginv1.TestConfigResponse, error) {
	result, err := s.ValidateConfig(ctx, &pluginv1.ValidateConfigRequest{ConfigJson: req.GetConfigJson()})
	return &pluginv1.TestConfigResponse{Success: result.GetValid(), Message: result.GetMessage()}, err
}

func (s *Server) CaptureRecord(ctx context.Context, req *pluginv1.CaptureRecordRequest) (*pluginv1.CaptureRecordResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	store := s.store
	if store == nil {
		return &pluginv1.CaptureRecordResponse{Accepted: false, Message: "archive is not configured"}, nil
	}
	if req.GetCompletedAtUnixNano() <= 0 {
		return &pluginv1.CaptureRecordResponse{Accepted: false, Message: "invalid completion time"}, nil
	}
	if _, err := store.Save(ctx, req.GetInstanceId(), req.GetRecordId(), time.Unix(0, req.GetCompletedAtUnixNano()), req.GetRecordJson()); err != nil {
		if errors.Is(err, archive.ErrLimitReached) {
			return &pluginv1.CaptureRecordResponse{Accepted: false, Message: "archive limit reached"}, nil
		}
		return &pluginv1.CaptureRecordResponse{Accepted: false, Message: "archive write failed"}, nil
	}
	return &pluginv1.CaptureRecordResponse{Accepted: true}, nil
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
