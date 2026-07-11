#!/usr/bin/env bash
# scripts/local-test.sh — spin up a local kind cluster, build & load the
# grepod image, install the Helm chart, and port-forward it so you can
# hit http://localhost:8080 immediately.
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-grepod}"
IMAGE_NAME="${IMAGE_NAME:-grepod:local}"
NAMESPACE="${NAMESPACE:-default}"
RELEASE_NAME="${RELEASE_NAME:-grepod}"

need() { command -v "$1" >/dev/null 2>&1 || { echo "error: '$1' is required but not installed" >&2; exit 1; }; }
need docker
need kind
need helm
need kubectl

echo "==> [1/6] Checking for existing kind cluster '${CLUSTER_NAME}'..."
if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  echo "    cluster already exists, reusing it."
else
  echo "    creating kind cluster '${CLUSTER_NAME}'..."
  kind create cluster --name "${CLUSTER_NAME}"
fi

echo "==> [2/6] Building Docker image ${IMAGE_NAME}..."
docker build -t "${IMAGE_NAME}" .

echo "==> [3/6] Loading image into kind cluster..."
kind load docker-image "${IMAGE_NAME}" --name "${CLUSTER_NAME}"

echo "==> [4/6] Installing/upgrading Helm release '${RELEASE_NAME}' in namespace '${NAMESPACE}'..."
helm upgrade --install "${RELEASE_NAME}" ./helm \
  --namespace "${NAMESPACE}" --create-namespace \
  --set image.repository="${IMAGE_NAME%%:*}" \
  --set image.tag="${IMAGE_NAME##*:}" \
  --set namespace="${NAMESPACE}"

echo "==> [5/6] Waiting for deployment to become ready..."
kubectl -n "${NAMESPACE}" rollout status deployment/"${RELEASE_NAME}" --timeout=120s

echo "==> [6/6] Port-forwarding svc/${RELEASE_NAME} 8080:80 (Ctrl+C to stop)..."
echo "    Once forwarding starts, open http://localhost:8080"
kubectl -n "${NAMESPACE}" port-forward "svc/${RELEASE_NAME}" 8080:80
