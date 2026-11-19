package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	cyphrav1 "github.com/Cyphra-dev/operator/api/v1alpha1"
)

const (
	boltPort  = 7687
	httpPort  = 7474
	httpsPort = 7473

	neo4jUID = 7474

	dataVolume  = "data"
	tlsVolume   = "tls"
	backupVol   = "backup"
	tlsMountDir = "/ssl"

	// RcloneImage uploads dumps to S3-compatible storage.
	RcloneImage = "rclone/rclone:1.71"

	// ExternalDNSHostnameAnnotation asks external-dns to publish the Service under spec.hostname.
	ExternalDNSHostnameAnnotation = "external-dns.alpha.kubernetes.io/hostname"
)

// DefaultStorageSize is used when spec.storage.size is empty.
var DefaultStorageSize = resource.MustParse("10Gi")

// Names of the objects owned by an instance.
func authSecretName(inst *cyphrav1.Neo4jInstance) string { return inst.Name + "-auth" }
func headlessServiceName(inst *cyphrav1.Neo4jInstance) string {
	return inst.Name + "-headless"
}

// TLSSecretName is the Secret mounted for TLS: spec.tls.secretName, or `<name>-tls` when the
// certificate is issued by cert-manager.
func TLSSecretName(inst *cyphrav1.Neo4jInstance) string {
	if inst.Spec.TLS != nil && inst.Spec.TLS.SecretName != "" {
		return inst.Spec.TLS.SecretName
	}
	return inst.Name + "-tls"
}

// PodName is the single Neo4j pod of the StatefulSet.
func PodName(inst *cyphrav1.Neo4jInstance) string { return inst.Name + "-0" }

// DesiredStorage is the requested size of the data volume.
func DesiredStorage(inst *cyphrav1.Neo4jInstance) resource.Quantity {
	if inst.Spec.Storage.Size.IsZero() {
		return DefaultStorageSize.DeepCopy()
	}
	return inst.Spec.Storage.Size.DeepCopy()
}

// PVCName is the claim created by the StatefulSet's volumeClaimTemplate for pod 0.
func PVCName(inst *cyphrav1.Neo4jInstance) string {
	return fmt.Sprintf("%s-%s-0", dataVolume, inst.Name)
}

// Labels applied to every owned object; SelectorLabels is the immutable subset used by selectors.
func SelectorLabels(inst *cyphrav1.Neo4jInstance) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "neo4j",
		"app.kubernetes.io/instance": inst.Name,
	}
}

func Labels(inst *cyphrav1.Neo4jInstance) map[string]string {
	l := SelectorLabels(inst)
	l["app.kubernetes.io/managed-by"] = "cyphra-operator"
	l["app.kubernetes.io/version"] = inst.Spec.Version
	return l
}

// Image returns the container image for the instance.
func Image(inst *cyphrav1.Neo4jInstance) string {
	if inst.Spec.Image != "" {
		return inst.Spec.Image
	}
	v := inst.Spec.Version
	if v == "" {
		v = "5.26"
	}
	return "neo4j:" + v + "-community"
}

// PasswordRef returns the Secret key holding the neo4j password.
func PasswordRef(inst *cyphrav1.Neo4jInstance) corev1.SecretKeySelector {
	if inst.Spec.PasswordSecretRef != nil {
		return *inst.Spec.PasswordSecretRef
	}
	return corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: authSecretName(inst)},
		Key:                  "password",
	}
}

// Endpoint is the Bolt URI clients use: the public hostname when set, the in-cluster Service otherwise.
func Endpoint(inst *cyphrav1.Neo4jInstance) string {
	if inst.Spec.Hostname != "" {
		return fmt.Sprintf("neo4j+s://%s:%d", inst.Spec.Hostname, boltPort)
	}
	scheme := "neo4j"
	if inst.Spec.TLS != nil {
		scheme = "neo4j+s"
	}
	return fmt.Sprintf("%s://%s.%s.svc:%d", scheme, inst.Name, inst.Namespace, boltPort)
}

// SettingEnv converts a Neo4j setting name to the official image's environment variable
// (`server.memory.heap.max_size` → `NEO4J_server_memory_heap_max__size`).
func SettingEnv(setting string) string {
	return "NEO4J_" + strings.ReplaceAll(strings.ReplaceAll(setting, "_", "__"), ".", "_")
}

