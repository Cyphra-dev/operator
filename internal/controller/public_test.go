package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cyphrav1 "github.com/Cyphra-dev/operator/api/v1alpha1"
)

func publicInstance() *cyphrav1.Neo4jInstance {
	inst := newInstance()
	inst.Spec.Hostname = "abc123.eu.db.example.com"
	inst.Spec.TLS = &cyphrav1.TLSSpec{SecretName: "blog-tls"}
	return inst
}

func (h *harness) service() *corev1.Service {
	h.t.Helper()
	var svc corev1.Service
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "blog"}, &svc); err != nil {
		h.t.Fatal(err)
	}
	return &svc
}

func gaugeValue(t *testing.T, g *prometheus.GaugeVec, labels ...string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := g.WithLabelValues(labels...).Write(m); err != nil {
		t.Fatal(err)
	}
	return m.GetGauge().GetValue()
}

func seriesFor(c prometheus.Collector, namespace, name string) int {
	ch := make(chan prometheus.Metric, 100)
	c.Collect(ch)
	close(ch)
	n := 0
	for metric := range ch {
		m := &dto.Metric{}
		_ = metric.Write(m)
		var ns, nm string
		for _, l := range m.GetLabel() {
			switch l.GetName() {
			case "namespace":
				ns = l.GetValue()
			case "name":
				nm = l.GetValue()
			}
		}
		if ns == namespace && nm == name {
			n++
		}
	}
	return n
}

// --- public hostname -------------------------------------------------------------

func TestPublicHostname(t *testing.T) {
	inst := publicInstance()
	if got := Endpoint(inst); got != "neo4j+s://abc123.eu.db.example.com:7687" {
		t.Errorf("endpoint = %s", got)
	}
	if got := Settings(inst)["server.default_advertised_address"]; got != "abc123.eu.db.example.com" {
		t.Errorf("advertised address = %q", got)
	}
	if got := ServiceType(inst); got != corev1.ServiceTypeLoadBalancer {
		t.Errorf("service type = %s, want LoadBalancer by default with a hostname", got)
	}
	inst.Spec.Service.Type = corev1.ServiceTypeClusterIP // e.g. behind a shared SNI proxy
	if got := ServiceType(inst); got != corev1.ServiceTypeClusterIP {
		t.Errorf("explicit service type ignored: %s", got)
	}

	inst.Spec.Service.Annotations = map[string]string{"service.beta.kubernetes.io/aws-load-balancer-type": "nlb"}
	a := ServiceAnnotations(inst)
	if a[ExternalDNSHostnameAnnotation] != "abc123.eu.db.example.com" || a["service.beta.kubernetes.io/aws-load-balancer-type"] != "nlb" {
		t.Errorf("annotations = %v", a)
	}
	inst.Spec.Service.Annotations[ExternalDNSHostnameAnnotation] = "a.example.com,b.example.com"
	if got := ServiceAnnotations(inst)[ExternalDNSHostnameAnnotation]; got != "a.example.com,b.example.com" {
		t.Errorf("user annotation must win, got %s", got)
	}
	if inst.Spec.Service.Annotations[ExternalDNSHostnameAnnotation] != "a.example.com,b.example.com" {
		t.Error("spec annotations must not be mutated")
	}

	plain := newInstance()
	if ServiceAnnotations(plain) != nil || ServiceType(plain) != corev1.ServiceTypeClusterIP {
		t.Error("instances without a hostname keep a plain ClusterIP Service")
	}
	if _, ok := Settings(plain)["server.default_advertised_address"]; ok {
		t.Error("no advertised address without a hostname")
	}
}

