package configrender

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
)

var testCA = func() struct{ Crt, Key string } {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"talos-test"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		panic(err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		panic(err)
	}

	enc := func(blockType string, b []byte) string {
		return base64.StdEncoding.EncodeToString(
			pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: b}))
	}

	return struct{ Crt, Key string }{
		Crt: enc("CERTIFICATE", der),
		Key: enc("PRIVATE KEY", keyDER),
	}
}()

func minimalBase() string {
	return `version: v1alpha1
machine:
  type: worker
  token: aaaaaa.bbbbbbbbbbbbbbbb
  ca:
    crt: ` + testCA.Crt + `
    key: ""
  install:
    disk: /dev/sda
  network:
    hostname: placeholder
    interfaces:
      - interface: eth0
        addresses:
          - 10.0.0.1/24
cluster:
  id: dGVzdA==
  secret: dGVzdA==
  controlPlane:
    endpoint: https://10.0.0.1:6443
  token: aaaaaa.bbbbbbbbbbbbbbbb
  ca:
    crt: ` + testCA.Crt + `
    key: ""
`
}

var worker0 = Node{Name: "worker-0", Role: "worker"}

func onto(t *testing.T) config.Provider {
	t.Helper()

	cfg, err := configloader.NewFromBytes([]byte(minimalBase()))
	if err != nil {
		t.Fatalf("load base: %v", err)
	}

	return cfg
}

func encoded(t *testing.T, cfg config.Provider) string {
	t.Helper()

	b, err := cfg.EncodeBytes(encoder.WithComments(encoder.CommentsDisabled))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	return string(b)
}

func tree(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, content := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(content)}
	}

	return fsys
}

func render(t *testing.T, files map[string]string, node Node, base config.Provider) *Result {
	t.Helper()

	res, err := Render(tree(files), node, base)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	return res
}

const sysctlA = "machine:\n  sysctls:\n    net.core.somaxconn: \"8192\"\n"

func TestKeepsUnmanagedFields(t *testing.T) {
	res := render(t, map[string]string{"base/10.yaml": sysctlA}, worker0, onto(t))

	for _, want := range []string{
		`net.core.somaxconn: "8192"`,
		"token: aaaaaa.bbbbbbbbbbbbbbbb",
		"disk: /dev/sda",
		"hostname: placeholder",
	} {
		if !strings.Contains(string(res.Bytes), want) {
			t.Errorf("rendered config lacks %q", want)
		}
	}
}

func TestNoOpIsByteIdentical(t *testing.T) {
	base := onto(t)
	restate := "machine:\n  network:\n    hostname: placeholder\n"
	res := render(t, map[string]string{"nodes/worker-0.yaml": restate}, worker0, base)

	if string(res.Bytes) != encoded(t, base) {
		t.Error("restating an existing value changed the encoding")
	}
}

func TestLayeringOrder(t *testing.T) {
	res := render(t, map[string]string{
		"base/00.yaml":        "machine:\n  network:\n    hostname: from-base\n",
		"roles/worker.yaml":   "machine:\n  network:\n    hostname: from-role\n",
		"nodes/worker-0.yaml": "- op: replace\n  path: /machine/network/hostname\n  value: from-node\n",
	}, worker0, onto(t))

	if !strings.Contains(string(res.Bytes), "hostname: from-node") {
		t.Error("node patch must win over role and base")
	}

	want := []string{"base/00.yaml", "roles/worker.yaml", "nodes/worker-0.yaml"}
	if strings.Join(res.Sources, ",") != strings.Join(want, ",") {
		t.Errorf("sources = %v, want %v", res.Sources, want)
	}
}

func TestRoleScopedBase(t *testing.T) {
	files := map[string]string{
		"base/00.yaml":        "machine:\n  network:\n    hostname: shared\n",
		"base/worker/00.yaml": "machine:\n  network:\n    hostname: worker-base\n",
	}

	if res := render(t, files, worker0, onto(t)); !strings.Contains(string(res.Bytes), "hostname: worker-base") {
		t.Error("base/worker/ must win over base/")
	}

	cp := Node{Name: "control-plane-0", Role: "control-plane"}
	if res := render(t, files, cp, onto(t)); !strings.Contains(string(res.Bytes), "hostname: shared") {
		t.Error("a role without its own base must fall back to base/")
	}

	files["base/worker/README.md"] = "notes"
	delete(files, "base/worker/00.yaml")

	if res := render(t, files, worker0, onto(t)); !strings.Contains(string(res.Bytes), "hostname: shared") {
		t.Error("an empty role directory must fall back to base/")
	}
}

