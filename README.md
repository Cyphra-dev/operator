# Cyphra Operator

Run **Neo4j Community** databases on any Kubernetes cluster, declaratively:

```yaml
apiVersion: cyphra.dev/v1alpha1
kind: Neo4jInstance
metadata:
  name: blog
spec:
  version: "5.26"
  resources:
    limits: { memory: 2Gi }
  storage: { size: 20Gi }
  tls: { secretName: blog-tls }
  backup:
    schedule: "0 3 * * *"
    s3: { bucket: my-backups, region: eu-west-1, credentialsSecretName: s3-backups }
```

```console
$ kubectl get neo4jinstances
NAME   PHASE   VERSION   ENDPOINT                         LAST BACKUP   AGE
blog   Ready   5.26      neo4j+s://blog.default.svc:7687  3h            12d
```

The operator gives you:
- **Storage:** a StatefulSet with persistent storage. The data survives restarts, rescheduling and deleting the instance.
- **Credentials:** a generated password in `<name>-auth`, with keys `username`, `password` and `uri`.
- **Memory sizing:** heap and page cache derived from your memory limit.
- **Exposure:** Bolt and HTTP through a Service: `ClusterIP`, `LoadBalancer` or `NodePort`.
- **TLS:** Bolt with TLS required, plus HTTPS, from any `kubernetes.io/tls` Secret (for example one issued by cert-manager).
- **Backups:** scheduled and on-demand, uploaded to any S3-compatible storage (AWS S3, MinIO, R2…), with retention.

It is part of [Cyphra](https://github.com/Cyphra-dev/cyphra), the TypeScript ORM for Neo4j, but works
with any Neo4j client.

## Install

```bash
helm install cyphra-operator oci://ghcr.io/cyphra-dev/charts/cyphra-operator \
  -n cyphra-system --create-namespace
kubectl apply -f examples/minimal.yaml
```

Requirements: Kubernetes 1.30 or later, and a default StorageClass (or set `spec.storage.storageClassName`).

## Connecting

```bash
kubectl get secret blog-auth -o jsonpath='{.data.password}' | base64 -d
kubectl port-forward svc/blog 7687:7687   # then bolt://localhost:7687, user neo4j
```

Inside the cluster, use `status.endpoint` (`neo4j://blog.<namespace>.svc:7687`, or `neo4j+s://` with TLS).
To bring your own password, set `spec.passwordSecretRef: { name, key }`.

> The password is applied when the database is first created. Changing the Secret later does not
> change the password of an existing database.

## Backups

Neo4j Community has **no online backup**. To take a consistent dump, the operator:

1. scales the instance to 0 (phase `BackingUp`);
2. runs a Job that executes `neo4j-admin database dump neo4j` on the data volume and uploads
   `<prefix>/<name>-<timestamp>.dump` with rclone;
3. deletes dumps older than `retentionDays`;
4. starts the instance again.

**Expect downtime** for the length of the dump, which depends on database size. Schedule backups
accordingly. A failed backup sets the `BackupFailed` condition, emits an event, and restarts the instance anyway.

The S3 credentials Secret needs the keys `accessKeyId` and `secretAccessKey`. Set `endpoint` for
non-AWS providers. `prefix` defaults to `<namespace>/<name>`.

On-demand backup (for example before an upgrade):

```bash
kubectl annotate neo4jinstance blog cyphra.dev/backup-requested-at="$(date -u +%FT%TZ)" --overwrite
```

### Restore

Stop the instance (data is kept), load the dump with `neo4j-admin database load`, then start it again:

```bash
kubectl patch neo4jinstance blog --type=merge -p '{"spec":{"stopped":true}}'
kubectl run restore --rm -it --restart=Never --image=neo4j:5.26-community \
  --overrides='{"spec":{"securityContext":{"runAsUser":7474,"fsGroup":7474},"volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"data-blog-0"}}],"containers":[{"name":"restore","image":"neo4j:5.26-community","stdin":true,"tty":true,"command":["bash"],"volumeMounts":[{"name":"data","mountPath":"/data"}]}]}}'
# inside: download the dump to /tmp, then
#   neo4j-admin database load neo4j --from-path=/tmp --overwrite-destination=true
kubectl patch neo4jinstance blog --type=merge -p '{"spec":{"stopped":false}}'
```

A `Neo4jRestore` resource is on the roadmap.

## Spec reference

| Field | Default | |
| --- | --- | --- |
| `version` | `5.26` | Image tag `neo4j:<version>-community` |
| `image` | | Override the image |
| `resources` | | Container resources; heap is 40% and page cache 30% of `limits.memory` |
| `storage.size` / `storage.storageClassName` | `10Gi` / cluster default | Data volume |
| `passwordSecretRef` | generated `<name>-auth` | Existing Secret key with the password |
| `service.type` / `service.annotations` | `ClusterIP` | Client Service |
| `tls.secretName` | | `kubernetes.io/tls` Secret: TLS for Bolt (required) and HTTPS |
| `backup.schedule` | | Cron, UTC |
| `backup.retentionDays` | `7` | `0` keeps everything |
| `backup.s3` | | `bucket`, `prefix`, `endpoint`, `region`, `credentialsSecretName` |
| `backup.suspend` | `false` | Pause scheduled backups (on-demand still works) |
| `stopped` | `false` | Scale to zero, keeping data (maintenance, restores) |
| `config` | | Extra Neo4j settings, e.g. `db.transaction.timeout: 60s` |

## Scope

This operator manages **single-server Neo4j Community** instances, which is what Community supports.
Clustering, online backup, role-based access control and multiple user databases are Neo4j Enterprise
features and are out of scope. Neo4j is a trademark of Neo4j, Inc.; this project is not affiliated
with Neo4j, Inc.

## Development

```bash
make test        # vet + unit tests (Go runs in Docker if it is not installed)
make generate    # deepcopy, CRD and RBAC (controller-gen), synced into the Helm chart
make e2e         # kind cluster: install, provision plain and TLS instances, write data, back up to S3, verify
```

## License

Apache-2.0
