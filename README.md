# Infrakube

A Kubernetes controller for running Terraform and other Infrastructure as Code workflows.

Infrakube is the successor to [terraform-operator](https://github.com/galleybytes/terraform-operator). It uses the same `kind: Terraform` resource and keeps familiar spec fields like `terraformVersion`, `terraformModule`, and `images.terraform`, making migration straightforward. The main change is the API group, which moves from `tf.galleybytes.com` to `infrakube.galleybytes.com`.

## Features

- Runs Terraform `init`, `plan`, and `apply` as Kubernetes pods
- Supports all Terraform versions from 0.12 through 1.14 out of the box
- Module downloads via git, https, and other sources supported by go-getter
- Kubernetes, S3, GCS, and other Terraform backends
- AWS IRSA, GCP Workload Identity, and other cloud auth methods
- Task plugins for custom pre/post workflows
- Monitor plugins for notifications and observability

## Quick Start

```yaml
apiVersion: infrakube.galleybytes.com/v1
kind: Terraform
metadata:
  name: my-infra
spec:
  terraformVersion: "1.5.7"
  terraformModule:
    source: https://github.com/example/module.git?ref=main
  backend: |-
    terraform {
      backend "kubernetes" {
        secret_suffix     = "my-infra"
        in_cluster_config = true
      }
    }
```

## Testing

Infrakube is tested at three levels: focused unit tests for risky logic, envtest-based controller tests for reconcile behavior, and a kind smoke test that runs real Terraform and Tofu workflows on Kubernetes.

| Layer | Command | What it proves |
| --- | --- | --- |
| Unit | `make test-unit` | Custom JSON behavior, task option resolution, pod/job manifest generation, and other high-risk logic that is easy to regress. |
| Controller integration | `make test-integration` | Terraform and Tofu reconcilers create the expected Kubernetes resources in envtest and wire status/stage state correctly. |
| Kind smoke | `make test-e2e` | A real controller plus real task pods can complete tiny Terraform and Tofu workflows on Kubernetes without paid infrastructure. |

The kind fixtures live under `test/e2e/manifests/` and use inline modules plus the Kubernetes backend so the default CI path stays free and secret-free.

## CI

Pull requests targeting `master` trigger three workflows. The controller build publishes `ghcr.io/galleybytes/infrakube:0.0.0-<commit>` and reuses a cached content tag when the controller inputs have not changed. The task build publishes `ghcr.io/galleybytes/infrakube-task:0.0.0-<commit>`. `.github/workflows/ci.yaml` runs `make test`, waits for those two build workflows to finish for the pull request head SHA, and then runs a kind-based Terraform and Tofu smoke test against those exact images.

Release image workflows are tag-driven. `infrakube-*` tags publish the controller image as `latest`, the requested version tag, and an immutable `0.0.0-<commit>` tag. `task-*` tags publish the task image as the requested version tag and an immutable `0.0.0-<commit>` tag. Pushes to `master` do not rerun the full validation pipeline or release builds.

## Support expectations

The automated suite is meant to prove controller behavior, task orchestration, and basic workflow execution. It does not try to cover every provider or cloud-specific module in the default CI path. If stronger proof is needed later, sandbox cloud smoke tests can live in separate nightly or manually triggered workflows.

For local test workflow details, see [`docs/testing.md`](docs/testing.md).

## Community

Join the channel: https://discord.gg/J5vRmT2PWg