func TestDoesNotMutateOnto(t *testing.T) {
	base := onto(t)
	before := encoded(t, base)

	render(t, map[string]string{"base/00.yaml": "machine:\n  network:\n    hostname: changed\n"}, worker0, base)

	if encoded(t, base) != before {
		t.Error("Render mutated onto")
	}
}

func TestRenderErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string]string
		node  Node
		nilOn bool
	}{
		"empty tree":             {files: map[string]string{}, node: worker0},
		"no node name":           {files: map[string]string{"base/00.yaml": sysctlA}, node: Node{Role: "worker"}},
		"nothing to render onto": {files: map[string]string{"base/00.yaml": sysctlA}, node: worker0, nilOn: true},
		"unparseable patch":      {files: map[string]string{"base/00.yaml": "{not yaml"}, node: worker0},
		"invalid result": {files: map[string]string{"base/00.yaml": "machine:\n  network:\n    interfaces:\n" +
			"      - interface: eth0\n        addresses:\n          - not-an-address\n"}, node: worker0},
	} {
		t.Run(name, func(t *testing.T) {
			base := onto(t)
			if tc.nilOn {
				base = nil
			}

			if _, err := Render(tree(tc.files), tc.node, base); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestRebootOnlyFieldsKeepTheNodeValue(t *testing.T) {
	without := render(t, map[string]string{"base/00.yaml": sysctlA}, worker0, onto(t))

	for name, tc := range map[string]struct{ patch, field string }{
		"env":       {"machine:\n  env:\n    HTTP_PROXY: http://proxy:3128\n", "v1alpha1: machine.env"},
		"etcd args": {"cluster:\n  etcd:\n    extraArgs:\n      election-timeout: \"5000\"\n", "v1alpha1: cluster.etcd"},
		"kernel module": {"- op: add\n  path: /machine/kernel\n  value:\n    modules:\n      - name: br_netfilter\n",
			"v1alpha1: machine.kernel"},
		"module document": {"apiVersion: v1alpha1\nkind: KernelModuleConfig\nname: br_netfilter\n",
			"KernelModuleConfig/br_netfilter"},
		"environment document": {
			"apiVersion: v1alpha1\nkind: EnvironmentConfig\nvariables:\n  HTTP_PROXY: http://proxy:3128\n",
			"EnvironmentConfig",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := render(t, map[string]string{"base/00.yaml": sysctlA, "base/10.yaml": tc.patch}, worker0, onto(t))

			if len(got.Skipped) != 1 || got.Skipped[0] != tc.field {
				t.Errorf("skipped = %v, want [%s]", got.Skipped, tc.field)
			}

			if !bytes.Equal(got.Bytes, without.Bytes) {
				t.Errorf("the reboot-only change leaked or the rest was dropped:\n%s", got.Bytes)
			}
		})
	}
}

func TestRebootOnlyFieldOnTheNodeIsKept(t *testing.T) {
	node, err := configloader.NewFromBytes([]byte(strings.Replace(minimalBase(), "machine:\n",
		"machine:\n  env:\n    HTTP_PROXY: http://proxy:3128\n", 1)))
	if err != nil {
		t.Fatalf("load node config: %v", err)
	}

	got := render(t, map[string]string{"base/00.yaml": sysctlA}, worker0, node)

	if len(got.Skipped) != 0 {
		t.Errorf("skipped = %v, want none", got.Skipped)
	}

	if !strings.Contains(string(got.Bytes), "HTTP_PROXY") {
		t.Error("the node's own env was dropped")
	}
}

func TestGatewayOutsideSubnetIsAccepted(t *testing.T) {
	render(t, map[string]string{"base/00.yaml": "- op: add\n  path: /machine/network/interfaces/0/routes\n" +
		"  value:\n    - network: 0.0.0.0/0\n      gateway: 198.51.100.1\n"}, worker0, onto(t))
}

func TestOlderKubernetesThanThisBuildSupportsRenders(t *testing.T) {
	base, err := configloader.NewFromBytes([]byte(strings.Replace(minimalBase(), "  install:\n",
		"  kubelet:\n    image: ghcr.io/siderolabs/kubelet:v1.31.0\n  install:\n", 1)))
	if err != nil {
		t.Fatalf("load base: %v", err)
	}

	render(t, map[string]string{"base/00.yaml": sysctlA}, worker0, base)
}

func TestListAppendIsRefused(t *testing.T) {
	_, err := Render(tree(map[string]string{"base/00.yaml": "machine:\n  time:\n    servers:\n      - ntp.example.com\n"}),
		worker0, onto(t))
	if err == nil || !strings.Contains(err.Error(), "not idempotent") {
		t.Fatalf("expected an idempotency error, got %v", err)
	}
}

func TestJSON6902AddSetsList(t *testing.T) {
	res := render(t, map[string]string{
		"base/00.yaml": "- op: add\n  path: /machine/time\n  value:\n    servers:\n      - ntp.example.com\n",
	}, worker0, onto(t))

	if n := strings.Count(string(res.Bytes), "ntp.example.com"); n != 1 {
		t.Errorf("server appears %d times, want 1", n)
	}
}

func TestRemovalIsIdempotent(t *testing.T) {
	for name, removal := range map[string]string{
		"strategic": "machine:\n  sysctls:\n    net.core.somaxconn:\n      $patch: delete\n",
		"json6902":  "- op: remove\n  path: /machine/sysctls/net.core.somaxconn\n",
	} {
		t.Run(name, func(t *testing.T) {
			set := render(t, map[string]string{"base/00.yaml": sysctlA}, worker0, onto(t))
			removed := render(t, map[string]string{"nodes/worker-0.yaml": removal}, worker0, set.Config)

			if strings.Contains(string(removed.Bytes), "somaxconn") {
				t.Fatal("removal did not remove the field")
			}

			again := render(t, map[string]string{"nodes/worker-0.yaml": removal}, worker0, removed.Config)
			if string(again.Bytes) != string(removed.Bytes) {
				t.Error("an applied removal must render the node unchanged")
			}

			if len(again.Warnings) == 0 {
				t.Error("a skipped removal should be visible as a warning")
			}
		})
	}
}

func TestMixedFileWithMissingDeleteFails(t *testing.T) {
	_, err := Render(tree(map[string]string{"base/00.yaml": "machine:\n  network:\n    hostname: changed\n" +
		"  sysctls:\n    not.there:\n      $patch: delete\n"}), worker0, onto(t))
	if err == nil {
		t.Error("a mixed file whose delete misses must fail")
	}
}

func TestIsRemovalOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want bool
	}{
		"strategic delete": {"machine:\n  sysctls:\n    a:\n      $patch: delete\n", true},
		"strategic set":    {"machine:\n  sysctls:\n    a: \"1\"\n", false},
		"mixed":            {"machine:\n  sysctls:\n    a: \"1\"\n    b:\n      $patch: delete\n", false},
		"list item delete": {
			"machine:\n  network:\n    interfaces:\n      - interface: eth0\n        $patch: delete\n", true,
		},
		"document delete": {"apiVersion: v1alpha1\nkind: SysctlConfig\n$patch: delete\n", true},
		"json6902 remove": {"- op: remove\n  path: /machine/sysctls/a\n", true},
		"json6902 mixed":  {"- op: remove\n  path: /a\n- op: add\n  path: /b\n  value: 1\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			patch, err := configpatcher.LoadPatch([]byte(tc.in))
			if err != nil {
				t.Fatalf("load: %v", err)
			}

			if got := isRemovalOnly([]byte(tc.in), patch); got != tc.want {
				t.Errorf("isRemovalOnly = %v, want %v", got, tc.want)
			}
		})
	}
}