// Settings computes the Neo4j configuration: memory sizing from the memory limit, TLS policies,
// then user config (which wins).
func Settings(inst *cyphrav1.Neo4jInstance) map[string]string {
	s := map[string]string{}
	if mem, ok := inst.Spec.Resources.Limits[corev1.ResourceMemory]; ok && !mem.IsZero() {
		mib := mem.Value() / (1024 * 1024)
		// Leave ~30% for the OS, native memory and transaction state.
		heap := mib * 40 / 100
		pagecache := mib * 30 / 100
		s["server.memory.heap.initial_size"] = fmt.Sprintf("%dm", heap)
		s["server.memory.heap.max_size"] = fmt.Sprintf("%dm", heap)
		s["server.memory.pagecache.size"] = fmt.Sprintf("%dm", pagecache)
	}
	if inst.Spec.TLS != nil {
		for _, scope := range []string{"bolt", "https"} {
			p := "dbms.ssl.policy." + scope
			s[p+".enabled"] = "true"
			s[p+".base_directory"] = tlsMountDir
			s[p+".private_key"] = "tls.key"
			s[p+".public_certificate"] = "tls.crt"
			s[p+".client_auth"] = "NONE"
		}
		s["server.bolt.tls_level"] = "REQUIRED"
		s["server.https.enabled"] = "true"
		s["server.http.enabled"] = "false"
	}
	if inst.Spec.Hostname != "" {
		// Routing drivers (neo4j+s://) connect to the address the server advertises, so it must be
		// the public name rather than the pod's.
		s["server.default_advertised_address"] = inst.Spec.Hostname
	}
	for k, v := range inst.Spec.Config {
		s[k] = v
	}
	return s
}

func neo4jEnv(inst *cyphrav1.Neo4jInstance) []corev1.EnvVar {
	ref := PasswordRef(inst)
	// The image turns every NEO4J_* variable into a setting, so the helper must not use that prefix.
	env := []corev1.EnvVar{
		{Name: "CYPHRA_NEO4J_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &ref}},
		// Only applied on first start (when the auth database is created).
		{Name: "NEO4J_AUTH", Value: "neo4j/$(CYPHRA_NEO4J_PASSWORD)"},
	}
	settings := Settings(inst)
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, corev1.EnvVar{Name: SettingEnv(k), Value: settings[k]})
	}
	return env
}

func podSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsUser:      ptr.To[int64](neo4jUID),
		RunAsGroup:     ptr.To[int64](neo4jUID),
		FSGroup:        ptr.To[int64](neo4jUID),
		RunAsNonRoot:   ptr.To(true),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func containerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// StatefulSetSpec builds the desired StatefulSet spec (replicas is 1, or 0 while a backup runs).
func StatefulSetSpec(inst *cyphrav1.Neo4jInstance, replicas int32) appsv1.StatefulSetSpec {
	ports := []corev1.ContainerPort{{Name: "bolt", ContainerPort: boltPort}}
	if inst.Spec.TLS != nil {
		ports = append(ports, corev1.ContainerPort{Name: "https", ContainerPort: httpsPort})
	} else {
		ports = append(ports, corev1.ContainerPort{Name: "http", ContainerPort: httpPort})
	}
	mounts := []corev1.VolumeMount{{Name: dataVolume, MountPath: "/data"}}
	var volumes []corev1.Volume
	if inst.Spec.TLS != nil {
		mounts = append(mounts, corev1.VolumeMount{Name: tlsVolume, MountPath: tlsMountDir, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{
			Name: tlsVolume,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  TLSSecretName(inst),
				DefaultMode: ptr.To[int32](0o440),
			}},
		})
	}
	bolt := corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(boltPort)}}

	storage := DesiredStorage(inst)

	return appsv1.StatefulSetSpec{
		Replicas:            ptr.To(replicas),
		ServiceName:         headlessServiceName(inst),
		Selector:            &metav1.LabelSelector{MatchLabels: SelectorLabels(inst)},
		PodManagementPolicy: appsv1.OrderedReadyPodManagement,
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: Labels(inst)},
			Spec: corev1.PodSpec{
				SecurityContext:               podSecurityContext(),
				TerminationGracePeriodSeconds: ptr.To[int64](120),
				Containers: []corev1.Container{{
					Name:            "neo4j",
					Image:           Image(inst),
					Env:             neo4jEnv(inst),
					Ports:           ports,
					Resources:       inst.Spec.Resources,
					VolumeMounts:    mounts,
					SecurityContext: containerSecurityContext(),
					StartupProbe: &corev1.Probe{
						ProbeHandler: bolt, PeriodSeconds: 5, FailureThreshold: 120,
					},
					ReadinessProbe: &corev1.Probe{ProbeHandler: bolt, PeriodSeconds: 5},
					LivenessProbe: &corev1.Probe{
						ProbeHandler: bolt, PeriodSeconds: 15, FailureThreshold: 4,
					},
				}},
				Volumes: volumes,
			},
		},
		VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
			ObjectMeta: metav1.ObjectMeta{Name: dataVolume, Labels: SelectorLabels(inst)},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: inst.Spec.Storage.StorageClassName,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: storage},
				},
			},
		}},
		// Keep data when the instance is scaled down for backups.
		PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
			WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		},
	}
}

