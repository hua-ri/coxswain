#!/bin/sh
# Installs Argo CD (with Dex for browser login) and Argo Workflows
# (argo-server in client auth mode) in the kind cluster cox-test, with
# demo apps and workflows, for trying Coxswain's argocd_* and argowf_*.
#
# It only touches the cluster cox-test, always through its own kubeconfig
# (never ~/.kube/config or $KUBECONFIG). Images are loaded from the local
# Docker (kind load), so set the *_IMAGE variables to mirrors you can pull.
#
#   scripts/kind-argo.sh up     install and port-forward (8080 Argo CD, 2746 Argo Workflows)
#   scripts/kind-argo.sh token  print a 1-hour token of the least-privilege ServiceAccount
#   scripts/kind-argo.sh down   remove what up installed
set -eu

CLUSTER=cox-test
KC=${COX_KIND_KUBECONFIG:-$HOME/.coxswain-test/kind/kubeconfig}
ARGOCD_IMAGE=${ARGOCD_IMAGE:-quay.io/argoproj/argocd:v3.2.0}
DEX_IMAGE=${DEX_IMAGE:-ghcr.io/dexidp/dex:v2.43.0}
REDIS_IMAGE=${REDIS_IMAGE:-public.ecr.aws/docker/library/redis:8.2.2-alpine}
ARGOCLI_IMAGE=${ARGOCLI_IMAGE:-quay.io/argoproj/argocli:v3.7.3}
CONTROLLER_IMAGE=${CONTROLLER_IMAGE:-quay.io/argoproj/workflow-controller:v3.7.3}
EXEC_IMAGE=${EXEC_IMAGE:-quay.io/argoproj/argoexec:v3.7.3}
BUSYBOX_IMAGE=${BUSYBOX_IMAGE:-busybox:1.36}
DIR=$(mktemp -d)
trap 'rm -rf "$DIR"' EXIT

k() { kubectl --kubeconfig "$KC" --context kind-$CLUSTER "$@"; }

check() {
  kind get clusters | grep -qx "$CLUSTER" || { echo "kind cluster $CLUSTER not found" >&2; exit 1; }
  [ -f "$KC" ] || { echo "no kubeconfig at $KC" >&2; exit 1; }
}

# load tags a local image as the name the manifests use and loads it into the nodes.
load() {
  src=$1 dst=$2
  docker image inspect "$src" >/dev/null 2>&1 || docker pull -q "$src"
  [ "$src" = "$dst" ] || docker tag "$src" "$dst"
  # docker save | ctr import, as kind load fails with some Docker image stores.
  for node in $(kind get nodes --name "$CLUSTER"); do
    docker exec "$node" ctr -n k8s.io images ls -q | grep -qx "$dst" && continue
    docker save "$dst" | docker exec -i "$node" ctr -n k8s.io images import --all-platforms - >/dev/null
  done
}

up() {
  check
  load "$ARGOCD_IMAGE" quay.io/argoproj/argocd:v3.2.0
  load "$DEX_IMAGE" ghcr.io/dexidp/dex:v2.43.0
  load "$REDIS_IMAGE" public.ecr.aws/docker/library/redis:8.2.2-alpine
  load "$ARGOCLI_IMAGE" quay.io/argoproj/argocli:v3.7.3
  load "$CONTROLLER_IMAGE" quay.io/argoproj/workflow-controller:v3.7.3
  load "$EXEC_IMAGE" quay.io/argoproj/argoexec:v3.7.3
  load "$BUSYBOX_IMAGE" busybox:1.36

  # Argo CD, with an admin password of our choosing and Dex's mock connector
  # (a browser login that needs no password) for trying SSO.
  k create namespace argocd --dry-run=client -o yaml | k apply -f -
  curl -fsSL https://raw.githubusercontent.com/argoproj/argo-cd/v3.2.0/manifests/install.yaml -o "$DIR/argocd.yaml"
  k apply -n argocd --server-side --force-conflicts -f "$DIR/argocd.yaml" >/dev/null
  HASH=$(htpasswd -nbBC 10 "" cox-admin-pw | tr -d ':\n' | sed 's/$2y/$2a/')
  k -n argocd patch secret argocd-secret --type merge -p "{\"stringData\":{\"admin.password\":\"$HASH\",\"admin.passwordMtime\":\"$(date -u +%FT%TZ)\"}}"
  k -n argocd patch configmap argocd-cm --type merge -p '{"data":{
    "url":"http://localhost:8080",
    "dex.config":"connectors:\n- type: mockCallback\n  id: mock\n  name: Mock SSO\n"}}'
  k -n argocd patch configmap argocd-rbac-cm --type merge -p '{"data":{"policy.default":"role:admin"}}'
  k -n argocd patch configmap argocd-cmd-params-cm --type merge -p '{"data":{"server.insecure":"true"}}'
  # Use the images loaded above, never pull (the manifests say Always for Dex).
  for d in argocd-dex-server argocd-server argocd-repo-server argocd-redis; do
    k -n argocd patch deploy $d --type json -p '[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' >/dev/null
  done
  k -n argocd patch deploy argocd-dex-server --type json -p '[{"op":"replace","path":"/spec/template/spec/initContainers/0/imagePullPolicy","value":"IfNotPresent"}]' >/dev/null
  k -n argocd rollout restart deploy argocd-server argocd-dex-server >/dev/null
  k -n argocd rollout status deploy argocd-server --timeout=300s

  # Argo Workflows: argo-server in client mode, a ServiceAccount allowed to work in "ci".
  k create namespace argo --dry-run=client -o yaml | k apply -f -
  k create namespace ci --dry-run=client -o yaml | k apply -f -
  curl -fsSL https://github.com/argoproj/argo-workflows/releases/download/v3.7.3/install.yaml -o "$DIR/argo.yaml"
  k apply -n argo --server-side --force-conflicts -f "$DIR/argo.yaml" >/dev/null
  k -n argo patch deploy argo-server --type json -p '[{"op":"replace","path":"/spec/template/spec/containers/0/args","value":["server","--auth-mode=client","--secure=false"]},
    {"op":"replace","path":"/spec/template/spec/containers/0/readinessProbe/httpGet/scheme","value":"HTTP"}]'
  k -n argo rollout status deploy argo-server --timeout=300s
  k apply -f - <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: {name: cox-ops, namespace: ci}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: cox-ops, namespace: ci}
