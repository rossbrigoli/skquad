// Command skquad-build-service is the governed agent build path
// (MIG-8 / S-314): agents submit build requests through the
// tool-gateway `rest` driver; this service enforces repo allowlists,
// live git-grant scope, destination namespaces and quotas, pins the
// revision to a full SHA, and schedules a kaniko Job in a dedicated
// namespace. Agents never hold registry credentials and never get a
// docker daemon.
//
// Governance summary (see README.md for the full contract):
//
//	deny-by-default at every gate; fail-closed on policy fetch errors;
//	audit event for every submit/deny/failure; digest returned from
//	the kaniko termination message.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/rossbrigoli/skquad/build-service/internal/config"
	"github.com/rossbrigoli/skquad/build-service/internal/httpapi"
	"github.com/rossbrigoli/skquad/build-service/internal/policyclient"
	"github.com/rossbrigoli/skquad/build-service/internal/principal"
	"github.com/rossbrigoli/skquad/build-service/internal/revision"
	"github.com/rossbrigoli/skquad/build-service/internal/service"
)

func main() {
	configPath := flag.String("config", "/etc/skquad-build/config.yaml", "config file path")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("config load failed; refusing to start", "err", err)
		os.Exit(1)
	}
	pstore, err := principal.LoadFile(cfg.PrincipalsFile)
	if err != nil {
		logger.Error("principals load failed; refusing to start", "err", err)
		os.Exit(1)
	}

	kstore, err := newKubeStore()
	if err != nil {
		logger.Error("kubernetes client unavailable; refusing to start", "err", err)
		os.Exit(1)
	}

	var scope service.ScopeChecker
	if cfg.CPPolicyURL != "" {
		scope = policyclient.New(
			cfg.CPPolicyURL,
			time.Duration(cfg.CPPolicyTTLSeconds)*time.Second,
			time.Duration(cfg.CPPolicyTimeoutSeconds)*time.Second,
		)
	}

	svc := service.New(cfg, kstore, scope,
		&revision.Resolver{Runner: revision.ExecRunner{}}, logger)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           httpapi.NewServer(pstore, svc, logger).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		logger.Info("skquad-build-service listening",
			"addr", cfg.Listen, "namespace", cfg.Namespace, "principals", pstore.Count())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "err", err)
	}
	logger.Info("skquad-build-service stopped")
}

// batchJob aliases the batch type used across the service boundary.
type batchJob = batchv1.Job

// batchPod aliases the core pod type for the JobStore surface.
type batchPod = corev1.Pod

// kubeStore adapts client-go to service.JobStore.
type kubeStore struct{ cs kubernetes.Interface }

func newKubeStore() (*kubeStore, error) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	return &kubeStore{cs: cs}, nil
}

func (k *kubeStore) CreateJob(ctx context.Context, job *batchJob) error {
	_, err := k.cs.BatchV1().Jobs(job.Namespace).Create(ctx, job, metav1.CreateOptions{})
	return err
}

func (k *kubeStore) GetJob(ctx context.Context, namespace, name string) (*batchJob, error) {
	return k.cs.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (k *kubeStore) ListJobs(ctx context.Context, namespace, labelSelector string) ([]batchJob, error) {
	list, err := k.cs.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}
	out := make([]batchJob, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, list.Items[i])
	}
	return out, nil
}

func (k *kubeStore) ListPods(ctx context.Context, namespace, labelSelector string) ([]batchPod, error) {
	list, err := k.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}
