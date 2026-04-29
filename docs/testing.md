# Testing Infrakube

This guide explains what the automated test suite covers and how to run it locally.

## What the suite covers

| Layer | Command | Primary goal |
| --- | --- | --- |
| Unit | `make test-unit` | Cover high-risk behavior such as custom JSON marshaling, task option merging, image/default precedence, pod env generation, and guardrails around invalid task pod configuration. |
| Controller integration | `make test-integration` | Run the reconcilers against envtest and assert that Terraform and Tofu resources create the expected PVCs, secrets, configmaps, RBAC, service accounts, and pods. |
| Kind smoke | `make test-e2e` | Run the controller and task image on a real Kubernetes cluster and verify a Terraform workflow and a Tofu workflow both finish successfully. |

The default suite avoids paid infrastructure and long-lived secrets. It focuses on controller behavior and a small number of real workflow executions instead of trying to cover every provider combination.

## Local prerequisites

- Go matching `go.mod`
- Docker
- A Kubernetes cluster for `make test-e2e`, typically kind
- `kubectl` configured to point at the cluster you want to use for the smoke test

`make test-unit` and `make test-integration` do not require a live cluster. The integration target downloads envtest assets into `./testbin`.

## Common commands

```bash
make test-unit
make test-integration
make test
make test-e2e
```

`make test` is the main validation entrypoint and runs both the unit and envtest integration suites.

If you are changing controller logic, `make test` is the minimum useful check. If you are changing deploy behavior, task images, or workflow execution, run `make test-e2e` against a kind cluster as well.

## End-to-end smoke fixtures

The smoke harness lives in `test/e2e/kind-smoke.sh`. The Terraform and Tofu fixtures live in `test/e2e/manifests/terraform-inline.yaml` and `test/e2e/manifests/tofu-inline.yaml`.

Those fixtures use:

- inline modules, so the tests do not depend on an external example repository
- the Kubernetes backend, so state handling is exercised inside the cluster
- status outputs, so success is easy to assert from CI

## CI behavior

`.github/workflows/ci.yaml` runs two jobs:

1. `unit-and-integration` runs `make test` and fails if generated artifacts drift.
2. `kind-smoke` builds the controller image and task image locally, loads them into kind, installs Infrakube, and runs the Terraform and Tofu smoke fixtures.

This keeps the default CI path free, secret-free, and easy to inspect.

## What is not covered by default

The default suite does not prove every provider plugin, cloud IAM path, or external backend permutation. Those are better fits for optional nightly or manually triggered sandbox workflows.
