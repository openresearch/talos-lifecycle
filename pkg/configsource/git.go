package configsource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"strings"
	"testing/fstest"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// GitFetcher clones the tree shallowly into memory, so machine config does not
// depend on a workload in the cluster it configures.
type GitFetcher struct {
	URL  string
	Ref  string
	Path string

	Identity []byte
	// required: host key checking cannot be skipped
	KnownHosts []byte

	Auth    transport.AuthMethod
	Timeout time.Duration
}

const fetchTimeout = 30 * time.Second

// go-git's ssh connect ignores ctx, so the clone is abandoned at the timeout
// rather than holding the config worker while a try timer runs.
func (g GitFetcher) Fetch(ctx context.Context) (*Source, error) {
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = fetchTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type result struct {
		src *Source
		err error
	}

	done := make(chan result, 1)

	go func() {
		src, err := g.fetch(ctx)
		done <- result{src, err}
	}()

	select {
	case r := <-done:
		return r.src, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("fetch %s: %w", g.URL, ctx.Err())
	}
}

func (g GitFetcher) fetch(ctx context.Context) (*Source, error) {
	if g.URL == "" {
		return nil, errors.New("git source has no url")
	}

	ref := g.Ref
	if ref == "" {
		ref = "main"
	}

	auth := g.Auth
	if auth == nil {
		var err error

		auth, err = g.sshAuth()
		if err != nil {
			return nil, err
		}
	}

	repo, err := git.CloneContext(ctx, memory.NewStorage(), nil, &git.CloneOptions{
		URL:           g.URL,
		Auth:          auth,
		ReferenceName: plumbing.NewBranchReferenceName(ref),
		SingleBranch:  true,
		Depth:         1,
		Tags:          git.NoTags,
	})
	if err != nil {
		return nil, fmt.Errorf("clone %s@%s: %w", g.URL, ref, err)
	}

	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", ref, err)
	}

	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, fmt.Errorf("read commit %s: %w", head.Hash(), err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("read tree of %s: %w", head.Hash(), err)
	}

	dir := strings.Trim(path.Clean("/"+g.Path), "/")
	if dir != "" {
		tree, err = tree.Tree(dir)
		if err != nil {
			return nil, fmt.Errorf("path %q at %s: %w", dir, head.Hash(), err)
		}
	}

	fsys, err := treeFS(tree)
	if err != nil {
		return nil, err
	}

	// a content hash, so a commit elsewhere in the repo is not a change
	rev, err := RevisionOf(fsys)
	if err != nil {
		return nil, err
	}

	return &Source{FS: fsys, Revision: rev, Commit: head.Hash().String()}, nil
}

func treeFS(tree *object.Tree) (fstest.MapFS, error) {
	fsys := fstest.MapFS{}

	err := tree.Files().ForEach(func(f *object.File) error {
		if !f.Mode.IsFile() {
			return nil
		}

		content, err := f.Contents()
		if err != nil {
			return fmt.Errorf("read %s: %w", f.Name, err)
		}

		fsys[f.Name] = &fstest.MapFile{Data: []byte(content)}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(fsys) == 0 {
		return nil, errors.New("git source path holds no files")
	}

	return fsys, nil
}

func (g GitFetcher) sshAuth() (transport.AuthMethod, error) {
	if len(g.Identity) == 0 {
		return nil, errors.New("git source secret has no identity (ssh private key)")
	}

	callback, err := knownHostsCallback(g.KnownHosts)
	if err != nil {
		return nil, err
	}

	keys, err := gitssh.NewPublicKeys("git", g.Identity, "")
	if err != nil {
		return nil, fmt.Errorf("load ssh identity: %w", err)
	}

	keys.HostKeyCallback = callback

	return keys, nil
}

// Hashed known_hosts entries (|1|...) are not supported.
func knownHostsCallback(content []byte) (ssh.HostKeyCallback, error) {
	type entry struct {
		hosts []string
		key   ssh.PublicKey
	}

	var entries []entry

	rest := content

	for len(bytes.TrimSpace(rest)) > 0 {
		_, hosts, key, _, next, err := ssh.ParseKnownHosts(rest)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return nil, fmt.Errorf("parse known_hosts: %w", err)
		}

		entries = append(entries, entry{hosts: hosts, key: key})
		rest = next
	}

	if len(entries) == 0 {
		return nil, errors.New("git source secret has no known_hosts entries; host key checking is required")
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		want := []string{knownhosts.Normalize(hostname)}
		if remote != nil {
			want = append(want, knownhosts.Normalize(remote.String()))
		}

		for _, e := range entries {
			for _, h := range e.hosts {
				for _, w := range want {
					if knownhosts.Normalize(h) == w && bytes.Equal(e.key.Marshal(), key.Marshal()) {
						return nil
					}
				}
			}
		}

		return fmt.Errorf("host key for %s not in known_hosts", hostname)
	}, nil
}
