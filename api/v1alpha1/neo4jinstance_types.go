package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Neo4jInstanceSpec is the desired state of a single-server Neo4j Community instance.
// +kubebuilder:validation:XValidation:rule="!has(self.hostname) || has(self.tls)",message="hostname requires tls (secretName or issuerRef)"
type Neo4jInstanceSpec struct {
	// Neo4j version, used as the image tag `neo4j:<version>-community`.
	// +kubebuilder:default="5.26"
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+){0,2}$`
	Version string `json:"version,omitempty"`

	// Image overrides the container image (the version is then ignored for the image).
	// +optional
	Image string `json:"image,omitempty"`

	// Resources for the Neo4j container. Heap and page cache are sized from the memory limit.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Storage for the data volume.
	// +optional
	Storage StorageSpec `json:"storage,omitempty"`

	// PasswordSecretRef points to an existing Secret key holding the `neo4j` user's password.
	// When empty, the operator generates one in the Secret `<name>-auth` (key `password`).
	// +optional
	PasswordSecretRef *corev1.SecretKeySelector `json:"passwordSecretRef,omitempty"`

	// Service exposes Bolt (7687) and HTTP (7474).
	// +optional
	Service ServiceSpec `json:"service,omitempty"`

	// Hostname is the public DNS name clients use, e.g. `abc123.eu.db.example.com`. When set,
	// `status.endpoint` becomes `neo4j+s://<hostname>:7687`, Neo4j advertises this name to routing
	// drivers, the client Service gets an `external-dns.alpha.kubernetes.io/hostname` annotation and
	// its type defaults to LoadBalancer. Requires `tls`.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Hostname string `json:"hostname,omitempty"`

	// TLS enables encrypted Bolt and HTTPS using a `kubernetes.io/tls` Secret (e.g. from cert-manager).
	// +optional
	TLS *TLSSpec `json:"tls,omitempty"`

	// Backup schedules offline dumps uploaded to S3-compatible storage.
	// +optional
	Backup *BackupSpec `json:"backup,omitempty"`

	// Stopped scales the instance to zero while keeping its data (e.g. to restore a dump).
	// +optional
	Stopped bool `json:"stopped,omitempty"`

	// Config adds Neo4j settings, e.g. `db.transaction.timeout: 30s`. Keys are Neo4j setting names.
	// +optional
	Config map[string]string `json:"config,omitempty"`
}

