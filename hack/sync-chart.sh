#!/usr/bin/env sh
# Copy generated CRDs and RBAC rules (config/) into the Helm chart.
set -eu
cd "$(dirname "$0")/.."
cp config/crd/*.yaml charts/cyphra-operator/crds/
{
  echo "# Generated from config/rbac/role.yaml by hack/sync-chart.sh. Do not edit."
  echo "apiVersion: rbac.authorization.k8s.io/v1"
  echo "kind: ClusterRole"
  echo "metadata:"
  echo '  name: {{ include "cyphra-operator.fullname" . }}'
  echo '  labels: {{- include "cyphra-operator.labels" . | nindent 4 }}'
  sed -n '/^rules:/,$p' config/rbac/role.yaml
} > charts/cyphra-operator/templates/clusterrole.yaml
