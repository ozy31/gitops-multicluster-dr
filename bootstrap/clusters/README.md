# Cluster secrets

ArgoCD discovers clusters from Secrets labelled
`argocd.argoproj.io/secret-type: cluster`. The ApplicationSets select
clusters purely by the labels on these Secrets:

| Label | Values | Used by |
|-------|--------|---------|
| `env` | `dev`, `stage`, `prod` (`local` for kind) | value file `values-<env>.yaml` |
| `role` | `primary`, `dr` | value file `values-<env>-<role>.yaml`, compared with `dr.activeSite` |
| `region` | AWS region | passed to charts as `site.region` |
| `gitops.example.com/workloads` | `"true"` | opt in to `applicationsets/apps.yaml` |
| `addons.example.com/<addon>` | `enabled` | opt in to one addon from `platform/addons/` |

The files here are **examples with placeholder values**. They show the EKS
IAM authentication shape (`awsAuthConfig`), in which the ArgoCD controller's
IAM role assumes a role mapped into each cluster via an EKS access entry. No
long-lived credentials are stored.

In a real setup create these Secrets from Terraform (or External Secrets)
alongside the clusters, not from this repository. They are deliberately not
part of the root app.
