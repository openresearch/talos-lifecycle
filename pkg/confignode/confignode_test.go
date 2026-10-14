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

package confignode

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	cfgA = `version: v1alpha1
debug: false
machine:
    type: worker
    token: aaaaaa.bbbbbbbbbbbbbbbb
    ca:
        crt: ""
        key: ""
    network:
        hostname: worker-0
cluster:
    id: test
    secret: c2VjcmV0
    controlPlane:
        endpoint: https://10.0.0.1:6443
`

	cfgB = `version: v1alpha1
debug: false
machine:
    type: worker
    token: aaaaaa.bbbbbbbbbbbbbbbb
    ca:
        crt: ""
        key: ""
    network:
        hostname: worker-0-renamed
cluster:
    id: test
    secret: c2VjcmV0
    controlPlane:
        endpoint: https://10.0.0.1:6443
`
)

func TestHashIsStableAndDiscriminating(t *testing.T) {
	t.Parallel()

	a, err := Load([]byte(cfgA))
	if err != nil {
		t.Fatalf("load cfgA: %v", err)
	}

	b, err := Load([]byte(cfgB))
	if err != nil {
		t.Fatalf("load cfgB: %v", err)
	}

	hashA, err := Hash(a)
	if err != nil {
		t.Fatalf("hash cfgA: %v", err)
	}

	hashA2, err := Hash(a)
	if err != nil {
		t.Fatalf("re-hash cfgA: %v", err)
	}

	hashB, err := Hash(b)
	if err != nil {
		t.Fatalf("hash cfgB: %v", err)
	}

	if hashA != hashA2 {
		t.Errorf("hash is not stable across calls: %q vs %q", hashA, hashA2)
	}

	if hashA == hashB {
		t.Error("configs differing in hostname hashed the same; drift detection would miss real changes")
	}

	if !strings.HasPrefix(hashA, "sha256:") {
		t.Errorf("hash %q lacks the sha256: prefix", hashA)
	}
}

func TestHashIgnoresSourceFormatting(t *testing.T) {
	t.Parallel()

	reformatted := strings.ReplaceAll(cfgA, "    ", "  ")

	a, err := Load([]byte(cfgA))
	if err != nil {
		t.Fatalf("load cfgA: %v", err)
	}

	r, err := Load([]byte(reformatted))
	if err != nil {
		t.Fatalf("load reformatted: %v", err)
	}

	hashA, err := Hash(a)
	if err != nil {
		t.Fatalf("hash cfgA: %v", err)
	}

	hashR, err := Hash(r)
	if err != nil {
		t.Fatalf("hash reformatted: %v", err)
	}

	if hashA != hashR {
		t.Error("re-indenting the source changed the hash; the controller would roll out on formatting churn")
	}
}

func TestTryWritesActiveSlotOnly(t *testing.T) {
	t.Parallel()

	node := NewFakeNode([]byte(cfgA))
	ctx := context.Background()

	if _, err := node.ApplyTry(ctx, []byte(cfgB), time.Minute); err != nil {
		t.Fatalf("apply try: %v", err)
	}

	if !bytes.Equal(node.SlotBytes(SlotActive), []byte(cfgB)) {
		t.Error("TRY did not write the active slot")
	}

	if !bytes.Equal(node.SlotBytes(SlotPersistent), []byte(cfgA)) {
		t.Error("TRY wrote the persistent slot; this would invalidate the entire design")
	}
}

func TestTryRevertsWithoutAnyCall(t *testing.T) {
	t.Parallel()

	node := NewFakeNode([]byte(cfgA))
	advance := node.SetClock(time.Unix(0, 0))
	ctx := context.Background()

	if _, err := node.ApplyTry(ctx, []byte(cfgB), 10*time.Minute); err != nil {
		t.Fatalf("apply try: %v", err)
	}

	if !node.TryPending() {
		t.Fatal("try should still be pending immediately after the apply")
	}

	before := len(node.Calls)

	advance(11 * time.Minute)

	if node.TryPending() {
		t.Error("try still pending after the timer expired")
	}

	if !bytes.Equal(node.SlotBytes(SlotActive), []byte(cfgA)) {
		t.Error("node did not restore its previous active config")
	}

	if got := len(node.Calls) - before; got != 0 {
		t.Errorf("recovery path issued %d calls; it must require none", got)
	}
}

