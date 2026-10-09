// Command terminal-service executes governed SSH on behalf of the
// skquad tool-gateway (TG-10 Terminal-as-a-Service). It holds ALL SSH
// credential material inside the quarantine namespace; agents hold
// none. See docs/tool-gateway.md §6.6.
package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/rossbrigoli/skquad/terminal-service/internal/apply"
	"github.com/rossbrigoli/skquad/terminal-service/internal/caclient"
	"github.com/rossbrigoli/skquad/terminal-service/internal/httpapi"
	"github.com/rossbrigoli/skquad/terminal-service/internal/recorder"
)

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	token := os.Getenv("SKQUAD_TERMINAL_INTERNAL_TOKEN")
	if token == "" {
		logger.Error("SKQUAD_TERMINAL_INTERNAL_TOKEN unset; refusing to start")
		os.Exit(1)
	}

	cfg := httpapi.Config{
		InternalToken:      token,
		MaxSessions:        envInt("SKQUAD_TERMINAL_MAX_SESSIONS", 8),
		SessionIdleTimeout: time.Duration(envInt("SKQUAD_TERMINAL_SESSION_IDLE_TIMEOUT_SECONDS", 1800)) * time.Second,
		CAMint:             &caminter{},
	}

	// Recording sink: S3 if configured, else local dir, else disabled.
	sinkFactory, sinkErr := buildSinkFactory(logger)
	if sinkErr != nil {
		logger.Error("recording sink unavailable", "err", sinkErr)
		os.Exit(1)
	}
	cfg.RecorderSinkFactory = sinkFactory

	// Artifact apply engine (TG-11 §6.7). Recording is best-effort:
	// a sink failure downgrades auditability, never the apply itself.
	cfg.ApplyEngine = &apply.Engine{NewRecorder: applyRecorderFactory(sinkFactory)}
	cfg.MaxApplies = envInt("SKQUAD_TERMINAL_MAX_APPLIES", 16)

	handler, err := httpapi.NewServer(cfg, logger)
	if err != nil {
		logger.Error("server config invalid", "err", err)
		os.Exit(1)
	}

	port := envInt("SKQUAD_TERMINAL_PORT", 8091)
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("terminal-service listening", "port", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("listen failed", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop
	logger.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// caminter mints CA certs via the step-ca HTTP contract.
// DECISION: endpoint/OTT wiring finalized at deploy integration (see
// caclient package docs). Unset endpoint → ca_unavailable at call time
// (fail closed), service still boots for static_key deployments.
type caminter struct{}

func (c *caminter) Mint(ctx context.Context, user, host string, ttl time.Duration) (*caclient.Cert, error) {
	endpoint := strings.TrimSpace(os.Getenv("SKQUAD_STEP_CA_URL"))
	token := strings.TrimSpace(os.Getenv("SKQUAD_STEP_CA_OTT"))
	if endpoint == "" {
		return nil, fmt.Errorf("ca_unavailable")
	}
	return caclient.MintCertWithRequest(ctx, endpoint, token, nil, user, host, ttl)
}

func applyRecorderFactory(sinkFactory func(context.Context, string) (recorder.Sink, error)) func(applyID string, meta map[string]any) (apply.Recorder, error) {
	if sinkFactory == nil {
		return nil
	}
	return func(applyID string, meta map[string]any) (apply.Recorder, error) {
		sink, err := sinkFactory(context.Background(), applyID)
		if err != nil {
			return nil, err
		}
		m := recorder.Meta{}
		if v, ok := meta["resource_id"].(string); ok {
			m.ResourceID = v
		}
		if v, ok := meta["agent_id"].(string); ok {
			m.AgentID = v
		}
		if v, ok := meta["host_group"].(string); ok {
			m.Host = v
		}
		if v, ok := meta["playbook"].(string); ok {
			m.Command = v
		}
		return recorder.New(sink, applyID, m)
	}
}

func buildSinkFactory(logger *slog.Logger) (func(context.Context, string) (recorder.Sink, error), error) {
	endpoint := env("SKQUAD_RECORDING_S3_ENDPOINT", "")
	if endpoint == "" {
		if dir := env("SKQUAD_RECORDING_LOCAL_DIR", ""); dir != "" {
			logger.Info("recording sink: local dir", "dir", dir)
			return func(_ context.Context, _ string) (recorder.Sink, error) {
				return recorder.NewLocalDirSink(dir)
			}, nil
		}
		logger.Warn("recording sink disabled (no S3 or local dir configured)")
		return nil, nil
	}
	bucket := env("SKQUAD_RECORDING_S3_BUCKET", "skquad-terminal-recordings")
	ak := env("SKQUAD_RECORDING_S3_ACCESS_KEY", "")
	sk := env("SKQUAD_RECORDING_S3_SECRET_KEY", "")
	if ak == "" || sk == "" {
		return nil, fmt.Errorf("S3 access/secret key required when endpoint set")
	}
	client, err := minio.New(strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://"), &minio.Options{
		Creds:  credentials.NewStaticV4(ak, sk, ""),
		Secure: strings.HasPrefix(endpoint, "https://"),
		Region: env("SKQUAD_RECORDING_S3_REGION", "us-east-1"),
	})
	if err != nil {
		return nil, err
	}
	// Ensure bucket exists once at boot (best-effort; recording failures
	// never kill sessions).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		exists, existErr := client.BucketExists(ctx, bucket)
		if !exists || existErr != nil {
			logger.Warn("recording bucket not ensured", "bucket", bucket, "err", err)
		}
	}
	logger.Info("recording sink: S3", "endpoint", endpoint, "bucket", bucket)
	return func(_ context.Context, _ string) (recorder.Sink, error) {
		return &recorder.S3Sink{
			Bucket: bucket,
			Put: func(ctx context.Context, bucket, key string, data []byte) error {
				_, err := client.PutObject(ctx, bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
					ContentType: "application/x-ndjson",
				})
				return err
			},
		}, nil
	}, nil
}
