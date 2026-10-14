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

package configsource

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestRevisionIsStableAndContentAddressed(t *testing.T) {
	t.Parallel()

	a := fstest.MapFS{
		"base/00-v1alpha1.yaml": &fstest.MapFile{Data: []byte("one")},
		"roles/worker.yaml":     &fstest.MapFile{Data: []byte("two")},
	}
	b := fstest.MapFS{
		"roles/worker.yaml":     &fstest.MapFile{Data: []byte("two")},
		"base/00-v1alpha1.yaml": &fstest.MapFile{Data: []byte("one")},
	}

	revA, err := RevisionOf(a)
	if err != nil {
		t.Fatalf("revision a: %v", err)
	}

	revB, err := RevisionOf(b)
	if err != nil {
		t.Fatalf("revision b: %v", err)
	}

	if revA != revB {
		t.Errorf("identical content produced different revisions %q and %q; rollouts would restart for nothing", revA, revB)
	}
}

func TestRevisionChangesWithContent(t *testing.T) {
	t.Parallel()

	base := fstest.MapFS{"base/a.yaml": &fstest.MapFile{Data: []byte("one")}}
	edited := fstest.MapFS{"base/a.yaml": &fstest.MapFile{Data: []byte("two")}}

	revBase, err := RevisionOf(base)
	if err != nil {
		t.Fatalf("revision base: %v", err)
	}

	revEdited, err := RevisionOf(edited)
	if err != nil {
		t.Fatalf("revision edited: %v", err)
	}

	if revBase == revEdited {
		t.Error("editing a file did not change the revision; drift would go unnoticed")
	}
}

func TestRevisionDistinguishesRenameFromEdit(t *testing.T) {
	t.Parallel()

	one := fstest.MapFS{"ab": &fstest.MapFile{Data: []byte("c")}}
	two := fstest.MapFS{"a": &fstest.MapFile{Data: []byte("bc")}}

	revOne, err := RevisionOf(one)
	if err != nil {
		t.Fatalf("revision one: %v", err)
	}

	revTwo, err := RevisionOf(two)
	if err != nil {
		t.Fatalf("revision two: %v", err)
	}

	if revOne == revTwo {
		t.Error("a rename hashed the same as an edit; the path and content streams are not separated")
	}
}

func TestDirFetcher(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "base"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "base", "00.yaml"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	src, err := DirFetcher{Path: dir}.Fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	got, err := fs.ReadFile(src.FS, "base/00.yaml")
	if err != nil {
		t.Fatalf("read through source FS: %v", err)
	}

	if string(got) != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}

	if src.Revision == "" {
		t.Error("revision must not be empty")
	}
}

func TestDirFetcherRejectsBadPaths(t *testing.T) {
	t.Parallel()

	if _, err := (DirFetcher{}).Fetch(context.Background()); err == nil {
		t.Error("expected an error for an empty path")
	}

	if _, err := (DirFetcher{Path: "/nonexistent/talos-lifecycle-test"}).Fetch(context.Background()); err == nil {
		t.Error("expected an error for a missing directory")
	}

	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := (DirFetcher{Path: file}).Fetch(context.Background()); err == nil {
		t.Error("expected an error when the path is a file, not a directory")
	}
}
