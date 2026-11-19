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
- **TLS:** Bolt with TLS required, plus HTTPS, from any `kubernetes.io/tls` Secret, or a certificate the operator requests from cert-manager.
- **Public endpoints:** a hostname per instance (`neo4j+s://<hostname>:7687`), published with external-dns.
- **Usage metrics:** data volume usage in the status and as Prometheus gauges, for dashboards or billing.
- **Online volume expansion:** grow `storage.size` and the claim is resized in place.
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
With a [public hostname](#public-endpoints), `status.endpoint` is the public URI instead.
To bring your own password, set `spec.passwordSecretRef: { name, key }`.

> The password is applied when the database is first created. Changing the Secret later does not
> change the password of an existing database.

## Public endpoints

To let applications outside the cluster connect (for example serverless functions), give the
instance a public `hostname`. TLS is required, and cert-manager can issue the certificate:

```yaml
apiVersion: cyphra.dev/v1alpha1
kind: Neo4jInstance
metadata:
  name: abc123
spec:
  hostname: abc123.eu.db.example.com
  tls:
    issuerRef: { name: letsencrypt, kind: ClusterIssuer } # certificate goes to the Secret abc123-tls
```

With a hostname, the operator:

- sets `status.endpoint` (and the `uri` key of `<name>-auth`) to `neo4j+s://abc123.eu.db.example.com:7687`;
- makes Neo4j advertise the hostname (`server.default_advertised_address`). Routing drivers
  (`neo4j+s://`) connect to the address the server advertises, so without it they would be sent to
  an internal pod name;
- annotates the client Service with `external-dns.alpha.kubernetes.io/hostname`, so
  [external-dns](https://github.com/kubernetes-sigs/external-dns) publishes the DNS record. Your own
  `service.annotations` are merged in and win over the operator's;
- defaults `service.type` to `LoadBalancer`.

With `tls.issuerRef`, the operator creates a cert-manager `Certificate` (`cert-manager.io/v1`) named
after the instance for the hostname (or for the in-cluster Service names when there is no hostname),
writing into `tls.secretName` (default `<name>-tls`). The instance becomes `Ready` only once the
`CertificateReady` condition is true. cert-manager is optional: without it, `issuerRef` reports
`CertManagerNotInstalled` and a `tls.secretName` you manage yourself still works. For Let's Encrypt,
use a DNS-01 solver: the Service only serves Bolt and HTTPS, so HTTP-01 challenges cannot reach it.

Neo4j loads certificates only at startup. When the TLS Secret changes, whether cert-manager renews it or you
replace it, the operator restarts the instance so it serves the new certificate. Expect a short
interruption about once per renewal, which is every 60 days with Let's Encrypt.

**One LoadBalancer per instance** is the default. It is simple, but every instance costs a cloud
load balancer. The design also works behind a single shared L4 proxy that routes on TLS SNI
(Traefik `HostSNI`, HAProxy, Envoy, Contour `TLSRoute` passthrough, and so on): Neo4j terminates TLS
itself, the certificate and advertised address carry the hostname, and the advertised port stays 7687.
For that, set `service.type: ClusterIP`, point a wildcard DNS record (`*.eu.db.example.com`) at the
proxy, and route each SNI name to `<name>.<namespace>.svc:7687`. The proxy must listen on 7687.

HTTPS (7473) is exposed next to Bolt. Clients that cannot open TCP sockets (for example edge
runtimes) can use Neo4j's HTTP Query API (Neo4j 5.19 or later) at
`https://<hostname>:7473/db/neo4j/query/v2`.

## Storage

### Usage

`status.storage` reports the data volume:

```yaml
status:
  storage:
    usedBytes: 524288000          # measured by the kubelet
    capacityBytes: 21474836480    # the claim's provisioned capacity
    lastUpdateTime: "2026-11-19T10:05:00Z"
```

`usedBytes` comes from the kubelet stats summary, read through the API server
(`/api/v1/nodes/<node>/proxy/stats/summary`), every 5 minutes (Helm value `usageRefreshInterval`;
`0` turns it off). This works for every volume type the kubelet measures, without exec'ing into the
database pod. It needs the `nodes/proxy` `get` permission, which is broad: it reaches every kubelet
API on every node. Turn the feature off if you do not want the operator to hold it. While the
instance is stopped, the last measurement is kept. Volumes the kubelet does not measure (such as
`hostPath`) only report `capacityBytes`.

### Metrics

The operator's metrics endpoint (`:8080/metrics`) exports, per instance (`namespace`, `name` labels):

| Metric | |
| --- | --- |
| `cyphra_neo4j_instance_phase{phase}` | 1 for the current phase, 0 for the others |
| `cyphra_neo4j_storage_used_bytes` | `status.storage.usedBytes` |
| `cyphra_neo4j_storage_capacity_bytes` | `status.storage.capacityBytes` |
| `cyphra_neo4j_last_successful_backup_timestamp_seconds` | Unix time of the last successful backup |

Series are removed when the instance is deleted. `kubectl get neo4jinstances -o wide` also shows the
used bytes and the hostname.

### Resizing

Increase `storage.size` to grow the volume online. StatefulSet volume claim templates cannot change,
so the operator patches the claim `data-<name>-0` directly. This needs a StorageClass with
`allowVolumeExpansion: true`. The `StorageResizeFailed` condition tracks the resize:

| Status / reason | |
| --- | --- |
| `False` / `Expanding` | Expansion requested, waiting for the storage provider |
| `False` / `InSync` | The volume has the requested size |
| `True` / `ExpansionNotSupported` | The StorageClass does not allow expansion; nothing was changed |
| `True` / `ShrinkNotSupported` | `storage.size` is smaller than the volume; volumes cannot shrink |

Some storage drivers grow the file system only when the pod restarts (the claim then has the
`FileSystemResizePending` condition). The next restart, such as a backup, completes it.

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
| `storage.size` / `storage.storageClassName` | `10Gi` / cluster default | Data volume; can grow, not shrink |
| `passwordSecretRef` | generated `<name>-auth` | Existing Secret key with the password |
| `hostname` | | Public DNS name: `neo4j+s://<hostname>:7687`, advertised address, external-dns. Requires `tls` |
| `service.type` / `service.annotations` | `LoadBalancer` with a hostname, else `ClusterIP` | Client Service |
| `tls.secretName` | `<name>-tls` with `issuerRef` | `kubernetes.io/tls` Secret: TLS for Bolt (required) and HTTPS |
| `tls.issuerRef` | | cert-manager issuer `{ name, kind, group }` (`kind` defaults to `Issuer`) |
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
make e2e         # kind cluster: plain and TLS + hostname instances, usage, backup to S3, restart, resize
```

## License

Apache-2.0
