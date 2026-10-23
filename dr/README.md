# DR runbook: prod <-> prod-dr

This runbook moves the prod workloads between the primary site (`prod`,
`role=primary`) and the DR site (`prod-dr`, `role=dr`). The switch is the
`dr.activeSite` value in [`active-site.yaml`](active-site.yaml); the tool is
`drswitch` with [`drswitch.yaml`](drswitch.yaml).

| State of `activeSite` | Meaning |
|-----------------------|---------|
| `primary` | normal operation |
| `both` | a switch is in progress: both sites run full capacity |
| `dr` | failed over |

## Before you start

- [ ] An incident channel and an incident lead exist. One person runs drswitch.
- [ ] Your checkout is on `main` and up to date: `git pull --ff-only`.
- [ ] Credentials work:
  - Git push to `main` (or the protected-branch bypass your org uses for DR).
  - `kubectl --context prod-dr get --raw /readyz` (and `prod` for failback).
  - `aws sts get-caller-identity --profile <dr-profile>` if `route53.enabled`.
- [ ] Data tier is ready on the target side (database promoted / global
      table writable / queue replicated). drswitch does **not** do this.
- [ ] `drswitch status` shows the expected starting point.

```bash
drswitch status
# flag activeSite:  primary
# route53 active:   primary  (weights primary=100 dr=0)
# health dr:        healthy  kube[prod-dr]/readyz
# health primary:   healthy  kube[prod-primary]/readyz
```

## Decide: failover or not

Fail over when the primary site cannot meet its SLO and recovery in place is
not expected within the agreed RTO, for example: regional AWS service
outage, loss of the primary EKS control plane, or a broken primary that a
rollback does not fix. A bad application release is **not** a DR event; roll
it back instead, because the DR site runs the same release.

## Planned failover (game day, region maintenance)

```bash
drswitch failover --dry-run
drswitch failover --reason "game day 2026-Q4" --push
```

What happens (see the sequence diagram in the top-level README):

1. `prod-dr` platform probe must pass, or nothing changes (exit code 3).
2. Commit `activeSite: both`. ArgoCD scales `prod-dr` up. `prod` keeps serving.
3. drswitch polls the DR ready probe (up to `ready.timeout`).
4. Route53 weights move to `dr=100, primary=0` in one change batch.
5. Drain (`drain`, 60s by default) so cached DNS answers expire.
6. Commit `activeSite: dr`. ArgoCD scales `prod` down to standby.

## Unplanned failover (primary region impaired)

The same command. Differences to expect:

- ArgoCD cannot reach `prod`, so its Applications show `Unknown` or sync
  errors. That is expected and does not block anything.
- external-dns in the primary region may be unable to lower its own record's
  weight. Keep `route53.enabled: true` in `drswitch.yaml` (or pass the
  setting through a dedicated config) so drswitch sets both weights directly.
- If the DR platform probe itself is flaky because of the incident, verify
  manually and use `--skip-health-check` as a conscious decision; note it in
  `--reason`.

```bash
drswitch failover --reason "INC-1234 region A outage" --push --yes
```

When region A recovers, ArgoCD syncs `prod` to the current flag (`dr`), so it
comes back as the standby: zero replicas, paused KEDA scalers (no queue
consumption) and DNS weight 0. Do not fail back until the cause is
understood.

## Verify after a switch

```bash
drswitch status                                         # flag and DNS agree
kubectl --context prod-dr -n orders-api get deploy,hpa  # replicas up, HPA present
kubectl --context prod-dr -n events-worker get scaledobject \
  -o custom-columns=NAME:.metadata.name,PAUSED:.metadata.annotations.autoscaling\\.keda\\.sh/paused-replicas
dig +short orders.example.com                           # resolves to the DR load balancer
```

Then check the service dashboards for error rate and latency on the DR site.

## Failback

Failback is a planned operation; run it in a quiet period.

1. Confirm the data tier has been re-synced towards primary and is writable
   there.
2. `drswitch status` must show `health primary: healthy`.
3. Run:

```bash
drswitch failback --dry-run
drswitch failback --reason "INC-1234 failback" --push
```

The order is the same as failover: primary scales up while DR keeps serving,
DNS moves after primary is ready, DR scales down after the drain.

## Resuming after a partial failure

Every step is idempotent. Fix the cause and re-run the **same** command.

| Symptom | State left behind | Action |
|---------|-------------------|--------|
| exit code 3, "failed health check" | nothing changed | fix or investigate target; re-run |
| "is not ready; DNS was NOT changed" | `activeSite: both`, DNS unchanged | both sites run; check pods/events on the target; re-run |
| "DNS shift ... failed" | `activeSite: both`, DNS unchanged | check IAM / throttling; re-run |
| "drain interrupted" | `activeSite: both`, DNS moved | re-run; it continues with the final commit |
| push failed after commit | local commit exists | `git pull --rebase && git push`, then re-run |
| `status` says flag and DNS disagree | mixed | run failover/failback for the site you want; drswitch only changes what differs |

`activeSite: both` is always a safe place to stop: both sites run full
capacity and DNS points at whichever site it pointed at before.

## Manual fallback (drswitch unavailable)

1. Edit `dr/active-site.yaml`: set `activeSite: both`, commit, push.
2. Wait until the target's Deployments are available:
   `kubectl --context prod-dr -n orders-api rollout status deploy/orders-api`.
3. Move DNS. List the record sets, then UPSERT both with the new weights
   (copy each record exactly, change only `Weight`):

   ```bash
   aws route53 list-resource-record-sets --hosted-zone-id Z0000000000000EXAMPLE \
     --query "ResourceRecordSets[?Name=='orders.example.com.']"
   aws route53 change-resource-record-sets --hosted-zone-id Z0000000000000EXAMPLE \
     --change-batch file://change.json
   ```

4. Wait at least the record TTL.
5. Set `activeSite: dr`, commit, push.

## Running drswitch inside the DR cluster

For automation or when laptops cannot reach the clusters, run drswitch as a
Job in `prod-dr` with a config that uses `kube.inCluster: true` for the DR
probes, an HTTP URL for the primary probe, and IRSA / Pod Identity for
Route53. The Job needs the Git push credential (deploy key or app token)
mounted as a Secret, and an IAM role limited to:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["route53:ListResourceRecordSets", "route53:ChangeResourceRecordSets"],
      "Resource": "arn:aws:route53:::hostedzone/Z0000000000000EXAMPLE"
    }
  ]
}
```
