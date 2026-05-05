# Development

Just a little bit of guidance on how this project is being developed and tested.

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