rules:
- apiGroups: [argoproj.io]
  resources: [workflows, workflowtemplates, cronworkflows, workflowtaskresults]
  verbs: [get, list, watch, create, update, patch, delete]
- apiGroups: [""]
  resources: [pods, pods/log, events]
  verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: cox-ops, namespace: ci}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: cox-ops}
subjects: [{kind: ServiceAccount, name: cox-ops, namespace: ci}]
---
# The workflows' own pods run as "default" in ci and report their results.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: executor, namespace: ci}
rules:
- apiGroups: [argoproj.io]
  resources: [workflowtaskresults]
  verbs: [create, patch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: executor, namespace: ci}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: executor}
subjects: [{kind: ServiceAccount, name: default, namespace: ci}]
YAML
  demo
  forward
}

demo() {
  # Argo CD apps: "guestbook-ok" healthy, "guestbook-drift" changed by hand,
  # "broken" whose container crashes. Sources are inline (no Git needed):
  # a ConfigMap-backed plugin is too much here, so the apps point at the
  # public example repo when reachable; otherwise use your own.
  k apply -n argocd -f - <<YAML
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: guestbook, namespace: argocd}
spec:
  project: default
  source: {repoURL: https://github.com/argoproj/argocd-example-apps.git, path: guestbook, targetRevision: HEAD}
  destination: {server: https://kubernetes.default.svc, namespace: guestbook}
  syncPolicy: {syncOptions: [CreateNamespace=true]}
YAML
  # Workflows: a template with parameters, a success, a failure (exit 1) and an OOM.
  k apply -n ci -f - <<YAML
apiVersion: argoproj.io/v1alpha1
kind: WorkflowTemplate
metadata:
  name: build
  annotations: {workflows.argoproj.io/description: "Pretend to build a branch"}
spec:
  entrypoint: main
  arguments:
    parameters:
    - name: branch
    - {name: env, value: staging, enum: [staging, prod]}
    - {name: fail, value: "no", enum: ["no", "yes"]}
  templates:
  - name: main
    steps:
    - - {name: checkout, template: run, arguments: {parameters: [{name: cmd, value: "echo cloning {{workflow.parameters.branch}}"}]}}
    - - {name: compile, template: run, arguments: {parameters: [{name: cmd, value: "echo compiling; sleep 3; [ {{workflow.parameters.fail}} = no ] || { echo 'error: undefined symbol checkout.Pay'; exit 2; }"}]}}
      - {name: lint, template: run, arguments: {parameters: [{name: cmd, value: "echo lint ok"}]}}
  - name: run
    inputs: {parameters: [{name: cmd}]}
    container: {image: busybox:1.36, command: [sh, -c], args: ["{{inputs.parameters.cmd}}"]}
YAML
  k create -n ci -f - <<YAML
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata: {generateName: oom-}
spec:
  entrypoint: eat
  templates:
  - name: eat
    container:
      image: busybox:1.36
      command: [sh, -c, "echo allocating; head -c 200m /dev/zero | tail"]
      resources: {limits: {memory: 32Mi}}
YAML
  for args in "-p branch=main" "-p branch=feat/pay -p fail=yes"; do
    k create -n ci -f - <<YAML
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata: {generateName: build-}
spec:
  workflowTemplateRef: {name: build}
  arguments:
    parameters:
$(for p in $args; do [ "$p" = -p ] && continue; echo "    - {name: ${p%%=*}, value: \"${p#*=}\"}"; done)
YAML
  done
}

forward() {
  pkill -f "port-forward.*svc/argocd-server" 2>/dev/null || true
  pkill -f "port-forward.*svc/argo-server" 2>/dev/null || true
  nohup kubectl --kubeconfig "$KC" --context kind-$CLUSTER -n argocd port-forward svc/argocd-server 8080:80 >/tmp/cox-argocd-pf.log 2>&1 &
  nohup kubectl --kubeconfig "$KC" --context kind-$CLUSTER -n argo port-forward svc/argo-server 2746:2746 >/tmp/cox-argo-pf.log 2>&1 &
  sleep 2
  echo "Argo CD:        http://localhost:8080  (admin / cox-admin-pw; browser login: Dex's mock SSO, no password)"
  echo "Argo Workflows: http://localhost:2746  (namespace ci; token: scripts/kind-argo.sh token)"
}

token() {
  check
  k -n ci create token cox-ops --duration 1h
}

down() {
  check
  pkill -f "port-forward.*svc/argocd-server" 2>/dev/null || true
  pkill -f "port-forward.*svc/argo-server" 2>/dev/null || true
  k delete namespace argocd argo ci guestbook --ignore-not-found --wait=false
}

case "${1:-}" in
up) up ;;
demo) check; demo ;;
forward) check; forward ;;
token) token ;;
down) down ;;
*) echo "usage: $0 up|demo|forward|token|down" >&2; exit 2 ;;
esac