func TestReconcileAddsHostname(t *testing.T) {
	inst := newInstance()
	inst.Spec.TLS = &cyphrav1.TLSSpec{SecretName: "blog-tls"}
	h := newHarness(t, inst)
	h.reconcile()
	if h.service().Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatal("expected ClusterIP before a hostname is set")
	}

	got := h.instance()
	got.Spec.Hostname = "abc123.eu.db.example.com"
	if err := h.c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	h.reconcile()

	svc := h.service()
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer || svc.Annotations[ExternalDNSHostnameAnnotation] != "abc123.eu.db.example.com" {
		t.Errorf("service = %s %v", svc.Spec.Type, svc.Annotations)
	}
	if ep := h.instance().Status.Endpoint; ep != "neo4j+s://abc123.eu.db.example.com:7687" {
		t.Errorf("status.endpoint = %s", ep)
	}
	env := envMap(h.sts().Spec.Template.Spec.Containers[0])
	if env["NEO4J_server_default__advertised__address"].Value != "abc123.eu.db.example.com" {
		t.Error("advertised address not passed to Neo4j")
	}
	var secret corev1.Secret
	_ = h.c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "blog-auth"}, &secret)
	if string(secret.Data["uri"]) != "neo4j+s://abc123.eu.db.example.com:7687" {
		t.Errorf("auth secret uri = %q", secret.Data["uri"])
	}
}

// --- cert-manager ----------------------------------------------------------------

func certScheme() *runtime.Scheme {
	s := newScheme()
	s.AddKnownTypeWithName(CertificateGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(CertificateGVK.GroupVersion().WithKind("CertificateList"), &unstructured.UnstructuredList{})
	return s
}

func (h *harness) certificate() *unstructured.Unstructured {
	h.t.Helper()
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(CertificateGVK)
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "blog"}, cert); err != nil {
		h.t.Fatalf("certificate: %v", err)
	}
	return cert
}

func TestCertificateIssuedBeforeReady(t *testing.T) {
	inst := newInstance()
	inst.Spec.Hostname = "abc123.eu.db.example.com"
	inst.Spec.TLS = &cyphrav1.TLSSpec{IssuerRef: &cyphrav1.IssuerRef{Name: "letsencrypt", Kind: "ClusterIssuer"}}
	h := newHarnessWithScheme(t, certScheme(), inst)
	h.reconcile()

	cert := h.certificate()
	spec := cert.Object["spec"].(map[string]any)
	if spec["secretName"] != "blog-tls" {
		t.Errorf("secretName = %v", spec["secretName"])
	}
	if names, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames"); len(names) != 1 || names[0] != "abc123.eu.db.example.com" {
		t.Errorf("dnsNames = %v", names)
	}
	if ref, _, _ := unstructured.NestedStringMap(cert.Object, "spec", "issuerRef"); ref["name"] != "letsencrypt" ||
		ref["kind"] != "ClusterIssuer" || ref["group"] != "cert-manager.io" {
		t.Errorf("issuerRef = %v", ref)
	}
	if owners := cert.GetOwnerReferences(); len(owners) != 1 || owners[0].Name != "blog" {
		t.Errorf("certificate must be owned by the instance: %v", owners)
	}
	if v := h.sts().Spec.Template.Spec.Volumes[0].Secret.SecretName; v != "blog-tls" {
		t.Errorf("mounted secret = %s", v)
	}

	// The pod is up, but the certificate is not issued yet: not Ready.
	h.setReady(1)
	res := h.reconcile()
	got := h.instance()
	if got.Status.Phase != cyphrav1.PhaseProvisioning || !meta.IsStatusConditionFalse(got.Status.Conditions, cyphrav1.ConditionCertificateReady) {
		t.Fatalf("expected Provisioning until the certificate is issued: %s %v", got.Status.Phase, got.Status.Conditions)
	}
	if res.RequeueAfter == 0 || res.RequeueAfter > 15*time.Second {
		t.Errorf("requeue = %v, want a short poll while issuing", res.RequeueAfter)
	}

	cert = h.certificate()
	_ = unstructured.SetNestedSlice(cert.Object, []any{map[string]any{"type": "Ready", "status": "True"}}, "status", "conditions")
	if err := h.c.Update(context.Background(), cert); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	got = h.instance()
	if got.Status.Phase != cyphrav1.PhaseReady || !meta.IsStatusConditionTrue(got.Status.Conditions, cyphrav1.ConditionCertificateReady) {
		t.Fatalf("expected Ready once issued: %s %v", got.Status.Phase, got.Status.Conditions)
	}
}

