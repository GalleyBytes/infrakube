#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

CONTROLLER_IMAGE="${CONTROLLER_IMAGE:-ghcr.io/galleybytes/infrakube:e2e}"
TASK_IMAGE="${TASK_IMAGE:-ghcr.io/galleybytes/infrakube-task:e2e}"

dump_debug() {
  echo "Collecting kind smoke diagnostics..."
  kubectl get pods --all-namespaces -o wide || true
  kubectl get terraform --all-namespaces || true
  kubectl get tofu --all-namespaces || true
  kubectl -n infrakube-system describe deployment infrakube || true
  kubectl -n infrakube-system logs deployment/infrakube --all-containers=true || true
  kubectl -n tf-e2e get pods || true
  kubectl -n tofu-e2e get pods || true
  kubectl -n tf-e2e describe terraform terraform-inline || true
  kubectl -n tofu-e2e describe tofu tofu-inline || true
}

trap dump_debug ERR

wait_for_jsonpath() {
  local resource="$1"
  local jsonpath="$2"
  local expected="$3"
  local attempts="${4:-90}"

  local current=""
  for _ in $(seq 1 "$attempts"); do
    current="$(kubectl get ${resource} -o "jsonpath=${jsonpath}" 2>/dev/null || true)"
    if [[ "${current}" == "${expected}" ]]; then
      return 0
    fi
    sleep 5
  done

  echo "Timed out waiting for ${resource} ${jsonpath} to equal ${expected}, last value was '${current}'"
  return 1
}

echo "Installing infrakube into the current cluster..."
kubectl apply -f deploy/namespace.yaml
kubectl apply -f deploy/crds/
kubectl wait --for=condition=Established crd/terraforms.infrakube.galleybytes.com --timeout=120s
kubectl wait --for=condition=Established crd/tofus.infrakube.galleybytes.com --timeout=120s
kubectl apply -f deploy/serviceaccount.yaml
kubectl apply -f deploy/clusterrole.yaml
kubectl apply -f deploy/clusterrolebinding.yaml
kubectl apply -f deploy/pvc.yaml
kubectl apply -f deploy/service.yaml
kubectl apply -f deploy/deployment.yaml

kubectl -n infrakube-system patch deployment infrakube --type=json -p "$(printf '[{"op":"replace","path":"/spec/template/spec/containers/0/image","value":"%s"},{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"},{"op":"replace","path":"/spec/template/spec/containers/0/args","value":["--zap-log-level=debug","--zap-encoder=console","--auto-download=true","--tf-download-base-url=https://releases.hashicorp.com/terraform","--tofu-download-base-url=https://github.com/opentofu/opentofu/releases/download","--task-image=%s"]}]' "${CONTROLLER_IMAGE}" "${TASK_IMAGE}")"
kubectl -n infrakube-system rollout status deployment/infrakube --timeout=180s

echo "Applying smoke fixtures..."
kubectl apply -f test/e2e/manifests/terraform-inline.yaml
kubectl apply -f test/e2e/manifests/tofu-inline.yaml

echo "Waiting for Terraform workflow..."
wait_for_jsonpath "terraform -n tf-e2e terraform-inline" "{.status.phase}" "completed"
wait_for_jsonpath "terraform -n tf-e2e terraform-inline" "{.status.outputs.hello}" "terraform-smoke"

echo "Waiting for Tofu workflow..."
wait_for_jsonpath "tofu -n tofu-e2e tofu-inline" "{.status.phase}" "completed"
wait_for_jsonpath "tofu -n tofu-e2e tofu-inline" "{.status.outputs.hello}" "tofu-smoke"

echo "Kind smoke test passed."
