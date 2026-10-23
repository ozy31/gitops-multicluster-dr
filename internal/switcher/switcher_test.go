package switcher

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ozy31/gitops-multicluster-dr/internal/health"
	"github.com/ozy31/gitops-multicluster-dr/internal/site"
)

// events records the order in which side effects happen.
type events []string

type fakeFlag struct {
	active site.Site
	ev     *events
}

func (f *fakeFlag) Active(context.Context) (site.Site, error) { return f.active, nil }
func (f *fakeFlag) SetActive(_ context.Context, v site.Site, _ string) (string, error) {
	if f.active == v {
		return "", nil
	}
	*f.ev = append(*f.ev, "flag:"+string(v))
	f.active = v
	return "sha-" + string(v), nil
}

type fakeDNS struct {
	weights map[site.Site]int64
	ev      *events
	err     error
}

func (d *fakeDNS) Weights(context.Context) (map[site.Site]int64, error) { return d.weights, nil }
func (d *fakeDNS) Shift(_ context.Context, a site.Site, _ string) (string, error) {
	if d.err != nil {
		return "", d.err
	}
	*d.ev = append(*d.ev, "dns:"+string(a))
	d.weights = map[site.Site]int64{a: 100, a.Other(): 0}
	return "/change/1", nil
}

type fakeCheck struct {
	name string
	err  error
	ev   *events
}

func (c *fakeCheck) Check(context.Context) error {
	if c.ev != nil {
		*c.ev = append(*c.ev, "check:"+c.name)
	}
	return c.err
}
func (c *fakeCheck) String() string { return c.name }

func newSwitcher(ev *events) (*Switcher, *fakeFlag, *fakeDNS) {
	f := &fakeFlag{active: site.Primary, ev: ev}
	d := &fakeDNS{weights: map[site.Site]int64{site.Primary: 100, site.DR: 0}, ev: ev}
	s := &Switcher{
		Flag: f,
		DNS:  d,
		Health: map[site.Site]health.Checker{
			site.Primary: &fakeCheck{name: "primary-health", ev: ev},
			site.DR:      &fakeCheck{name: "dr-health", ev: ev},
		},
		Ready: map[site.Site]health.Checker{
			site.DR: &fakeCheck{name: "dr-ready", ev: ev},
		},
		ReadyTimeout:  time.Second,
		ReadyInterval: time.Millisecond,
		Out:           &bytes.Buffer{},
		Sleep: func(_ context.Context, d time.Duration) error {
			*ev = append(*ev, "drain:"+d.String())
			return nil
		},
	}
	return s, f, d
}

