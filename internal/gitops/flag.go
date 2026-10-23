package gitops

import (
	"context"
	"fmt"

	"github.com/ozy31/gitops-multicluster-dr/internal/site"
)

// FlagStore reads and writes the activeSite flag in a Git repository.
type FlagStore struct {
	Repo Repo
	File string
	Key  string
	Push bool
}

// Active returns the site currently recorded in the flag file.
func (f *FlagStore) Active(_ context.Context) (site.Site, error) {
	data, err := f.Repo.ReadFile(f.File)
	if err != nil {
		return "", fmt.Errorf("read flag file: %w", err)
	}
	v, err := GetString(data, f.Key)
	if err != nil {
		return "", fmt.Errorf("%s: %w", f.File, err)
	}
	return site.ParseFlag(v)
}

// SetActive rewrites the flag, commits it and optionally pushes. It returns
// the commit SHA, or "" when the flag already had the requested value.
func (f *FlagStore) SetActive(ctx context.Context, target site.Site, reason string) (string, error) {
	data, err := f.Repo.ReadFile(f.File)
	if err != nil {
		return "", fmt.Errorf("read flag file: %w", err)
	}
	cur, err := GetString(data, f.Key)
	if err != nil {
		return "", fmt.Errorf("%s: %w", f.File, err)
	}
	if cur == string(target) {
		return "", nil
	}
	updated, err := SetString(data, f.Key, string(target))
	if err != nil {
		return "", fmt.Errorf("%s: %w", f.File, err)
	}
	if err := f.Repo.WriteFile(f.File, updated); err != nil {
		return "", fmt.Errorf("write flag file: %w", err)
	}
	sha, err := f.Repo.Commit(ctx, commitMessage(cur, string(target), reason), f.File)
	if err != nil {
		return "", err
	}
	if f.Push {
		if err := f.Repo.Push(ctx); err != nil {
			return sha, fmt.Errorf("commit %s created but push failed: %w", short(sha), err)
		}
	}
	return sha, nil
}

func commitMessage(from, to, reason string) string {
	var msg string
	switch to {
	case string(site.Both):
		msg = fmt.Sprintf("chore(dr): scale up both sites (activeSite %s -> %s)", from, to)
	case string(site.Primary):
		msg = fmt.Sprintf("chore(dr): failback activeSite %s -> %s", from, to)
	default:
		msg = fmt.Sprintf("chore(dr): failover activeSite %s -> %s", from, to)
	}
	if reason != "" {
		msg += "\n\nReason: " + reason
	}
	return msg
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