// ServiceType is the client Service type: spec.service.type, else LoadBalancer for a public
// hostname and ClusterIP otherwise.
func ServiceType(inst *cyphrav1.Neo4jInstance) corev1.ServiceType {
	switch {
	case inst.Spec.Service.Type != "":
		return inst.Spec.Service.Type
	case inst.Spec.Hostname != "":
		return corev1.ServiceTypeLoadBalancer
	default:
		return corev1.ServiceTypeClusterIP
	}
}

// ServiceAnnotations are the user's annotations plus the external-dns hostname (user values win).
func ServiceAnnotations(inst *cyphrav1.Neo4jInstance) map[string]string {
	if inst.Spec.Hostname == "" && len(inst.Spec.Service.Annotations) == 0 {
		return nil
	}
	a := map[string]string{}
	if inst.Spec.Hostname != "" {
		a[ExternalDNSHostnameAnnotation] = inst.Spec.Hostname
	}
	for k, v := range inst.Spec.Service.Annotations {
		a[k] = v
	}
	return a
}

// Certificate builds the cert-manager Certificate for spec.tls.issuerRef. It is unstructured so
// that cert-manager stays an optional dependency.
func Certificate(inst *cyphrav1.Neo4jInstance) *unstructured.Unstructured {
	ref := inst.Spec.TLS.IssuerRef
	kind, group := ref.Kind, ref.Group
	if kind == "" {
		kind = "Issuer"
	}
	if group == "" {
		group = "cert-manager.io"
	}
	dnsNames := []any{inst.Spec.Hostname}
	if inst.Spec.Hostname == "" {
		dnsNames = []any{
			fmt.Sprintf("%s.%s.svc", inst.Name, inst.Namespace),
			fmt.Sprintf("%s.%s.svc.cluster.local", inst.Name, inst.Namespace),
		}
	}
	labels := map[string]any{}
	for k, v := range Labels(inst) {
		labels[k] = v
	}
	cert := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"secretName": TLSSecretName(inst),
			"dnsNames":   dnsNames,
			"issuerRef":  map[string]any{"name": ref.Name, "kind": kind, "group": group},
			// Neo4j (Java) reads PKCS#8 keys.
			"privateKey":     map[string]any{"algorithm": "RSA", "size": int64(2048), "encoding": "PKCS8", "rotationPolicy": "Always"},
			"secretTemplate": map[string]any{"labels": labels},
		},
	}}
	cert.SetGroupVersionKind(CertificateGVK)
	cert.SetName(inst.Name)
	cert.SetNamespace(inst.Namespace)
	cert.SetLabels(Labels(inst))
	return cert
}

// CertificateGVK is cert-manager's Certificate kind.
var CertificateGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}

// CertificateReady reports the Certificate's Ready condition and its message.
func CertificateReady(cert *unstructured.Unstructured) (bool, string) {
	conds, _, _ := unstructured.NestedSlice(cert.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != "Ready" {
			continue
		}
		msg, _ := m["message"].(string)
		return m["status"] == "True", msg
	}
	return false, "waiting for cert-manager to issue the certificate"
}

// ServicePorts are the client-facing ports.
func ServicePorts(inst *cyphrav1.Neo4jInstance) []corev1.ServicePort {
	ports := []corev1.ServicePort{{Name: "bolt", Port: boltPort, TargetPort: intstr.FromString("bolt")}}
	if inst.Spec.TLS != nil {
		ports = append(ports, corev1.ServicePort{Name: "https", Port: httpsPort, TargetPort: intstr.FromString("https")})
	} else {
		ports = append(ports, corev1.ServicePort{Name: "http", Port: httpPort, TargetPort: intstr.FromString("http")})
	}
	return ports
}

// BackupPrefix is the object prefix for an instance's dumps.
func BackupPrefix(inst *cyphrav1.Neo4jInstance) string {
	if inst.Spec.Backup != nil && inst.Spec.Backup.S3.Prefix != "" {
		return strings.Trim(inst.Spec.Backup.S3.Prefix, "/")
	}
	return inst.Namespace + "/" + inst.Name
}

