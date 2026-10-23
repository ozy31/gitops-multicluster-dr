package health

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ozy31/gitops-multicluster-dr/internal/config"
)

func TestHTTPChecker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "upstream down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if err := (&HTTPChecker{URL: srv.URL + "/ok"}).Check(context.Background()); err != nil {
		t.Fatalf("expected healthy: %v", err)
	}
	err := (&HTTPChecker{URL: srv.URL + "/bad"}).Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "status 503") || !strings.Contains(err.Error(), "upstream down") {
		t.Fatalf("err = %v", err)
	}
}

func TestHTTPCheckerTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	err := (&HTTPChecker{URL: srv.URL, Timeout: 50 * time.Millisecond}).Check(context.Background())
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestKubeCheckerUsesKubeconfigCredentials(t *testing.T) {
	var gotAuth atomic.Value
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: dr
  cluster:
    server: %s
    insecure-skip-tls-verify: true
users:
- name: drswitch
  user:
    token: s3cr3t
contexts:
- name: prod-dr
  context: {cluster: dr, user: drswitch}
current-context: prod-dr
`, srv.URL)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := NewKubeChecker(&config.KubeProbe{Context: "prod-dr", Path: "readyz"}, time.Second, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got := gotAuth.Load(); got != "Bearer s3cr3t" {
		t.Fatalf("Authorization = %v", got)
	}
	if c.String() != "kube[prod-dr]/readyz" {
		t.Fatalf("String = %q", c.String())
	}
}

func TestKubeCheckerUnknownContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewKubeChecker(&config.KubeProbe{Context: "nope", Path: "/readyz"}, time.Second, path); err == nil {
		t.Fatal("expected error for unknown context")
	}
}

type flaky struct{ fails, calls int }

func (f *flaky) Check(context.Context) error {
	f.calls++
	if f.calls <= f.fails {
		return fmt.Errorf("not yet (%d)", f.calls)
	}
	return nil
}
func (f *flaky) String() string { return "flaky" }

func TestWaitHealthy(t *testing.T) {
	f := &flaky{fails: 2}
	if err := WaitHealthy(context.Background(), f, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if f.calls != 3 {
		t.Fatalf("calls = %d", f.calls)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := WaitHealthy(ctx, &flaky{fails: 1 << 30}, 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "not healthy before deadline") {
		t.Fatalf("err = %v", err)
	}
}
