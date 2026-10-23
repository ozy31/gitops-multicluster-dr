package dns

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"

	"github.com/ozy31/gitops-multicluster-dr/internal/site"
)

type fakeRoute53 struct {
	pages   [][]types.ResourceRecordSet
	calls   int
	changes []*route53.ChangeResourceRecordSetsInput
	err     error
}

func (f *fakeRoute53) ListResourceRecordSets(_ context.Context, in *route53.ListResourceRecordSetsInput, _ ...func(*route53.Options)) (*route53.ListResourceRecordSetsOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	page := f.calls
	f.calls++
	out := &route53.ListResourceRecordSetsOutput{ResourceRecordSets: f.pages[page]}
	if page+1 < len(f.pages) {
		out.IsTruncated = true
		next := f.pages[page+1][0]
		out.NextRecordName, out.NextRecordType, out.NextRecordIdentifier = next.Name, next.Type, next.SetIdentifier
	}
	return out, nil
}

func (f *fakeRoute53) ChangeResourceRecordSets(_ context.Context, in *route53.ChangeResourceRecordSetsInput, _ ...func(*route53.Options)) (*route53.ChangeResourceRecordSetsOutput, error) {
	f.changes = append(f.changes, in)
	return &route53.ChangeResourceRecordSetsOutput{ChangeInfo: &types.ChangeInfo{Id: aws.String("/change/C123")}}, nil
}

func rr(name, setID string, weight int64, target string) types.ResourceRecordSet {
	return types.ResourceRecordSet{
		Name:            aws.String(name),
		Type:            types.RRTypeCname,
		SetIdentifier:   aws.String(setID),
		Weight:          aws.Int64(weight),
		TTL:             aws.Int64(60),
		ResourceRecords: []types.ResourceRecord{{Value: aws.String(target)}},
	}
}

func newRecords(api Route53API) *WeightedRecords {
	return &WeightedRecords{
		Client:       api,
		HostedZoneID: "Z123",
		Name:         "App.Example.com",
		Type:         types.RRTypeCname,
		SetIDs:       map[site.Site]string{site.Primary: "primary", site.DR: "dr"},
		ActiveWeight: 100,
	}
}

func TestWeightsAcrossPages(t *testing.T) {
	api := &fakeRoute53{pages: [][]types.ResourceRecordSet{
		{rr("app.example.com.", "canary", 0, "c"), rr("app.example.com.", "dr", 0, "lb-dr")},
		{rr("app.example.com.", "primary", 100, "lb-primary"), rr("zzz.example.com.", "x", 1, "x")},
	}}
	got, err := newRecords(api).Weights(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got[site.Primary] != 100 || got[site.DR] != 0 {
		t.Fatalf("weights = %v", got)
	}
	if ActiveFrom(got) != site.Primary {
		t.Fatalf("ActiveFrom = %q", ActiveFrom(got))
	}
}

func TestShiftUpsertsBothRecordsAtomically(t *testing.T) {
	api := &fakeRoute53{pages: [][]types.ResourceRecordSet{{
		rr("app.example.com.", "dr", 0, "lb-dr"),
		rr("app.example.com.", "primary", 100, "lb-primary"),
	}}}
	id, err := newRecords(api).Shift(context.Background(), site.DR, "failover")
	if err != nil {
		t.Fatal(err)
	}
	if id != "/change/C123" {
		t.Fatalf("change id = %q", id)
	}
	if len(api.changes) != 1 {
		t.Fatalf("expected exactly one change batch, got %d", len(api.changes))
	}
	weights := map[string]int64{}
	for _, c := range api.changes[0].ChangeBatch.Changes {
		if c.Action != types.ChangeActionUpsert {
			t.Errorf("action = %s", c.Action)
		}
		weights[aws.ToString(c.ResourceRecordSet.SetIdentifier)] = aws.ToInt64(c.ResourceRecordSet.Weight)
		// Targets and TTL must be carried over untouched.
		if aws.ToInt64(c.ResourceRecordSet.TTL) != 60 || len(c.ResourceRecordSet.ResourceRecords) != 1 {
			t.Errorf("record body was not preserved: %+v", c.ResourceRecordSet)
		}
	}
	if weights["dr"] != 100 || weights["primary"] != 0 {
		t.Fatalf("weights = %v", weights)
	}
}

func TestMissingRecordIsAnError(t *testing.T) {
	api := &fakeRoute53{pages: [][]types.ResourceRecordSet{{rr("app.example.com.", "primary", 100, "lb")}}}
	_, err := newRecords(api).Weights(context.Background())
	if err == nil || !strings.Contains(err.Error(), `dr (set identifier "dr")`) {
		t.Fatalf("err = %v", err)
	}
}

func TestNonWeightedRecordIsAnError(t *testing.T) {
	r := rr("app.example.com.", "primary", 0, "lb")
	r.Weight = nil
	api := &fakeRoute53{pages: [][]types.ResourceRecordSet{{r}}}
	if _, err := newRecords(api).Weights(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestListError(t *testing.T) {
	api := &fakeRoute53{err: errors.New("AccessDenied")}
	if _, err := newRecords(api).Shift(context.Background(), site.DR, ""); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("err = %v", err)
	}
}

func TestActiveFrom(t *testing.T) {
	cases := []struct {
		p, d int64
		want site.Site
	}{
		{100, 0, site.Primary},
		{0, 100, site.DR},
		{50, 50, ""},
		{0, 0, ""},
	}
	for _, c := range cases {
		if got := ActiveFrom(map[site.Site]int64{site.Primary: c.p, site.DR: c.d}); got != c.want {
			t.Errorf("ActiveFrom(%d,%d) = %q, want %q", c.p, c.d, got, c.want)
		}
	}
}
