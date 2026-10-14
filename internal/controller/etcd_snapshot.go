package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
	"github.com/openresearch/talos-lifecycle/internal/talosutil"
)

const snapshotMaxAge = 30 * time.Minute

const snapshotTimeout = 10 * time.Minute

type s3Config struct {
	endpoint string
	bucket   string
	access   string
	secret   string
	region   string
	prefix   string
	secure   bool
}

func loadS3Config() (*s3Config, error) {
	cfg := &s3Config{
		endpoint: os.Getenv("S3_ENDPOINT"),
		bucket:   os.Getenv("S3_BUCKET"),
		access:   os.Getenv("S3_ACCESS_KEY"),
		secret:   os.Getenv("S3_SECRET_KEY"),
		region:   os.Getenv("S3_REGION"),
		prefix:   os.Getenv("S3_PREFIX"),
		secure:   true,
	}

	switch {
	case strings.HasPrefix(cfg.endpoint, "https://"):
		cfg.endpoint = strings.TrimPrefix(cfg.endpoint, "https://")
	case strings.HasPrefix(cfg.endpoint, "http://"):
		cfg.endpoint = strings.TrimPrefix(cfg.endpoint, "http://")
		cfg.secure = false
	}

	if cfg.endpoint == "" || cfg.bucket == "" || cfg.access == "" || cfg.secret == "" {
		return nil, fmt.Errorf(
			"etcd snapshots are required but S3 is not configured " +
				"(set S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY, S3_SECRET_KEY " +
				"or set spec.etcdSnapshotPolicy: disabled)")
	}

	return cfg, nil
}

func (r *UpgradePlanReconciler) ensureEtcdSnapshot(
	ctx context.Context,
	plan *upgradev1alpha1.UpgradePlan,
	trigger string,
) (blockReason string) {
	log := log.FromContext(ctx)

	if plan.Spec.EtcdSnapshotPolicy == upgradev1alpha1.EtcdSnapshotDisabled {
		return ""
	}

	if last := plan.Status.LastEtcdSnapshot; last != nil &&
		time.Since(last.Time.Time) < snapshotMaxAge {
		return ""
	}

	snapshot, err := r.takeEtcdSnapshot(ctx, plan, trigger)
	if err != nil {
		log.Error(err, "etcd snapshot failed, blocking upgrade")
		metricSnapshotFailures.Inc()
		r.event(plan, corev1.EventTypeWarning, "EtcdSnapshotFailed", err.Error())

		return fmt.Sprintf("etcd snapshot failed: %v", err)
	}

	plan.Status.LastEtcdSnapshot = snapshot
	metricSnapshotLastSuccess.Set(float64(snapshot.Time.Unix()))
	r.event(plan, corev1.EventTypeNormal, "EtcdSnapshotStored",
		fmt.Sprintf("stored %s (%d bytes)", snapshot.Name, snapshot.SizeBytes))
	log.Info("etcd snapshot stored",
		"object", snapshot.Name, "bytes", snapshot.SizeBytes)

	return ""
}

func (r *UpgradePlanReconciler) takeEtcdSnapshot(
	ctx context.Context,
	plan *upgradev1alpha1.UpgradePlan,
	trigger string,
) (*upgradev1alpha1.EtcdSnapshotStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()

	cfg, err := loadS3Config()
	if err != nil {
		return nil, err
	}

	endpoint, err := controlPlaneEndpoint(plan)
	if err != nil {
		return nil, err
	}

	c, talosCtx, err := talosutil.Client(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	defer c.Close() //nolint:errcheck

	reader, err := c.EtcdSnapshot(talosCtx, &machineapi.EtcdSnapshotRequest{})
	if err != nil {
		return nil, fmt.Errorf("stream etcd snapshot from %s: %w", endpoint, err)
	}
	defer reader.Close() //nolint:errcheck

	s3, err := minio.New(cfg.endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.access, cfg.secret, ""),
		Secure: cfg.secure,
		Region: cfg.region,
	})
	if err != nil {
		return nil, fmt.Errorf("create s3 client: %w", err)
	}

	name := fmt.Sprintf("etcd-snapshot-%s-%s.db",
		time.Now().UTC().Format("2006-01-02T15-04-05Z"), trigger)
	if cfg.prefix != "" {
		name = strings.TrimSuffix(cfg.prefix, "/") + "/" + name
	}

	// the size is unknown; without PartSize minio-go buffers about 576 MiB
	uploaded, err := s3.PutObject(ctx, cfg.bucket, name, reader, -1, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
		PartSize:    16 * 1024 * 1024,
	})
	if err != nil {
		return nil, fmt.Errorf("upload snapshot to s3://%s/%s: %w", cfg.bucket, name, err)
	}

	if uploaded.Size == 0 {
		return nil, fmt.Errorf("uploaded snapshot s3://%s/%s is empty", cfg.bucket, name)
	}

	return &upgradev1alpha1.EtcdSnapshotStatus{
		Name:      name,
		Time:      metav1.Now(),
		SizeBytes: uploaded.Size,
	}, nil
}
