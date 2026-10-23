// Package dns shifts traffic between sites by rewriting the weights of a
// Route53 weighted record pair.
package dns

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"

	"github.com/ozy31/gitops-multicluster-dr/internal/site"
)

// Route53API is the subset of the Route53 client used here; *route53.Client
// satisfies it and tests use a fake.
type Route53API interface {
	ListResourceRecordSets(ctx context.Context, in *route53.ListResourceRecordSetsInput, optFns ...func(*route53.Options)) (*route53.ListResourceRecordSetsOutput, error)
	ChangeResourceRecordSets(ctx context.Context, in *route53.ChangeResourceRecordSetsInput, optFns ...func(*route53.Options)) (*route53.ChangeResourceRecordSetsOutput, error)
}

var _ Route53API = (*route53.Client)(nil)

// WeightedRecords manages one weighted record per site under the same name.
// The records themselves (targets, alias config, health checks) are owned
// by infrastructure code; drswitch only ever changes their Weight.
type WeightedRecords struct {
	Client        Route53API
	HostedZoneID  string
	Name          string
	Type          types.RRType
	SetIDs        map[site.Site]string
	ActiveWeight  int64
	StandbyWeight int64
}

// Weights returns the current weight per site.
func (w *WeightedRecords) Weights(ctx context.Context) (map[site.Site]int64, error) {
	sets, err := w.records(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[site.Site]int64, len(sets))
	for s, rr := range sets {
		out[s] = aws.ToInt64(rr.Weight)
	}
	return out, nil
}

// Shift makes active the only site with a non-standby weight. Both records
// are UPSERTed in one change batch, which Route53 applies atomically.
func (w *WeightedRecords) Shift(ctx context.Context, active site.Site, comment string) (string, error) {
	sets, err := w.records(ctx)
	if err != nil {
		return "", err
	}
	changes := make([]types.Change, 0, len(sets))
	for _, s := range site.All() {
		rr := sets[s]
		weight := w.StandbyWeight
		if s == active {
			weight = w.ActiveWeight
		}
		rr.Weight = aws.Int64(weight)
		changes = append(changes, types.Change{Action: types.ChangeActionUpsert, ResourceRecordSet: &rr})
	}
	out, err := w.Client.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(w.HostedZoneID),
		ChangeBatch:  &types.ChangeBatch{Comment: aws.String(comment), Changes: changes},
	})
	if err != nil {
		return "", fmt.Errorf("route53 change: %w", err)
	}
	if out.ChangeInfo == nil {
		return "", errors.New("route53 change: empty ChangeInfo")
	}
	return aws.ToString(out.ChangeInfo.Id), nil
}

// ActiveFrom derives the active site from a weight map. It returns "" when
// the weights do not describe a clean active/standby split.
func ActiveFrom(weights map[site.Site]int64) site.Site {
	p, d := weights[site.Primary], weights[site.DR]
	switch {
	case p > 0 && d == 0:
		return site.Primary
	case d > 0 && p == 0:
		return site.DR
	default:
		return ""
	}
}

func (w *WeightedRecords) records(ctx context.Context) (map[site.Site]types.ResourceRecordSet, error) {
	name := fqdn(w.Name)
	bySetID := make(map[string]site.Site, len(w.SetIDs))
	for s, id := range w.SetIDs {
		bySetID[id] = s
	}
	found := make(map[site.Site]types.ResourceRecordSet, 2)

	in := &route53.ListResourceRecordSetsInput{
		HostedZoneId:    aws.String(w.HostedZoneID),
		StartRecordName: aws.String(name),
		StartRecordType: w.Type,
	}
	for {
		out, err := w.Client.ListResourceRecordSets(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("route53 list: %w", err)
		}
		done := false
		for _, rr := range out.ResourceRecordSets {
			// Results are sorted by name then type; stop once past ours.
			if !strings.EqualFold(fqdn(aws.ToString(rr.Name)), name) || rr.Type != w.Type {
				done = true
				break
			}
			s, ok := bySetID[aws.ToString(rr.SetIdentifier)]
			if !ok {
				continue
			}
			if rr.Weight == nil {
				return nil, fmt.Errorf("record %s (%s) is not a weighted record", name, aws.ToString(rr.SetIdentifier))
			}
			found[s] = rr
		}
		if done || !out.IsTruncated {
			break
		}
		in.StartRecordName = out.NextRecordName
		in.StartRecordType = out.NextRecordType
		in.StartRecordIdentifier = out.NextRecordIdentifier
	}

	var missing []string
	for _, s := range site.All() {
		if _, ok := found[s]; !ok {
			missing = append(missing, fmt.Sprintf("%s (set identifier %q)", s, w.SetIDs[s]))
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("weighted record %s %s not found for: %s", name, w.Type, strings.Join(missing, ", "))
	}
	return found, nil
}

func fqdn(name string) string {
	name = strings.ToLower(name)
	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	return name
}