func TestCertificateWithoutHostnameCoversServiceNames(t *testing.T) {
	inst := newInstance()
	inst.Spec.TLS = &cyphrav1.TLSSpec{SecretName: "custom", IssuerRef: &cyphrav1.IssuerRef{Name: "ca"}}
	cert := Certificate(inst)
	names, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames")
	if len(names) != 2 || names[0] != "blog.apps.svc" {
		t.Errorf("dnsNames = %v", names)
	}
	if ref, _, _ := unstructured.NestedStringMap(cert.Object, "spec", "issuerRef"); ref["kind"] != "Issuer" {
		t.Errorf("kind should default to Issuer: %v", ref)
	}
	if s, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName"); s != "custom" {
		t.Errorf("secretName = %s", s)
	}
}

func TestCertManagerMissing(t *testing.T) {
	inst := publicInstance()
	inst.Spec.TLS.IssuerRef = &cyphrav1.IssuerRef{Name: "letsencrypt"}
	h := newHarness(t, inst)
	// Without the cert-manager CRDs, the API server's RESTMapper has no Certificate kind.
	h.r.Client = interceptor.NewClient(h.c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if obj.GetObjectKind().GroupVersionKind() == CertificateGVK {
				return &meta.NoKindMatchError{GroupKind: CertificateGVK.GroupKind(), SearchedVersions: []string{"v1"}}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	h.reconcile()
	h.setReady(1)
	h.reconcile()
	got := h.instance()
	c := meta.FindStatusCondition(got.Status.Conditions, cyphrav1.ConditionCertificateReady)
	if c == nil || c.Reason != "CertManagerNotInstalled" || got.Status.Phase == cyphrav1.PhaseReady {
		t.Fatalf("expected CertManagerNotInstalled and not Ready: %s %v", got.Status.Phase, got.Status.Conditions)
	}
}

// --- storage usage and metrics ---------------------------------------------------

const summaryJSON = `{"node":{"nodeName":"node-a"},"pods":[
 {"podRef":{"name":"other-0","namespace":"apps"},"volume":[{"name":"data","usedBytes":1,"capacityBytes":2,"pvcRef":{"name":"data-other-0","namespace":"apps"}}]},
 {"podRef":{"name":"blog-0","namespace":"apps"},"volume":[
   {"name":"kube-api-access","usedBytes":12288,"capacityBytes":100},
   {"name":"data","usedBytes":524288000,"capacityBytes":10434699264,"pvcRef":{"name":"data-blog-0","namespace":"apps"}}]}]}`

func TestVolumeUsage(t *testing.T) {
	var s StatsSummary
	if err := json.Unmarshal([]byte(summaryJSON), &s); err != nil {
		t.Fatal(err)
	}
	used, capacity, ok := VolumeUsage(&s, newInstance())
	if !ok || used != 524288000 || capacity != 10434699264 {
		t.Fatalf("usage = %d/%d %v", used, capacity, ok)
	}
	other := newInstance()
	other.Name = "missing"
	if _, _, ok := VolumeUsage(&s, other); ok {
		t.Error("unexpected usage for an unknown pod")
	}
}

func storageObjects(expandable bool) []client.Object {
	return []client.Object{
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "fast"}, Provisioner: "csi.example.com",
			AllowVolumeExpansion: ptr.To(expandable)},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "data-blog-0", Namespace: "apps"},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: ptr.To("fast"),
				Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("10Gi")}},
			},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("10Gi")}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "blog-0", Namespace: "apps"},
			Spec:       corev1.PodSpec{NodeName: "node-a"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		},
	}
}

