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
	"github.com/rossbrigoli/skquad/terminal-service/internal/drift"
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

	// TG-11 slice D: drift-check mode — the CronJob entrypoint in the
	// SAME image (no new container image). Runs one read-only sweep and
	// exits; the long-running server path below is untouched.
	if len(os.Args) > 1 && os.Args[1] == "drift-check" {
		if err := runDriftCheck(context.Background(), logger); err != nil {
			logger.Error("drift-check failed", "err", err)
			os.Exit(1)
		}
		return
	}

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

// runDriftCheck executes one drift sweep (TG-11 slice D). It reuses
// the apply engine (CheckOnly via the runner), the recording sink and
// the CA minter from this image, and talks to the CP drift surface
// with the direction-scoped SKQUAD_DRIFT_INGEST_TOKEN.
//
// Exit policy: the sweep only fails when the resource listing fails or
// the context dies — per-resource/per-group problems are logged and
// skipped by the runner, and DRIFT ITSELF is data, not a failure.
func runDriftCheck(ctx context.Context, logger *slog.Logger) error {
	cpURL := env("SKQUAD_CP_URL", "")
	ingestToken := env("SKQUAD_DRIFT_INGEST_TOKEN", "")
	if cpURL == "" || ingestToken == "" {
		return fmt.Errorf("SKQUAD_CP_URL and SKQUAD_DRIFT_INGEST_TOKEN are required for drift-check")
	}
	// Recording is best-effort here too: a check run should be auditable
	// when a sink exists, but a missing sink never blocks drift checks.
	sinkFactory, _ := buildSinkFactory(logger)
	engine := &apply.Engine{NewRecorder: applyRecorderFactory(sinkFactory)}
	client := &drift.CPClient{BaseURL: cpURL, Token: ingestToken}
	runner := &drift.Runner{
		Resources:       client,
		Poster:          client,
		Engine:          engine,
		Tips:            &drift.GitTipResolver{},
		CAMint:          applyCAMinter{inner: &caminter{}},
		DefaultPlaybook: env("SKQUAD_DRIFT_PLAYBOOK", "site.yml"),
		CheckTimeout:    time.Duration(envInt("SKQUAD_DRIFT_CHECK_TIMEOUT_SECONDS", 600)) * time.Second,
		CertTTL:         time.Duration(envInt("SKQUAD_DRIFT_CERT_TTL_SECONDS", 900)) * time.Second,
		Logger:          logger,
	}
	// Static-key deployments: the drift identity key is read from a
	// mounted file (SealedSecret), never from values or env literals.
	if kf := env("SKQUAD_DRIFT_SSH_KEY_FILE", ""); kf != "" {
		keyPEM, err := os.ReadFile(kf)
		if err != nil {
			return fmt.Errorf("drift: read SSH key file: %w", err)
		}
		runner.SSHKeyPEM = string(keyPEM)
	}
	sum, err := runner.Run(ctx)
	logger.Info("drift-check complete",
		"resources", sum.Resources,
		"checks_posted", sum.Checks,
		"drifted", sum.Drifted,
		"skipped", sum.Skipped)
	return err
}

// applyCAMinter adapts the session-side caminter (*caclient.Cert) to
// the apply engine's CAMinter shape (raw cert PEM bytes).
type applyCAMinter struct{ inner *caminter }

func (a applyCAMinter) Mint(ctx context.Context, user, host string, ttl time.Duration) ([]byte, error) {
	cert, err := a.inner.Mint(ctx, user, host, ttl)
	if err != nil {
		return nil, err
	}
	return cert.CertPEM, nil
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
