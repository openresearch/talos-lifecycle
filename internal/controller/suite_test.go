/*
Copyright 2026 OpenResearch GmbH.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
)

var (
	testCfg    *rest.Config
	k8sClient  client.Client
	testCtx    = context.Background()
	testEnvSet bool
)

func TestMain(m *testing.M) {
	code, err := runTests(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "envtest setup:", err)
		os.Exit(1)
	}

	os.Exit(code)
}

func runTests(m *testing.M) (int, error) {
	if err := upgradev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return 0, err
	}

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if dir := firstEnvTestBinaryDir(); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}

	cfg, err := testEnv.Start()
	if err != nil {
		return 0, err
	}

	defer func() { _ = testEnv.Stop() }()

	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return 0, err
	}

	testCfg, k8sClient, testEnvSet = cfg, c, true

	return m.Run(), nil
}

func requireEnvtest(t *testing.T) {
	t.Helper()

	if !testEnvSet {
		t.Skip("envtest control plane not available")
	}
}

// firstEnvTestBinaryDir lets tests run from an IDE; run `make setup-envtest` first.
func firstEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")

	entries, err := os.ReadDir(basePath)
	if err != nil {
		return ""
	}

	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}

	return ""
}
