// Package gitops reads and flips the activeSite flag stored in the GitOps
// repository that ArgoCD reconciles from.
package gitops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Repo is the minimal set of Git operations drswitch needs. It is an
// interface so the switch logic can be tested without a real repository.
type Repo interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte) error
	// Commit records only the given paths and returns the new commit SHA.
	Commit(ctx context.Context, message string, paths ...string) (string, error)
	Push(ctx context.Context) error
}

// ExecRepo implements Repo by shelling out to the git binary, which keeps
// the user's existing credential helpers, SSH agent and signing config.
type ExecRepo struct {
	Dir    string
	Remote string
	Branch string
}

var _ Repo = (*ExecRepo)(nil)

// ReadFile reads a file relative to the repository root.
func (r *ExecRepo) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(filepath.Join(r.Dir, path))
}

// WriteFile writes a file relative to the repository root, keeping its mode.
func (r *ExecRepo) WriteFile(path string, data []byte) error {
	full := filepath.Join(r.Dir, path)
	mode := os.FileMode(0o644)
	if st, err := os.Stat(full); err == nil {
		mode = st.Mode().Perm()
	}
	return os.WriteFile(full, data, mode)
}

// Commit stages and commits only the given paths.
func (r *ExecRepo) Commit(ctx context.Context, message string, paths ...string) (string, error) {
	addArgs := append([]string{"add", "--"}, paths...)
	if _, err := r.git(ctx, addArgs...); err != nil {
		return "", err
	}
	commitArgs := append([]string{"commit", "--quiet", "-m", message, "--"}, paths...)
	if _, err := r.git(ctx, commitArgs...); err != nil {
		return "", err
	}
	sha, err := r.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(sha), nil
}

// Push pushes HEAD to Remote/Branch.
func (r *ExecRepo) Push(ctx context.Context) error {
	_, err := r.git(ctx, "push", r.Remote, "HEAD:"+r.Branch)
	return err
}

func (r *ExecRepo) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.Dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
