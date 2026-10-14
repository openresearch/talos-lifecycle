package configdiff

import (
	"strings"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
)

func baseConfig(t *testing.T, machineToken, clusterSecret, hostname string) config.Provider {
	t.Helper()

	raw := `version: v1alpha1
machine:
  type: worker
  token: ` + machineToken + `
  ca:
    crt: ""
    key: ""
  install:
    disk: /dev/sda
  network:
    hostname: ` + hostname + `
cluster:
  id: dGVzdA==
  secret: ` + clusterSecret + `
  controlPlane:
    endpoint: https://10.0.0.1:6443
  token: aaaaaa.bbbbbbbbbbbbbbbb
  ca:
    crt: ""
    key: ""
`

	cfg, err := configloader.NewFromBytes([]byte(raw), configloader.WithNoValidation())
	if err != nil {
		t.Fatalf("load fixture config: %v", err)
	}

	return cfg
}

const (
	tokenA = "aaaaaa.bbbbbbbbbbbbbbbb"
	tokenB = "cccccc.dddddddddddddddd"
	secret = "c2VjcmV0LXZhbHVlLWE="
	other  = "c2VjcmV0LXZhbHVlLWI="
)

func TestDiffIdenticalConfigsIsEmpty(t *testing.T) {
	a := baseConfig(t, tokenA, secret, "worker-0")
	b := baseConfig(t, tokenA, secret, "worker-0")

	report, err := Diff(a, b)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	if !empty(report) {
		t.Errorf("expected an empty report, got:\n%s", report)
	}
}

func TestDiffShowsStructuralChange(t *testing.T) {
	a := baseConfig(t, tokenA, secret, "worker-0")
	b := baseConfig(t, tokenA, secret, "worker-RENAMED")

	report, err := Diff(a, b)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	if report.Structural == "" {
		t.Fatal("expected a structural diff for a changed hostname, got none")
	}

	if !strings.Contains(report.Structural, "worker-RENAMED") {
		t.Errorf("structural diff does not mention the new hostname:\n%s", report.Structural)
	}

	if len(report.SecretChanges) != 0 {
		t.Errorf("expected no secret changes, got %v", report.SecretChanges)
	}
}

func TestDiffDetectsSecretRotationThatRedactionHides(t *testing.T) {
	a := baseConfig(t, tokenA, secret, "worker-0")
	b := baseConfig(t, tokenB, secret, "worker-0")

	report, err := Diff(a, b)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	if strings.Contains(report.Structural, tokenA) || strings.Contains(report.Structural, tokenB) {
		t.Fatalf("SECURITY: a raw token leaked into the structural diff:\n%s", report.Structural)
	}

	if len(report.SecretChanges) == 0 {
		t.Fatal("secret rotation went undetected; this is the whole point of the package")
	}

	var found bool

	for _, change := range report.SecretChanges {
		if strings.HasPrefix(change.Field, "token") {
			found = true

			if change.OldHash == change.NewHash {
				t.Errorf("token hashes are equal for different values: %+v", change)
			}

			if change.OldHash == "" || change.NewHash == "" {
				t.Errorf("expected both hashes to be present, got %+v", change)
			}
		}
	}

	if !found {
		t.Errorf("no change reported for the machine token, got %v", report.SecretChanges)
	}
}

func TestReportNeverLeaksSecretValues(t *testing.T) {
	a := baseConfig(t, tokenA, secret, "worker-0")
	b := baseConfig(t, tokenB, other, "worker-1")

	report, err := Diff(a, b)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	rendered := report.String()

	for _, leak := range []string{tokenA, tokenB, secret, other} {
		if strings.Contains(rendered, leak) {
			t.Errorf("SECURITY: rendered report contains the raw value %q:\n%s", leak, rendered)
		}
	}
}

func TestSecretChangeString(t *testing.T) {
	tests := []struct {
		name   string
		change SecretChange
		want   string
	}{
		{
			name:   "rotated",
			change: SecretChange{Field: "token", OldHash: "sha256:aaa", NewHash: "sha256:bbb"},
			want:   "token: sha256:aaa -> sha256:bbb",
		},
		{
			name:   "added",
			change: SecretChange{Field: "token", NewHash: "sha256:bbb"},
			want:   "token: (absent) -> sha256:bbb",
		},
		{
			name:   "removed",
			change: SecretChange{Field: "token", OldHash: "sha256:aaa"},
			want:   "token: sha256:aaa -> (absent)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.change.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReportStringNoChanges(t *testing.T) {
	var report Report

	if got := report.String(); !strings.Contains(got, "no structural changes") {
		t.Errorf("String() = %q, want it to mention no structural changes", got)
	}
}

func TestSecretHashesDisambiguatesRepeatedFieldNames(t *testing.T) {
	raw := `version: v1alpha1
machine:
  type: worker
  token: ` + tokenA + `
  ca:
    crt: ""
    key: ""
  registries:
    config:
      a.example.com:
        auth:
          password: first-password
      b.example.com:
        auth:
          password: second-password
cluster:
  id: dGVzdA==
  secret: ` + secret + `
  controlPlane:
    endpoint: https://10.0.0.1:6443
  token: aaaaaa.bbbbbbbbbbbbbbbb
  ca:
    crt: ""
    key: ""
`

	cfg, err := configloader.NewFromBytes([]byte(raw), configloader.WithNoValidation())
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	hashes, err := secretHashes(cfg)
	if err != nil {
		t.Fatalf("secretHashes: %v", err)
	}

	var passwordKeys int

	for field := range hashes {
		if strings.HasPrefix(field, "password") {
			passwordKeys++
		}
	}

	if passwordKeys < 2 {
		t.Errorf("expected both registry passwords to be keyed separately, got %d password keys in %v",
			passwordKeys, hashes)
	}
}

func empty(r Report) bool { return r.Structural == "" && len(r.SecretChanges) == 0 }