func TestStorageUsageAndMetrics(t *testing.T) {
	inst := newInstance()
	last := metav1.NewTime(time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC))
	inst.Status.Backup.LastSuccessfulTime = &last
	h := newHarness(t, inst, storageObjects(true)...)
	calls := 0
	h.r.StatsSummary = func(_ context.Context, node string) (*StatsSummary, error) {
		calls++
		if node != "node-a" {
			t.Errorf("node = %s", node)
		}
		var s StatsSummary
		return &s, json.Unmarshal([]byte(summaryJSON), &s)
	}
	h.reconcile()
	h.setReady(1)
	res := h.reconcile()

	st := h.instance().Status.Storage
	if st.UsedBytes != 524288000 || st.CapacityBytes != 10*1024*1024*1024 || st.LastUpdateTime == nil {
		t.Fatalf("status.storage = %+v", st)
	}
	if res.RequeueAfter != DefaultUsageInterval {
		t.Errorf("requeue = %v, want %v", res.RequeueAfter, DefaultUsageInterval)
	}

	// Within the interval, usage is not measured again.
	h.now = h.now.Add(time.Minute)
	n := calls
	res = h.reconcile()
	if calls != n {
		t.Error("usage measured again before the interval elapsed")
	}
	if res.RequeueAfter != 4*time.Minute {
		t.Errorf("requeue = %v, want the rest of the interval (4m)", res.RequeueAfter)
	}
	h.now = h.now.Add(5 * time.Minute)
	h.reconcile()
	if calls != n+1 {
		t.Error("usage not refreshed after the interval")
	}

	if v := gaugeValue(t, storageUsedGauge, "apps", "blog"); v != 524288000 {
		t.Errorf("used gauge = %v", v)
	}
	if v := gaugeValue(t, storageCapacityGauge, "apps", "blog"); v != 10*1024*1024*1024 {
		t.Errorf("capacity gauge = %v", v)
	}
	if v := gaugeValue(t, lastBackupGauge, "apps", "blog"); v != float64(last.Unix()) {
		t.Errorf("backup gauge = %v", v)
	}
	if gaugeValue(t, phaseGauge, "apps", "blog", "Ready") != 1 || gaugeValue(t, phaseGauge, "apps", "blog", "Provisioning") != 0 {
		t.Error("phase gauge should be 1 for Ready only")
	}

	// Deleting the instance removes its series.
	if err := h.c.Delete(context.Background(), h.instance()); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	for _, g := range []*prometheus.GaugeVec{phaseGauge, storageUsedGauge, storageCapacityGauge, lastBackupGauge} {
		if n := seriesFor(g, "apps", "blog"); n != 0 {
			t.Errorf("%d series left after deletion", n)
		}
	}
}

func TestStorageUsageKeptWhileStopped(t *testing.T) {
	inst := newInstance()
	inst.Spec.Stopped = true
	before := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	inst.Status.Storage = cyphrav1.StorageStatus{UsedBytes: 42, LastUpdateTime: &before}
	objs := storageObjects(true)
	objs[2].(*corev1.Pod).Status.Phase = corev1.PodSucceeded
	h := newHarness(t, inst, objs...)
	h.r.StatsSummary = func(context.Context, string) (*StatsSummary, error) {
		t.Error("must not query the kubelet for a pod that is not running")
		return nil, nil
	}
	h.reconcile()
	if st := h.instance().Status.Storage; st.UsedBytes != 42 {
		t.Errorf("last usage lost: %+v", st)
	}
}

// --- volume expansion ------------------------------------------------------------

func (h *harness) pvc() *corev1.PersistentVolumeClaim {
	h.t.Helper()
	var pvc corev1.PersistentVolumeClaim
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "data-blog-0"}, &pvc); err != nil {
		h.t.Fatal(err)
	}
	return &pvc
}

