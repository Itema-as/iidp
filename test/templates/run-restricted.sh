#!/usr/bin/env bash
# Runs an image built from one of iidp's Create templates the way the
# Platform does, and checks that it passes Pod Security's restricted level:
# a kind cluster on the Kubernetes version the Platform runs, a namespace
# that enforces, warns and audits restricted, and the application chart
# rendered with runAsNonRoot: true, which iidp app create writes for these
# templates. It fails on any warning from the apply, on a Pod that does not
# become Ready, on a container running as root, and on anything but a 200
# on / at the template's port.
#
#   test/templates/run-restricted.sh <nextjs|vite-react> <image>
#
# The image must be fully qualified (for example
# iidp-templates.local/vite-react:ci) and built locally; it is loaded into
# the cluster, never pulled. Needs kind, kubectl, helm and docker (or
# podman, with KIND_EXPERIMENTAL_PROVIDER=podman). The Templates job in
# .github/workflows/ci.yaml runs it for both templates.
set -euo pipefail

framework=${1:?usage: run-restricted.sh <nextjs|vite-react> <image>}
image=${2:?usage: run-restricted.sh <nextjs|vite-react> <image>}
case "$framework" in
  nextjs) kind_of=web-service port=3000 ;;
  vite-react) kind_of=static-site port=8080 ;;
  *) echo "unknown framework $framework" >&2; exit 2 ;;
esac

root=$(cd "$(dirname "$0")/../.." && pwd)
cluster=${IIDP_TEMPLATES_CLUSTER:-iidp-templates}
node_image=$(sed -n 's/^  nodeImage: //p' "$root/bootstrap/versions.yaml")
engine=docker
if [ "${KIND_EXPERIMENTAL_PROVIDER:-}" = podman ]; then engine=podman; fi
work=$(mktemp -d)
kubeconfig="$work/kubeconfig"
export KUBECONFIG="$kubeconfig"

cleanup() {
  [ -n "${forward_pid:-}" ] && kill "$forward_pid" 2>/dev/null || true
  kind delete cluster --name "$cluster" --kubeconfig "$kubeconfig" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

echo "creating kind cluster $cluster from $node_image"
kind create cluster --name "$cluster" --image "$node_image" --kubeconfig "$kubeconfig" --wait 120s
"$engine" save -o "$work/image.tar" "$image"
kind load image-archive "$work/image.tar" --name "$cluster"

kubectl create namespace demo
kubectl label namespace demo \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/warn=restricted \
  pod-security.kubernetes.io/audit=restricted

repository=${image%:*}
tag=${image##*:}
cat >"$work/values.yaml" <<EOF
application:
  name: demoapp
platform:
  baseDomain: example.test
kind: $kind_of
image:
  repository: $repository
  tag: "$tag"
runAsNonRoot: true
EOF

helm template demoapp "$root/chart/application" --namespace demo --values "$work/values.yaml" >"$work/manifests.yaml"
# kubectl prints admission warnings (Pod Security's among them) on stderr,
# prefixed "Warning:".
kubectl apply --namespace demo -f "$work/manifests.yaml" 2>&1 | tee "$work/apply.log"
if grep -q '^Warning:' "$work/apply.log"; then
  echo "applying the chart for the $framework template raised warnings; it must pass restricted" >&2
  exit 1
fi

if ! kubectl --namespace demo rollout status deployment/demoapp --timeout=180s; then
  kubectl --namespace demo describe pods >&2 || true
  kubectl --namespace demo get events --sort-by=.lastTimestamp >&2 || true
  exit 1
fi

uid=$(kubectl --namespace demo exec deployment/demoapp -- id -u)
echo "the $framework container runs as uid $uid"
if [ "$uid" = 0 ]; then
  echo "the $framework container runs as root" >&2
  exit 1
fi

kubectl --namespace demo port-forward service/demoapp 18080:"$port" >"$work/forward.log" 2>&1 &
forward_pid=$!
status=000
for _ in $(seq 1 30); do
  status=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/ || true)
  [ "$status" = 200 ] && break
  sleep 1
done
if [ "$status" != 200 ]; then
  echo "GET / on the $framework template's port $port returned $status, want 200" >&2
  cat "$work/forward.log" >&2
  kubectl --namespace demo logs deployment/demoapp >&2 || true
  exit 1
fi
echo "the $framework template passes restricted and answers 200 on port $port"
