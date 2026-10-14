package configsource

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
)

// gitRepo needs the git binary (the file transport shells out to it); tests skip without it.
func gitRepo(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}

	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()

		cmd := exec.Command("git", args...)
		cmd.Dir = dir

		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")

		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run("init", "-q", "-b", "main")

	return dir
}

func writeAndCommit(t *testing.T, repo string, files map[string]string, msg string) {
	t.Helper()

	for name, content := range files {
		p := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", msg}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo

		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")

		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

type noAuth struct{}

func (noAuth) Name() string   { return "none" }
func (noAuth) String() string { return "none" }

func TestGitFetcherReadsPathAtRef(t *testing.T) {
	t.Parallel()

	repo := gitRepo(t)
	writeAndCommit(t, repo, map[string]string{
		"machineconfig/base/worker/10.yaml": "machine:\n  sysctls:\n    a: \"1\"\n",
		"terraform/main.tf":                 "# unrelated\n",
	}, "init")

	src, err := GitFetcher{URL: "file://" + repo, Path: "machineconfig", Auth: noAuth{}}.Fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	got, err := fs.ReadFile(src.FS, "base/worker/10.yaml")
	if err != nil {
		t.Fatalf("read through FS: %v", err)
	}

	if !strings.Contains(string(got), "a: \"1\"") {
		t.Errorf("unexpected content %q", got)
	}

	if _, err := fs.ReadFile(src.FS, "terraform/main.tf"); err == nil {
		t.Error("files outside Path must not be visible")
	}

	if len(src.Commit) != 40 {
		t.Errorf("commit = %q, want a full SHA", src.Commit)
	}
}

func TestGitFetcherRevisionIgnoresUnrelatedCommits(t *testing.T) {
	t.Parallel()

	repo := gitRepo(t)
	writeAndCommit(t, repo, map[string]string{"mc/base/00.yaml": "x: 1\n", "other.txt": "a\n"}, "one")

	f := GitFetcher{URL: "file://" + repo, Path: "mc", Auth: noAuth{}}

	first, err := f.Fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	writeAndCommit(t, repo, map[string]string{"other.txt": "b\n"}, "unrelated")

	second, err := f.Fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if first.Commit == second.Commit {
		t.Fatal("setup: commit should have moved")
	}

	if first.Revision != second.Revision {
		t.Error("an unrelated commit changed the revision")
	}

	writeAndCommit(t, repo, map[string]string{"mc/base/00.yaml": "x: 2\n"}, "related")

	third, err := f.Fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if third.Revision == second.Revision {
		t.Error("a change under Path must change the revision")
	}
}

func TestGitAndDirAgreeOnRevision(t *testing.T) {
	t.Parallel()

	repo := gitRepo(t)
	writeAndCommit(t, repo, map[string]string{"mc/base/00.yaml": "same\n"}, "init")

	fromGit, err := GitFetcher{URL: "file://" + repo, Path: "mc", Auth: noAuth{}}.Fetch(context.Background())
	if err != nil {
		t.Fatalf("git fetch: %v", err)
	}

	fromDir, err := DirFetcher{Path: filepath.Join(repo, "mc")}.Fetch(context.Background())
	if err != nil {
		t.Fatalf("dir fetch: %v", err)
	}

	if fromGit.Revision != fromDir.Revision {
		t.Errorf("git %q != dir %q for identical content", fromGit.Revision, fromDir.Revision)
	}
}

func TestGitFetcherErrors(t *testing.T) {
	t.Parallel()

	repo := gitRepo(t)
	writeAndCommit(t, repo, map[string]string{"mc/base/00.yaml": "x\n"}, "init")

	ctx := context.Background()

	if _, err := (GitFetcher{}).Fetch(ctx); err == nil {
		t.Error("expected an error without a url")
	}

	if _, err := (GitFetcher{URL: "file://" + repo, Path: "nope", Auth: noAuth{}}).Fetch(ctx); err == nil {
		t.Error("expected an error for a missing path")
	}

	if _, err := (GitFetcher{URL: "file://" + repo, Ref: "nope", Auth: noAuth{}}).Fetch(ctx); err == nil {
		t.Error("expected an error for a missing branch")
	}

	if _, err := (GitFetcher{URL: "ssh://git@example.invalid/x.git"}).Fetch(ctx); err == nil {
		t.Error("expected an error without an identity")
	}
}

func TestKnownHostsCallback(t *testing.T) {
	t.Parallel()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}

	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewPublicKey(otherPub)

	line := "gitlab.example.com " + string(ssh.MarshalAuthorizedKey(key))

	cb, err := knownHostsCallback([]byte("# comment\n" + line))
	if err != nil {
		t.Fatalf("callback: %v", err)
	}

	addr := &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 22}

	if err := cb("gitlab.example.com:22", addr, key); err != nil {
		t.Errorf("known host rejected: %v", err)
	}

	if err := cb("gitlab.example.com:22", addr, other); err == nil {
		t.Error("a different host key must be rejected")
	}

	if err := cb("evil.example.com:22", addr, key); err == nil {
		t.Error("an unknown host must be rejected")
	}

	if _, err := knownHostsCallback(nil); err == nil {
		t.Error("empty known_hosts must be refused; host key checking is not optional")
	}
}

func TestGitFetchGivesUpOnAStalledEndpoint(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	accepted := make(chan net.Conn, 4)

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()

		close(accepted)

		for c := range accepted {
			_ = c.Close()
		}
	})

	f := GitFetcher{
		URL:     "ssh://git@" + ln.Addr().String() + "/repo.git",
		Timeout: 200 * time.Millisecond,
		Auth: &gitssh.Password{User: "git", Password: "unused",
			HostKeyCallbackHelper: gitssh.HostKeyCallbackHelper{HostKeyCallback: ssh.InsecureIgnoreHostKey()}},
	}

	done := make(chan error, 1)
	go func() {
		_, err := f.Fetch(context.Background())
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a fetch from a silent endpoint succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fetch still blocked 5s after a 200ms timeout")
	}
}
