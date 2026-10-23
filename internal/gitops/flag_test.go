package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ozy31/gitops-multicluster-dr/internal/site"
)

func newTestRepo(t *testing.T) *ExecRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(dir, "dr"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dr", "active-site.yaml"), []byte(flagDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	// An unrelated dirty file must not end up in the flag commit.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "init")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	return &ExecRepo{Dir: dir, Remote: "origin", Branch: "main"}
}

func TestFlagStoreRoundTripWithGit(t *testing.T) {
	repo := newTestRepo(t)
	fs := &FlagStore{Repo: repo, File: "dr/active-site.yaml", Key: "dr.activeSite"}
	ctx := context.Background()

	got, err := fs.Active(ctx)
	if err != nil || got != site.Primary {
		t.Fatalf("Active = %q, %v", got, err)
	}

	sha, err := fs.SetActive(ctx, site.DR, "region outage drill")
	if err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if len(sha) < 40 {
		t.Fatalf("unexpected sha %q", sha)
	}
	if got, _ := fs.Active(ctx); got != site.DR {
		t.Fatalf("Active after flip = %q", got)
	}

	out, err := exec.Command("git", "-C", repo.Dir, "show", "--stat", "--format=%B", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	log := string(out)
	if !strings.Contains(log, "chore(dr): failover activeSite primary -> dr") || !strings.Contains(log, "Reason: region outage drill") {
		t.Errorf("unexpected commit message:\n%s", log)
	}
	if strings.Contains(log, "README.md") {
		t.Errorf("flag commit must only contain the flag file:\n%s", log)
	}

	// Setting the same value again is a no-op.
	sha, err = fs.SetActive(ctx, site.DR, "")
	if err != nil || sha != "" {
		t.Fatalf("second SetActive = %q, %v; want no-op", sha, err)
	}
}

type memRepo struct {
	files   map[string][]byte
	commits []string
	pushes  int
	pushErr error
}

func (m *memRepo) ReadFile(p string) ([]byte, error) {
	b, ok := m.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}
func (m *memRepo) WriteFile(p string, b []byte) error { m.files[p] = b; return nil }
func (m *memRepo) Commit(_ context.Context, msg string, _ ...string) (string, error) {
	m.commits = append(m.commits, msg)
	return "0123456789abcdef0123456789abcdef01234567", nil
}
func (m *memRepo) Push(context.Context) error { m.pushes++; return m.pushErr }

func TestFlagStorePush(t *testing.T) {
	repo := &memRepo{files: map[string][]byte{"f.yaml": []byte("dr:\n  activeSite: dr\n")}}
	fs := &FlagStore{Repo: repo, File: "f.yaml", Key: "dr.activeSite", Push: true}
	if _, err := fs.SetActive(context.Background(), site.Primary, ""); err != nil {
		t.Fatal(err)
	}
	if repo.pushes != 1 {
		t.Fatalf("pushes = %d", repo.pushes)
	}
	if !strings.HasPrefix(repo.commits[0], "chore(dr): failback activeSite dr -> primary") {
		t.Fatalf("commit = %q", repo.commits[0])
	}
}

func TestFlagStoreRejectsUnknownValue(t *testing.T) {
	repo := &memRepo{files: map[string][]byte{"f.yaml": []byte("dr:\n  activeSite: moon\n")}}
	fs := &FlagStore{Repo: repo, File: "f.yaml", Key: "dr.activeSite"}
	if _, err := fs.Active(context.Background()); err == nil {
		t.Fatal("expected error for unknown site")
	}
}
