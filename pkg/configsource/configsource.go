// Package configsource reads a tree of Talos patches and its revision.
package configsource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
)

type Source struct {
	FS fs.FS
	// content hash: the same tree has the same revision from any source
	Revision string
	Commit   string
}

type Fetcher interface {
	Fetch(ctx context.Context) (*Source, error)
}

type DirFetcher struct {
	Path string
}

func (d DirFetcher) Fetch(_ context.Context) (*Source, error) {
	if d.Path == "" {
		return nil, errors.New("source path is empty")
	}

	info, err := os.Stat(d.Path)
	if err != nil {
		return nil, fmt.Errorf("stat source path %q: %w", d.Path, err)
	}

	if !info.IsDir() {
		return nil, fmt.Errorf("source path %q is not a directory", d.Path)
	}

	fsys := os.DirFS(d.Path)

	rev, err := RevisionOf(fsys)
	if err != nil {
		return nil, err
	}

	return &Source{FS: fsys, Revision: rev}, nil
}

// RevisionOf includes lengths, so a rename cannot hash the same as an edit.
func RevisionOf(fsys fs.FS) (string, error) {
	var paths []string

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() {
			paths = append(paths, p)
		}

		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk source tree: %w", err)
	}

	sort.Strings(paths)

	h := sha256.New()

	for _, p := range paths {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", p, err)
		}

		_, _ = fmt.Fprintf(h, "%d:%s:%d:", len(p), p, len(b))
		_, _ = h.Write(b)
	}

	return hex.EncodeToString(h.Sum(nil))[:16], nil
}