func TestVolumeExpansion(t *testing.T) {
	inst := newInstance()
	inst.Spec.Storage.Size = resource.MustParse("10Gi")
	h := newHarness(t, inst, storageObjects(true)...)
	h.reconcile()
	if meta.FindStatusCondition(h.instance().Status.Conditions, cyphrav1.ConditionStorageResizeFailed) != nil {
		t.Fatal("no storage condition expected while the size is unchanged")
	}

	got := h.instance()
	got.Spec.Storage.Size = resource.MustParse("20Gi")
	if err := h.c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	if req := h.pvc().Spec.Resources.Requests[corev1.ResourceStorage]; req.String() != "20Gi" {
		t.Fatalf("PVC request = %s, want 20Gi", req.String())
	}
	c := meta.FindStatusCondition(h.instance().Status.Conditions, cyphrav1.ConditionStorageResizeFailed)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "Expanding" {
		t.Fatalf("condition = %+v", c)
	}

	// The storage provider grows the volume.
	pvc := h.pvc()
	pvc.Status.Capacity[corev1.ResourceStorage] = resource.MustParse("20Gi")
	if err := h.c.Status().Update(context.Background(), pvc); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	got = h.instance()
	if c := meta.FindStatusCondition(got.Status.Conditions, cyphrav1.ConditionStorageResizeFailed); c.Reason != "InSync" {
		t.Errorf("reason = %s", c.Reason)
	}
	if got.Status.Storage.CapacityBytes != 20*1024*1024*1024 {
		t.Errorf("capacity = %d", got.Status.Storage.CapacityBytes)
	}

	// Shrinking is rejected and the volume is left alone.
	got.Spec.Storage.Size = resource.MustParse("5Gi")
	if err := h.c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	c = meta.FindStatusCondition(h.instance().Status.Conditions, cyphrav1.ConditionStorageResizeFailed)
	if c.Status != metav1.ConditionTrue || c.Reason != "ShrinkNotSupported" {
		t.Errorf("condition = %+v", c)
	}
	if req := h.pvc().Spec.Resources.Requests[corev1.ResourceStorage]; req.String() != "20Gi" {
		t.Errorf("PVC shrunk to %s", req.String())
	}
}

func TestVolumeExpansionNotSupported(t *testing.T) {
	inst := newInstance()
	inst.Spec.Storage.Size = resource.MustParse("20Gi")
	h := newHarness(t, inst, storageObjects(false)...)
	h.reconcile()
	c := meta.FindStatusCondition(h.instance().Status.Conditions, cyphrav1.ConditionStorageResizeFailed)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "ExpansionNotSupported" {
		t.Fatalf("condition = %+v", c)
	}
	if req := h.pvc().Spec.Resources.Requests[corev1.ResourceStorage]; req.String() != "10Gi" {
		t.Errorf("PVC changed to %s", req.String())
	}
}

func TestTLSHashRollsPodOnRenewal(t *testing.T) {
	s := &corev1.Secret{Data: map[string][]byte{corev1.TLSCertKey: []byte("cert-1"), corev1.TLSPrivateKeyKey: []byte("key")}}
	h1 := TLSHash(s)
	if len(h1) != 16 || h1 != TLSHash(s) {
		t.Fatalf("hash must be stable and 16 chars, got %q", h1)
	}
	s.Data[corev1.TLSCertKey] = []byte("cert-2")
	if TLSHash(s) == h1 {
		t.Fatal("a renewed certificate must change the hash")
	}
	var tpl corev1.PodTemplateSpec
	SetTLSHash(&tpl, "")
	if tpl.Annotations != nil {
		t.Fatal("no TLS: no annotation")
	}
	SetTLSHash(&tpl, h1)
	if tpl.Annotations[TLSHashAnnotation] != h1 {
		t.Fatal("hash not set on pod template")
	}
}