func TestFailoverOrder(t *testing.T) {
	var ev events
	s, f, d := newSwitcher(&ev)
	res, err := s.SwitchTo(context.Background(), site.DR, Options{Reason: "test", Drain: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	want := "check:dr-health,flag:both,check:dr-ready,dns:dr,drain:1m0s,flag:dr"
	if got := strings.Join(ev, ","); got != want {
		t.Fatalf("order =\n  %s\nwant\n  %s", got, want)
	}
	if f.active != site.DR || d.weights[site.DR] != 100 || res.ChangeID != "/change/1" {
		t.Fatalf("unexpected result %+v", res)
	}
	if strings.Join(res.Commits, ",") != "sha-both,sha-dr" {
		t.Fatalf("commits = %v", res.Commits)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	var ev events
	s, f, d := newSwitcher(&ev)
	res, err := s.SwitchTo(context.Background(), site.DR, Options{DryRun: true, Drain: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if f.active != site.Primary || d.weights[site.Primary] != 100 {
		t.Fatal("dry-run must not change flag or DNS")
	}
	if got := strings.Join(ev, ","); got != "check:dr-health" {
		t.Fatalf("dry-run side effects: %s", got)
	}
	if len(res.Steps) != 6 || !res.DryRun {
		t.Fatalf("steps = %q", res.Steps)
	}
	out := s.Out.(*bytes.Buffer).String()
	for _, want := range []string{"[dry-run] set activeSite primary -> both", "[dry-run] shift Route53 weights to dr", "[dry-run] set activeSite -> dr"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestUnhealthyTargetAborts(t *testing.T) {
	var ev events
	s, f, _ := newSwitcher(&ev)
	s.Health[site.DR] = &fakeCheck{name: "dr-health", err: errors.New("connection refused")}
	_, err := s.SwitchTo(context.Background(), site.DR, Options{})
	if !errors.Is(err, ErrHealthCheck) {
		t.Fatalf("err = %v, want ErrHealthCheck", err)
	}
	if f.active != site.Primary || len(ev) != 0 {
		t.Fatalf("nothing should change on failed health check, events=%v", ev)
	}
}

func TestSkipHealthCheck(t *testing.T) {
	var ev events
	s, f, _ := newSwitcher(&ev)
	s.Health = nil
	if _, err := s.SwitchTo(context.Background(), site.DR, Options{}); err == nil {
		t.Fatal("missing probe should be an error without --skip-health-check")
	}
	if _, err := s.SwitchTo(context.Background(), site.DR, Options{SkipHealthCheck: true}); err != nil {
		t.Fatal(err)
	}
	if f.active != site.DR {
		t.Fatal("expected flip")
	}
}

func TestReadyTimeoutLeavesBothRunning(t *testing.T) {
	var ev events
	s, f, d := newSwitcher(&ev)
	s.Ready[site.DR] = &fakeCheck{name: "dr-ready", err: errors.New("0/3 ready")}
	s.ReadyTimeout = 20 * time.Millisecond
	_, err := s.SwitchTo(context.Background(), site.DR, Options{})
	if err == nil || !strings.Contains(err.Error(), "DNS was NOT changed") {
		t.Fatalf("err = %v", err)
	}
	if f.active != site.Both {
		t.Fatalf("flag = %q, want both (old site must keep running)", f.active)
	}
	if d.weights[site.Primary] != 100 {
		t.Fatal("DNS must not move while target is not ready")
	}

	// Re-running after the target recovers resumes without a second "both" commit.
	ev = nil
	s.Ready[site.DR] = &fakeCheck{name: "dr-ready", ev: &ev}
	if _, err := s.SwitchTo(context.Background(), site.DR, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ev, ","); got != "check:dr-health,check:dr-ready,dns:dr,flag:dr" {
		t.Fatalf("resume events = %s", got)
	}
}

func TestDNSFailureKeepsBoth(t *testing.T) {
	var ev events
	s, f, d := newSwitcher(&ev)
	d.err = errors.New("Throttling")
	if _, err := s.SwitchTo(context.Background(), site.DR, Options{}); err == nil {
		t.Fatal("expected error")
	}
	if f.active != site.Both {
		t.Fatalf("flag = %q, want both", f.active)
	}
}

func TestNoOpWhenAlreadyActive(t *testing.T) {
	var ev events
	s, _, _ := newSwitcher(&ev)
	res, err := s.SwitchTo(context.Background(), site.Primary, Options{})
	if err != nil || !res.NoOp || len(ev) != 0 {
		t.Fatalf("res=%+v err=%v ev=%v", res, err, ev)
	}
}

func TestRepairsDNSDrift(t *testing.T) {
	// Flag already says dr but DNS still points at primary: only DNS changes.
	var ev events
	s, f, _ := newSwitcher(&ev)
	f.active = site.DR
	res, err := s.SwitchTo(context.Background(), site.DR, Options{Drain: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ev, ","); got != "check:dr-health,check:dr-ready,dns:dr" {
		t.Fatalf("events = %s", got)
	}
	if len(res.Commits) != 0 {
		t.Fatal("no commit expected")
	}
}

func TestFailbackWithoutDNS(t *testing.T) {
	var ev events
	s, f, _ := newSwitcher(&ev)
	s.DNS = nil
	f.active = site.DR
	if _, err := s.SwitchTo(context.Background(), site.Primary, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ev, ","); got != "check:primary-health,flag:both,flag:primary" {
		t.Fatalf("events = %s", got)
	}
}

func TestStatus(t *testing.T) {
	var ev events
	s, _, d := newSwitcher(&ev)
	s.Health[site.DR] = &fakeCheck{name: "dr-health", err: errors.New("timeout")}
	d.weights = map[site.Site]int64{site.Primary: 0, site.DR: 100}
	st, err := s.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.FlagActive != site.Primary || st.DNSActive != site.DR || st.Consistent {
		t.Fatalf("status = %+v", st)
	}
	if st.Health[site.Primary].Status != "healthy" || st.Health[site.DR].Status != "unhealthy" {
		t.Fatalf("health = %+v", st.Health)
	}
}

func TestStatusBothIsInconsistent(t *testing.T) {
	var ev events
	s, f, _ := newSwitcher(&ev)
	s.DNS = nil
	f.active = site.Both
	st, err := s.Status(context.Background())
	if err != nil || st.Consistent {
		t.Fatalf("st=%+v err=%v", st, err)
	}
}