// StorageSpec configures the PersistentVolumeClaim for /data.
type StorageSpec struct {
	// +kubebuilder:default="10Gi"
	Size resource.Quantity `json:"size,omitempty"`
	// StorageClassName for the claim; empty uses the cluster default.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// ServiceSpec configures the client-facing Service.
type ServiceSpec struct {
	// Type of the Service. Defaults to LoadBalancer when `hostname` is set, ClusterIP otherwise.
	// +kubebuilder:validation:Enum=ClusterIP;LoadBalancer;NodePort
	// +optional
	Type corev1.ServiceType `json:"type,omitempty"`
	// Annotations for the Service (e.g. cloud load-balancer settings).
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// TLSSpec references the certificate used for Bolt and HTTPS.
// +kubebuilder:validation:XValidation:rule="(has(self.secretName) && self.secretName != ”) || has(self.issuerRef)",message="tls needs secretName, issuerRef or both"
type TLSSpec struct {
	// SecretName of a `kubernetes.io/tls` Secret with `tls.crt` and `tls.key`.
	// With `issuerRef`, the issued certificate is written here; it then defaults to `<name>-tls`.
	// +optional
	SecretName string `json:"secretName,omitempty"`
	// IssuerRef makes the operator request the certificate from cert-manager: a
	// `cert-manager.io/v1` Certificate for `hostname` (or the in-cluster Service names when no
	// hostname is set). The instance becomes Ready once the certificate is issued.
	// +optional
	IssuerRef *IssuerRef `json:"issuerRef,omitempty"`
}

// IssuerRef references a cert-manager Issuer or ClusterIssuer.
type IssuerRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Kind is `Issuer` (same namespace) or `ClusterIssuer`, or an external issuer's kind.
	// +kubebuilder:default=Issuer
	// +optional
	Kind string `json:"kind,omitempty"`
	// Group of an external issuer; defaults to `cert-manager.io`.
	// +optional
	Group string `json:"group,omitempty"`
}

// BackupSpec configures scheduled backups.
//
// Neo4j Community has no online backup, so each backup stops the instance, runs
// `neo4j-admin database dump`, uploads the dump, and starts the instance again.
type BackupSpec struct {
	// Cron schedule (UTC), e.g. "0 3 * * *".
	Schedule string `json:"schedule"`
	// Keep backups for this many days (older dumps under the prefix are deleted). 0 keeps everything.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=7
	RetentionDays int32  `json:"retentionDays,omitempty"`
	S3            S3Spec `json:"s3"`
	// Suspend stops scheduling new backups.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// S3Spec is an S3-compatible destination (AWS S3, MinIO, R2, …).
type S3Spec struct {
	Bucket string `json:"bucket"`
	// Prefix inside the bucket; defaults to `<namespace>/<name>`.
	// +optional
	Prefix string `json:"prefix,omitempty"`
	// Endpoint URL for non-AWS providers, e.g. https://minio.example.com.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// +optional
	Region string `json:"region,omitempty"`
	// Secret with keys `accessKeyId` and `secretAccessKey`.
	CredentialsSecretName string `json:"credentialsSecretName"`
}

// Phase is a coarse lifecycle state.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;BackingUp;Stopped;Failed
type Phase string

const (
	PhasePending      Phase = "Pending"
	PhaseProvisioning Phase = "Provisioning"
	PhaseReady        Phase = "Ready"
	PhaseBackingUp    Phase = "BackingUp"
	PhaseStopped      Phase = "Stopped"
	PhaseFailed       Phase = "Failed"
)

// Condition types.
const (
	ConditionReady        = "Ready"
	ConditionBackupFailed = "BackupFailed"
	// ConditionCertificateReady mirrors the cert-manager Certificate when `tls.issuerRef` is set.
	ConditionCertificateReady = "CertificateReady"
	// ConditionStorageResizeFailed is True when `storage.size` cannot be applied to the data volume
	// (shrinking, or a StorageClass without volume expansion).
	ConditionStorageResizeFailed = "StorageResizeFailed"
)

// StorageStatus reports the data volume's size and usage.
type StorageStatus struct {
	// Bytes used on the data volume, as reported by the kubelet. Kept while the instance is stopped.
	// +optional
	UsedBytes int64 `json:"usedBytes,omitempty"`
	// Provisioned size of the data volume (the PersistentVolumeClaim's capacity).
	// +optional
	CapacityBytes int64 `json:"capacityBytes,omitempty"`
	// When usedBytes was last measured.
	// +optional
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`
}

// BackupStatus records backup progress.
type BackupStatus struct {
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`
	// +optional
	LastSuccessfulTime *metav1.Time `json:"lastSuccessfulTime,omitempty"`
	// Object key of the last successful dump.
	// +optional
	LastSuccessfulKey string `json:"lastSuccessfulKey,omitempty"`
	// Name of the backup Job currently running, if any.
	// +optional
	ActiveJob string `json:"activeJob,omitempty"`
}

// Neo4jInstanceStatus is the observed state.
type Neo4jInstanceStatus struct {
	// +optional
	Phase Phase `json:"phase,omitempty"`
	// Bolt URI clients use: in-cluster (neo4j://blog.default.svc:7687), or the public
	// neo4j+s://<hostname>:7687 when `spec.hostname` is set.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// Secret (and key) holding the password.
	// +optional
	PasswordSecret string `json:"passwordSecret,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Backup BackupStatus `json:"backup,omitempty"`
	// +optional
	Storage StorageStatus `json:"storage,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Neo4jInstance is a managed single-server Neo4j Community database.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=n4j;neo4j,categories=cyphra
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`
// +kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.spec.storage.size`
// +kubebuilder:printcolumn:name="Used bytes",type=integer,JSONPath=`.status.storage.usedBytes`,priority=1
// +kubebuilder:printcolumn:name="Hostname",type=string,JSONPath=`.spec.hostname`,priority=1
// +kubebuilder:printcolumn:name="Last backup",type=date,JSONPath=`.status.backup.lastSuccessfulTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Neo4jInstance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   Neo4jInstanceSpec   `json:"spec,omitempty"`
	Status Neo4jInstanceStatus `json:"status,omitempty"`
}

// Neo4jInstanceList contains a list of Neo4jInstance.
// +kubebuilder:object:root=true
type Neo4jInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Neo4jInstance `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Neo4jInstance{}, &Neo4jInstanceList{})
}
