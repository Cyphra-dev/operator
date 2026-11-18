#!/usr/bin/env bash
# End-to-end test in a throwaway kind cluster:
#   operator (Helm) → Neo4jInstance (plain + TLS) → data written through a client → on-demand
#   backup to an S3 server (rclone serve s3) → data still there after the instance restarts.
#
# Env: CLUSTER (default cyphra-e2e), KEEP=1 keeps the cluster, NEO4J_VERSION (default 5.26),
#      CLIENT_CMD: optional command run with NEO4J_URI/USER/PASSWORD exported to write test data
#                  (CI uses the Cyphra ORM example); defaults to cypher-shell inside the pod.
set -euo pipefail
cd "$(dirname "$0")/../.."

CLUSTER="${CLUSTER:-cyphra-e2e}"
NS=e2e
TAG="e2e-$(date +%s)" # unique per run so Helm rolls the operator when the cluster is reused
IMG="ghcr.io/cyphra-dev/operator:$TAG"
NEO4J_VERSION="${NEO4J_VERSION:-5.26}"
RCLONE_IMG=$(grep -o 'RcloneImage = "[^"]*"' internal/controller/resources.go | cut -d'"' -f2)
WORK=$(mktemp -d)

log() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
cleanup() {
  if [ "${KEEP:-0}" != 1 ]; then kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
  if [ -n "${PF_PID:-}" ]; then kill "$PF_PID" 2>/dev/null || true; fi
}
trap cleanup EXIT
fail() {
  echo "FAIL: $*" >&2
  kubectl -n "$NS" get neo4jinstances,pods,jobs -o wide || true
  kubectl -n cyphra-system logs deploy/cyphra-operator --tail=80 || true
  exit 1
}

log "kind cluster $CLUSTER"
if ! kind get clusters | grep -qx "$CLUSTER"; then
  # Node boot can miss kind's deadline on a loaded machine; retry once.
  kind create cluster --name "$CLUSTER" --wait 120s ||
    { kind delete cluster --name "$CLUSTER" >/dev/null 2>&1; kind create cluster --name "$CLUSTER" --wait 120s; }
fi
kubectl config use-context "kind-$CLUSTER" >/dev/null

log "images"
docker build -q -t "$IMG" . >/dev/null
# Only the locally built operator image is loaded; neo4j and rclone are pulled by the cluster
# (kind cannot load some multi-platform images).
kind load docker-image --name "$CLUSTER" "$IMG" >/dev/null

log "operator"
helm upgrade --install cyphra-operator charts/cyphra-operator -n cyphra-system --create-namespace \
  --set image.tag="$TAG" --set image.pullPolicy=Never --set logLevel=debug --wait --timeout 3m

log "S3 server (rclone serve s3) and credentials"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$NS" create secret generic s3 --from-literal=accessKeyId=e2e-access \
  --from-literal=secretAccessKey=e2e-secret-key --dry-run=client -o yaml | kubectl apply -f -
sed "s|RCLONE_IMG|$RCLONE_IMG|" test/e2e/s3.yaml | kubectl -n "$NS" apply -f -
kubectl -n "$NS" rollout status deploy/s3 --timeout=120s

log "TLS secret (self-signed)"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=secure.$NS.svc" \
  -addext "subjectAltName=DNS:secure.$NS.svc,DNS:localhost" \
  -keyout "$WORK/tls.key" -out "$WORK/tls.crt" 2>/dev/null
kubectl -n "$NS" create secret tls secure-tls --cert="$WORK/tls.crt" --key="$WORK/tls.key" \
  --dry-run=client -o yaml | kubectl apply -f -

log "Neo4jInstances"
sed "s|NEO4J_VERSION|$NEO4J_VERSION|" test/e2e/instances.yaml | kubectl -n "$NS" apply -f -
for i in blog secure; do
  kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready "neo4jinstance/$i" --timeout=300s \
    || fail "$i did not become Ready"
done
kubectl -n "$NS" get neo4jinstances

PASSWORD=$(kubectl -n "$NS" get secret blog-auth -o jsonpath='{.data.password}' | base64 -d)
cypher() { kubectl -n "$NS" exec "$1-0" -c neo4j -- cypher-shell -a "$2" -u neo4j -p "$3" --format plain "$4"; }

log "TLS: Bolt requires encryption"
SECURE_PW=$(kubectl -n "$NS" get secret secure-auth -o jsonpath='{.data.password}' | base64 -d)
cypher secure "neo4j+ssc://localhost:7687" "$SECURE_PW" "RETURN 1 AS ok" | grep -q 1 || fail "TLS connection failed"
if cypher secure "neo4j://localhost:7687" "$SECURE_PW" "RETURN 1" >/dev/null 2>&1; then
  fail "plaintext Bolt was accepted on a TLS instance"
fi

log "write data"
if [ -n "${CLIENT_CMD:-}" ]; then
  kubectl -n "$NS" port-forward svc/blog 17687:7687 >/dev/null 2>&1 &
  PF_PID=$!
  sleep 3
  NEO4J_URI=bolt://127.0.0.1:17687 NEO4J_USER=neo4j NEO4J_PASSWORD="$PASSWORD" bash -c "$CLIENT_CMD"
  kill "$PF_PID"
  PF_PID=
else
  cypher blog "neo4j://localhost:7687" "$PASSWORD" \
    "CREATE (:User {email:'alice@example.com'})-[:AUTHORED]->(:Post {title:'Hello'})"
fi
BEFORE=$(cypher blog "neo4j://localhost:7687" "$PASSWORD" "MATCH (n) RETURN count(n) AS c" | tail -1)
[ "$BEFORE" -gt 0 ] || fail "no data written"
echo "nodes before backup: $BEFORE"

log "on-demand backup"
kubectl -n "$NS" annotate neo4jinstance blog cyphra.dev/backup-requested-at="$(date -u +%FT%TZ)" --overwrite
KEY=
for _ in $(seq 1 90); do
  KEY=$(kubectl -n "$NS" get neo4jinstance blog -o jsonpath='{.status.backup.lastSuccessfulKey}')
  [ -n "$KEY" ] && break
  FAILED=$(kubectl -n "$NS" get neo4jinstance blog -o jsonpath='{.status.conditions[?(@.type=="BackupFailed")].status}')
  if [ "$FAILED" = True ]; then
    fail "backup failed: $(kubectl -n "$NS" logs -l app.kubernetes.io/component=backup --all-containers --tail=50)"
  fi
  sleep 5
done
[ -n "$KEY" ] || fail "backup did not finish"
echo "backup key: $KEY"
SIZE=$(kubectl -n "$NS" exec deploy/s3 -- stat -c %s "/data/dumps/$KEY") || fail "dump not found in the bucket"
echo "dump size: $SIZE bytes"
[ "$SIZE" -gt 0 ] || fail "empty dump"

log "instance restarted with its data"
kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready neo4jinstance/blog --timeout=300s \
  || fail "blog did not come back after the backup"
AFTER=$(cypher blog "neo4j://localhost:7687" "$PASSWORD" "MATCH (n) RETURN count(n) AS c" | tail -1)
[ "$AFTER" = "$BEFORE" ] || fail "node count changed across the backup: $BEFORE -> $AFTER"

log "PASS"
