package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
)

func TestLoadS3Config(t *testing.T) {
	for _, key := range []string{"S3_ENDPOINT", "S3_BUCKET", "S3_ACCESS_KEY", "S3_SECRET_KEY"} {
		t.Setenv(key, "")
	}

	if _, err := loadS3Config(); err == nil {
		t.Fatal("loadS3Config with empty env: want error, got nil")
	}

	t.Setenv("S3_ENDPOINT", "https://s3.example.com")
	t.Setenv("S3_BUCKET", "backups")
	t.Setenv("S3_ACCESS_KEY", "ak")
	t.Setenv("S3_SECRET_KEY", "sk")

	cfg, err := loadS3Config()
	if err != nil {
		t.Fatalf("loadS3Config: %v", err)
	}

	if cfg.endpoint != "s3.example.com" || !cfg.secure {
		t.Errorf("https endpoint parsed as %q secure=%v, want s3.example.com secure=true", cfg.endpoint, cfg.secure)
	}

	t.Setenv("S3_ENDPOINT", "http://minio.minio.svc:9000")

	cfg, err = loadS3Config()
	if err != nil {
		t.Fatalf("loadS3Config: %v", err)
	}

	if cfg.endpoint != "minio.minio.svc:9000" || cfg.secure {
		t.Errorf("http endpoint parsed as %q secure=%v, want minio.minio.svc:9000 secure=false", cfg.endpoint, cfg.secure)
	}
}

func TestEnsureEtcdSnapshotPolicy(t *testing.T) {
	r := &UpgradePlanReconciler{}
	ctx := context.Background()

	plan := &upgradev1alpha1.UpgradePlan{
		Spec: upgradev1alpha1.UpgradePlanSpec{
			EtcdSnapshotPolicy: upgradev1alpha1.EtcdSnapshotDisabled,
		},
	}
	if reason := r.ensureEtcdSnapshot(ctx, plan, "test"); reason != "" {
		t.Errorf("disabled policy blocked with %q, want no block", reason)
	}

	plan = &upgradev1alpha1.UpgradePlan{
		Status: upgradev1alpha1.UpgradePlanStatus{
			LastEtcdSnapshot: &upgradev1alpha1.EtcdSnapshotStatus{
				Name: "etcd-snapshot-test.db",
				Time: metav1.NewTime(time.Now().Add(-5 * time.Minute)),
			},
		},
	}
	if reason := r.ensureEtcdSnapshot(ctx, plan, "test"); reason != "" {
		t.Errorf("fresh snapshot blocked with %q, want no block", reason)
	}

	for _, key := range []string{"S3_ENDPOINT", "S3_BUCKET", "S3_ACCESS_KEY", "S3_SECRET_KEY"} {
		t.Setenv(key, "")
	}

	plan.Status.LastEtcdSnapshot.Time = metav1.NewTime(time.Now().Add(-2 * time.Hour))
	if reason := r.ensureEtcdSnapshot(ctx, plan, "test"); reason == "" {
		t.Error("stale snapshot without S3 config: want blocking reason, got none")
	}
}