// BackupJob builds the Job that dumps the (stopped) database and uploads it.
// The init container runs `neo4j-admin database dump` on the data volume; the main container
// uploads the dump with rclone and applies retention.
func BackupJob(inst *cyphrav1.Neo4jInstance, jobName, key string) *batchv1.Job {
	b := inst.Spec.Backup
	secret := func(k string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: b.S3.CredentialsSecretName},
			Key:                  k,
		}}
	}
	provider := "AWS"
	if b.S3.Endpoint != "" {
		provider = "Other"
	}
	env := []corev1.EnvVar{
		{Name: "HOME", Value: "/tmp"},
		{Name: "RCLONE_S3_PROVIDER", Value: provider},
		{Name: "RCLONE_S3_ENV_AUTH", Value: "false"},
		{Name: "RCLONE_S3_ACCESS_KEY_ID", ValueFrom: secret("accessKeyId")},
		{Name: "RCLONE_S3_SECRET_ACCESS_KEY", ValueFrom: secret("secretAccessKey")},
		{Name: "RCLONE_S3_NO_CHECK_BUCKET", Value: "true"},
		{Name: "BUCKET", Value: b.S3.Bucket},
		{Name: "KEY", Value: key},
		{Name: "PREFIX", Value: BackupPrefix(inst)},
		{Name: "RETENTION_DAYS", Value: fmt.Sprint(b.RetentionDays)},
	}
	if b.S3.Endpoint != "" {
		env = append(env, corev1.EnvVar{Name: "RCLONE_S3_ENDPOINT", Value: b.S3.Endpoint})
	}
	if b.S3.Region != "" {
		env = append(env, corev1.EnvVar{Name: "RCLONE_S3_REGION", Value: b.S3.Region})
	}
	upload := `set -eu
rclone copyto /backup/neo4j.dump ":s3:${BUCKET}/${KEY}"
if [ "${RETENTION_DAYS}" -gt 0 ]; then
  rclone delete --min-age "${RETENTION_DAYS}d" --include "*.dump" ":s3:${BUCKET}/${PREFIX}/"
fi
echo "uploaded s3://${BUCKET}/${KEY}"`

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: inst.Namespace,
			Labels:    Labels(inst),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To[int32](0),
			TTLSecondsAfterFinished: ptr.To[int32](7 * 24 * 3600),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					"app.kubernetes.io/name":      "neo4j-backup",
					"app.kubernetes.io/instance":  inst.Name,
					"app.kubernetes.io/component": "backup",
				}},
				Spec: corev1.PodSpec{
					RestartPolicy:   corev1.RestartPolicyNever,
					SecurityContext: podSecurityContext(),
					InitContainers: []corev1.Container{{
						Name:  "dump",
						Image: Image(inst),
						Command: []string{
							"neo4j-admin", "database", "dump", "neo4j",
							"--to-path=/backup", "--overwrite-destination=true",
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: dataVolume, MountPath: "/data"},
							{Name: backupVol, MountPath: "/backup"},
						},
						SecurityContext: containerSecurityContext(),
					}},
					Containers: []corev1.Container{{
						Name:            "upload",
						Image:           RcloneImage,
						Command:         []string{"sh", "-c", upload},
						Env:             env,
						VolumeMounts:    []corev1.VolumeMount{{Name: backupVol, MountPath: "/backup", ReadOnly: true}},
						SecurityContext: containerSecurityContext(),
					}},
					Volumes: []corev1.Volume{
						{Name: dataVolume, VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: PVCName(inst)},
						}},
						{Name: backupVol, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
}

// TLSHashAnnotation on the pod template changes when the TLS certificate does, rolling the pod.
const TLSHashAnnotation = "cyphra.dev/tls-hash"

// TLSHash is a short fingerprint of a TLS Secret's certificate, key and CA.
func TLSHash(secret *corev1.Secret) string {
	h := sha256.New()
	for _, k := range []string{corev1.TLSCertKey, corev1.TLSPrivateKeyKey, "ca.crt"} {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write(secret.Data[k])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// SetTLSHash records the TLS fingerprint on a pod template (no-op for "").
func SetTLSHash(t *corev1.PodTemplateSpec, hash string) {
	if hash == "" {
		return
	}
	if t.Annotations == nil {
		t.Annotations = map[string]string{}
	}
	t.Annotations[TLSHashAnnotation] = hash
}
