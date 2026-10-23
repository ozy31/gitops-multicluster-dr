package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// setup creates a throwaway GitOps repo with a flag file and a drswitch
// config whose health probes point at a local test server. Route53 is
// disabled so the test exercises the real Git and HTTP code paths only.
func setup(t *testing.T, drHealthy bool) (cfgPath, repo string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dr" && !drHealthy {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	repo = t.TempDir()
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME": "test", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "test", "GIT_COMMITTER_EMAIL": "t@example.com",
		"GIT_CONFIG_GLOBAL": os.DevNull,
	} {
		t.Setenv(k, v)
	}
	mustGit(t, repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(repo, "dr", "active-site.yaml"), "dr:\n  activeSite: primary\n")
	cfgPath = filepath.Join(repo, "dr", "drswitch.yaml")
	write(t, cfgPath, fmt.Sprintf(`git:
  repoPath: ..
sites:
  primary:
    health: {url: %[1]s/primary}
  dr:
    health: {url: %[1]s/dr}
    ready: {url: %[1]s/dr-ready}
ready:
  timeout: 2s
  interval: 10ms
`, srv.URL))
	mustGit(t, repo, "add", ".")
	mustGit(t, repo, "commit", "-q", "-m", "init")
	return cfgPath, repo
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func run(t *testing.T, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := NewRootCmd("test", DefaultFactory)
	var o, e bytes.Buffer
	cmd.SetArgs(args)
	cmd.SetOut(&o)
	cmd.SetErr(&e)
	cmd.SetIn(strings.NewReader(stdin))
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		return o.String(), e.String() + err.Error(), 1
	}
	return o.String(), e.String(), 0
}

func TestFailoverAndFailbackEndToEnd(t *testing.T) {
	cfg, repo := setup(t, true)

	out, errOut, code := run(t, "", "failover", "-c", cfg, "--yes", "--reason", "game day")
	if code != 0 {
		t.Fatalf("failover failed: %s", errOut)
	}
	if !strings.Contains(out, "failover complete: primary -> dr") {
		t.Fatalf("stdout = %q", out)
	}
	if got := mustGit(t, repo, "show", "HEAD:dr/active-site.yaml"); !strings.Contains(got, "activeSite: dr") {
		t.Fatalf("flag not committed: %s", got)
	}
	log := mustGit(t, repo, "log", "--format=%s", "-3")
	wantLog := "chore(dr): failover activeSite both -> dr\nchore(dr): scale up both sites (activeSite primary -> both)\ninit\n"
	if log != wantLog {
		t.Fatalf("git log =\n%s\nwant\n%s", log, wantLog)
	}
	if msg := mustGit(t, repo, "log", "-1", "--format=%B"); !strings.Contains(msg, "Reason: game day") {
		t.Fatalf("commit message = %q", msg)
	}

	out, _, code = run(t, "", "failback", "-c", cfg, "--yes")
	if code != 0 || !strings.Contains(out, "failback complete: dr -> primary") {
		t.Fatalf("failback: code=%d out=%q", code, out)
	}
}

func TestStatusJSON(t *testing.T) {
	cfg, _ := setup(t, false)
	out, errOut, code := run(t, "", "status", "-c", cfg, "-o", "json")
	if code != 0 {
		t.Fatalf("status failed: %s", errOut)
	}
	var st struct {
		FlagActiveSite string                       `json:"flagActiveSite"`
		Health         map[string]map[string]string `json:"health"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("invalid json %q: %v", out, err)
	}
	if st.FlagActiveSite != "primary" || st.Health["dr"]["status"] != "unhealthy" || st.Health["primary"]["status"] != "healthy" {
		t.Fatalf("status = %+v", st)
	}
}

func TestDryRunDoesNotCommit(t *testing.T) {
	cfg, repo := setup(t, true)
	before := mustGit(t, repo, "rev-parse", "HEAD")
	out, errOut, code := run(t, "", "failover", "-c", cfg, "--dry-run")
	if code != 0 {
		t.Fatalf("dry-run failed: %s", errOut)
	}
	if !strings.Contains(out, "nothing was changed") || !strings.Contains(errOut, "[dry-run] set activeSite primary -> both") {
		t.Fatalf("out=%q err=%q", out, errOut)
	}
	if after := mustGit(t, repo, "rev-parse", "HEAD"); after != before {
		t.Fatal("dry-run created a commit")
	}
}

func TestFailoverRefusesUnhealthyTarget(t *testing.T) {
	cfg, repo := setup(t, false)
	before := mustGit(t, repo, "rev-parse", "HEAD")
	_, errOut, code := run(t, "", "failover", "-c", cfg, "--yes")
	if code == 0 || !strings.Contains(errOut, "failed health check") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	if after := mustGit(t, repo, "rev-parse", "HEAD"); after != before {
		t.Fatal("unhealthy target must not produce a commit")
	}
}

func TestConfirmationPrompt(t *testing.T) {
	cfg, _ := setup(t, true)
	if _, errOut, code := run(t, "primary\n", "failover", "-c", cfg); code == 0 || !strings.Contains(errOut, "aborted") {
		t.Fatalf("wrong confirmation should abort: %q", errOut)
	}
	if out, errOut, code := run(t, "dr\n", "failover", "-c", cfg); code != 0 || !strings.Contains(out, "complete") {
		t.Fatalf("correct confirmation should proceed: %q %q", out, errOut)
	}
}

func TestBrokenPrimaryProbeDoesNotBlockFailover(t *testing.T) {
	cfg, _ := setup(t, true)
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Point the primary probe at a kubeconfig context that does not exist,
	// as if the primary region's credentials were unavailable.
	re := regexp.MustCompile(`health: \{url: \S+/primary\}`)
	broken := re.ReplaceAllString(string(data), "health: {kube: {context: gone, kubeconfig: /nonexistent}}")
	if broken == string(data) {
		t.Fatal("primary probe not found in test config")
	}
	write(t, cfg, broken)

	out, errOut, code := run(t, "", "status", "-c", cfg)
	if code != 0 || !regexp.MustCompile(`health primary:\s+unhealthy\s+sites.primary.health`).MatchString(out) {
		t.Fatalf("status code=%d out=%q err=%q", code, out, errOut)
	}
	if _, errOut, code := run(t, "", "failover", "-c", cfg, "--yes"); code != 0 {
		t.Fatalf("failover should not depend on the primary probe: %s", errOut)
	}
}