func TestRebootDiscardsTry(t *testing.T) {
	t.Parallel()

	node := NewFakeNode([]byte(cfgA))
	ctx := context.Background()

	if _, err := node.ApplyTry(ctx, []byte(cfgB), time.Hour); err != nil {
		t.Fatalf("apply try: %v", err)
	}

	node.Reboot()

	if !bytes.Equal(node.SlotBytes(SlotActive), []byte(cfgA)) {
		t.Error("a reboot mid-try must boot the persistent slot, which is the last known good config")
	}

	if node.TryPending() {
		t.Error("reboot should have discarded the pending try")
	}
}

func TestCommitPersistsAndCancelsTimer(t *testing.T) {
	t.Parallel()

	node := NewFakeNode([]byte(cfgA))
	advance := node.SetClock(time.Unix(0, 0))
	ctx := context.Background()

	if _, err := node.ApplyTry(ctx, []byte(cfgB), 10*time.Minute); err != nil {
		t.Fatalf("apply try: %v", err)
	}

	if _, err := node.ApplyCommit(ctx, []byte(cfgB)); err != nil {
		t.Fatalf("apply commit: %v", err)
	}

	if node.TryPending() {
		t.Error("commit must cancel the try timer")
	}

	advance(time.Hour)

	if !bytes.Equal(node.SlotBytes(SlotActive), []byte(cfgB)) {
		t.Error("committed config reverted; the timer was not cancelled")
	}

	if !bytes.Equal(node.SlotBytes(SlotPersistent), []byte(cfgB)) {
		t.Error("commit did not write the persistent slot")
	}
}

func TestRejectedApplyIsDistinguishable(t *testing.T) {
	t.Parallel()

	node := NewFakeNode([]byte(cfgA))
	node.RejectApply = true

	_, err := node.ApplyTry(context.Background(), []byte(cfgB), time.Minute)
	if err == nil {
		t.Fatal("expected the apply to be rejected")
	}

	if !errors.Is(err, ErrRejected) {
		t.Errorf("rejection must be distinguishable from a transport error, got %v", err)
	}

	if !bytes.Equal(node.SlotBytes(SlotActive), []byte(cfgA)) {
		t.Error("a rejected apply must change nothing on the node")
	}
}

func TestRejectedCommitCancelsTheTryTimer(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	node := NewFakeNode([]byte(cfgA))

	if _, err := node.ApplyTry(ctx, []byte(cfgB), time.Minute); err != nil {
		t.Fatalf("try: %v", err)
	}

	node.RejectApply = true
	if _, err := node.ApplyCommit(ctx, []byte(cfgB)); !errors.Is(err, ErrRejected) {
		t.Fatalf("commit err = %v, want ErrRejected", err)
	}

	if node.TryPending() {
		t.Error("a rejected commit must cancel the try timer, as Talos does")
	}

	active, persistent := node.SlotBytes(SlotActive), node.SlotBytes(SlotPersistent)
	if !bytes.Equal(active, []byte(cfgB)) || !bytes.Equal(persistent, []byte(cfgA)) {
		t.Error("a rejected commit must leave the tried config active and nothing persisted")
	}
}

func TestZeroTryTimeoutRefused(t *testing.T) {
	t.Parallel()

	node := NewFakeNode([]byte(cfgA))

	if _, err := node.ApplyTry(context.Background(), []byte(cfgB), 0); err == nil {
		t.Error("a zero try timeout must be refused: it would apply with no revert armed")
	}
}

func TestUnreachableNodeFailsEveryCall(t *testing.T) {
	t.Parallel()

	node := NewFakeNode([]byte(cfgA))
	node.Unreachable = true
	ctx := context.Background()

	if _, err := node.ReadSlot(ctx, SlotActive); err == nil {
		t.Error("expected ReadSlot to fail on an unreachable node")
	}

	if _, err := node.ApplyTry(ctx, []byte(cfgB), time.Minute); err == nil {
		t.Error("expected ApplyTry to fail on an unreachable node")
	}
}

func TestFakeDialer(t *testing.T) {
	t.Parallel()

	node := NewFakeNode([]byte(cfgA))
	dialer := &FakeDialer{Nodes: map[string]*FakeNode{"10.0.0.1": node}}

	got, err := dialer.Dial(context.Background(), "10.0.0.1")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	if got != Client(node) {
		t.Error("dialer returned a different node than registered")
	}

	if _, err := dialer.Dial(context.Background(), "10.0.0.99"); err == nil {
		t.Error("expected an error dialling an unregistered address")
	}
}

func TestTalosDialerRejectsEmptyAddress(t *testing.T) {
	t.Parallel()

	if _, err := (TalosDialer{}).Dial(context.Background(), ""); err == nil {
		t.Error("expected an error for an empty node address")
	}
}
