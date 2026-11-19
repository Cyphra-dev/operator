package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cyphrav1 "github.com/Cyphra-dev/operator/api/v1alpha1"
)

func newInstance() *cyphrav1.Neo4jInstance {
	return &cyphrav1.Neo4jInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name: "blog", Namespace: "apps", Generation: 1,
			CreationTimestamp: metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		},
		Spec: cyphrav1.Neo4jInstanceSpec{Version: "5.26"},
	}
}

func envMap(c corev1.Container) map[string]corev1.EnvVar {
	m := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		m[e.Name] = e
	}
	return m
}

func TestSettingEnv(t *testing.T) {
	cases := map[string]string{
		"server.memory.heap.max_size": "NEO4J_server_memory_heap_max__size",
		"db.transaction.timeout":      "NEO4J_db_transaction_timeout",
	}
	for in, want := range cases {
		if got := SettingEnv(in); got != want {
			t.Errorf("SettingEnv(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStatefulSetDefaults(t *testing.T) {
	inst := newInstance()
	inst.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}
	inst.Spec.Config = map[string]string{"server.memory.pagecache.size": "100m"}
	spec := StatefulSetSpec(inst, 1)

	c := spec.Template.Spec.Containers[0]
	if c.Image != "neo4j:5.26-community" {
		t.Fatalf("image = %s", c.Image)
	}
	env := envMap(c)
	if env["NEO4J_AUTH"].Value != "neo4j/$(CYPHRA_NEO4J_PASSWORD)" {
		t.Errorf("NEO4J_AUTH = %q", env["NEO4J_AUTH"].Value)
	}
	for name := range env {
		if strings.HasPrefix(name, "NEO4J_") && name != "NEO4J_AUTH" && !strings.HasPrefix(name, "NEO4J_server_") &&
			!strings.HasPrefix(name, "NEO4J_dbms_") && !strings.HasPrefix(name, "NEO4J_db_") {
			t.Errorf("%s would be read by the image as a Neo4j setting", name)
		}
	}
	if ref := env["CYPHRA_NEO4J_PASSWORD"].ValueFrom.SecretKeyRef; ref.Name != "blog-auth" || ref.Key != "password" {
		t.Errorf("password ref = %+v", ref)
	}
	if got := env["NEO4J_server_memory_heap_max__size"].Value; got != "819m" {
		t.Errorf("heap = %s, want 819m (40%% of 2Gi)", got)
	}
	if got := env["NEO4J_server_memory_pagecache_size"].Value; got != "100m" {
		t.Errorf("user config must override computed settings, got pagecache %s", got)
	}
	if got := spec.VolumeClaimTemplates[0].Spec.Resources.Requests.Storage().String(); got != "10Gi" {
		t.Errorf("default storage = %s", got)
	}
	if *spec.Template.Spec.SecurityContext.RunAsUser != neo4jUID {
		t.Error("pod must run as the neo4j user")
	}
}

func TestTLSSettings(t *testing.T) {
	inst := newInstance()
	inst.Spec.TLS = &cyphrav1.TLSSpec{SecretName: "blog-tls"}
	s := Settings(inst)
	if s["server.bolt.tls_level"] != "REQUIRED" || s["dbms.ssl.policy.bolt.enabled"] != "true" {
		t.Fatalf("TLS settings missing: %v", s)
	}
	if Endpoint(inst) != "neo4j+s://blog.apps.svc:7687" {
		t.Errorf("endpoint = %s", Endpoint(inst))
	}
	spec := StatefulSetSpec(inst, 1)
	if spec.Template.Spec.Volumes[0].Secret.SecretName != "blog-tls" {
		t.Error("TLS secret not mounted")
	}
	if ports := ServicePorts(inst); ports[1].Name != "https" {
		t.Errorf("service should expose https, got %v", ports)
	}
}

func TestBackupJob(t *testing.T) {
	inst := newInstance()
	inst.Spec.Backup = &cyphrav1.BackupSpec{
		Schedule: "0 3 * * *", RetentionDays: 7,
		S3: cyphrav1.S3Spec{Bucket: "dumps", Endpoint: "http://minio:9000", CredentialsSecretName: "s3"},
	}
	job := BackupJob(inst, "blog-backup-1", "apps/blog/blog-x.dump")
	dump := job.Spec.Template.Spec.InitContainers[0]
	if dump.Image != Image(inst) || strings.Join(dump.Command, " ") !=
		"neo4j-admin database dump neo4j --to-path=/backup --overwrite-destination=true" {
		t.Errorf("dump container = %v %v", dump.Image, dump.Command)
	}
	if claim := job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName; claim != "data-blog-0" {
		t.Errorf("claim = %s", claim)
	}
	env := envMap(job.Spec.Template.Spec.Containers[0])
	if env["RCLONE_S3_PROVIDER"].Value != "Other" || env["RCLONE_S3_ENDPOINT"].Value != "http://minio:9000" {
		t.Errorf("s3 env = %v", env)
	}
	if *job.Spec.BackoffLimit != 0 {
		t.Error("backup jobs must not retry while the instance is down")
	}
}

func TestNextBackupTime(t *testing.T) {
	next, err := NextBackupTime("0 3 * * *", time.Date(2026, 1, 1, 4, 0, 0, 0, time.UTC))
	if err != nil || !next.Equal(time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("next = %v, %v", next, err)
	}
	if _, err := NextBackupTime("nonsense", time.Now()); err == nil {
		t.Fatal("expected an error for an invalid schedule")
	}
}

// --- reconcile with a fake client ------------------------------------------------

type harness struct {
	t   *testing.T
	c   client.Client
	r   *Neo4jInstanceReconciler
	now time.Time
}

func newHarness(t *testing.T, inst *cyphrav1.Neo4jInstance, objs ...client.Object) *harness {
	return newHarnessWithScheme(t, newScheme(), inst, objs...)
}

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = cyphrav1.AddToScheme(s)
	return s
}

func newHarnessWithScheme(t *testing.T, s *runtime.Scheme, inst *cyphrav1.Neo4jInstance, objs ...client.Object) *harness {
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(append([]client.Object{inst}, objs...)...).
		WithStatusSubresource(&cyphrav1.Neo4jInstance{}, &appsv1.StatefulSet{}, &batchv1.Job{}).
		Build()
	h := &harness{t: t, c: c, now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	h.r = &Neo4jInstanceReconciler{Client: c, Scheme: s, Now: func() time.Time { return h.now }}
	return h
}

func (h *harness) reconcile() ctrl.Result {
	h.t.Helper()
	res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "apps", Name: "blog"}})
	if err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (h *harness) instance() *cyphrav1.Neo4jInstance {
	var inst cyphrav1.Neo4jInstance
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "blog"}, &inst); err != nil {
		h.t.Fatal(err)
	}
	return &inst
}

func (h *harness) sts() *appsv1.StatefulSet {
	var sts appsv1.StatefulSet
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "blog"}, &sts); err != nil {
		h.t.Fatal(err)
	}
	return &sts
}

// markReady simulates the StatefulSet controller and kubelet.
func (h *harness) setReady(n int32) {
	sts := h.sts()
	sts.Status.ObservedGeneration = sts.Generation
	sts.Status.ReadyReplicas = n
	sts.Status.Replicas = n
	if err := h.c.Status().Update(context.Background(), sts); err != nil {
		h.t.Fatal(err)
	}
}

func TestReconcileProvisionsAndBecomesReady(t *testing.T) {
	h := newHarness(t, newInstance())
	h.reconcile()

	var secret corev1.Secret
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "blog-auth"}, &secret); err != nil {
		t.Fatalf("auth secret: %v", err)
	}
	if len(secret.StringData["password"]) < 20 {
		t.Error("generated password too short")
	}
	for _, name := range []string{"blog", "blog-headless"} {
		var svc corev1.Service
		if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: name}, &svc); err != nil {
			t.Fatalf("service %s: %v", name, err)
		}
	}
	if got := h.instance().Status.Phase; got != cyphrav1.PhaseProvisioning {
		t.Fatalf("phase = %s", got)
	}

	h.setReady(1)
	h.reconcile()
	inst := h.instance()
	if inst.Status.Phase != cyphrav1.PhaseReady || !meta.IsStatusConditionTrue(inst.Status.Conditions, cyphrav1.ConditionReady) {
		t.Fatalf("expected Ready, got %s %v", inst.Status.Phase, inst.Status.Conditions)
	}
	if inst.Status.Endpoint != "neo4j://blog.apps.svc:7687" || inst.Status.PasswordSecret != "blog-auth/password" {
		t.Errorf("status = %+v", inst.Status)
	}

	// The password is never regenerated.
	h.reconcile()
	var again corev1.Secret
	_ = h.c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "blog-auth"}, &again)
	if again.StringData["password"] != secret.StringData["password"] {
		t.Error("password changed on a later reconcile")
	}
}

func TestScheduledBackupLifecycle(t *testing.T) {
	inst := newInstance()
	inst.Spec.Backup = &cyphrav1.BackupSpec{
		Schedule: "0 3 * * *", RetentionDays: 7,
		S3: cyphrav1.S3Spec{Bucket: "dumps", CredentialsSecretName: "s3"},
	}
	h := newHarness(t, inst)
	h.reconcile()
	h.setReady(1)
	res := h.reconcile() // creation was 2026-01-01T00:00; first run is 03:00, already past at 12:00
	_ = res
	got := h.instance()
	if got.Status.Backup.ActiveJob == "" || got.Status.Phase != cyphrav1.PhaseBackingUp {
		t.Fatalf("backup should have started: %+v", got.Status)
	}
	if *h.sts().Spec.Replicas != 0 {
		t.Fatal("instance must be stopped for the dump")
	}

	// Pod still terminating: no job yet. (The fake client has no pods, so it proceeds immediately.)
	h.setReady(0)
	h.reconcile()
	var job batchv1.Job
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: got.Status.Backup.ActiveJob}, &job); err != nil {
		t.Fatalf("backup job not created: %v", err)
	}
	if !strings.HasPrefix(job.Annotations["cyphra.dev/backup-key"], "apps/blog/blog-20260101T120000Z") {
		t.Errorf("key = %s", job.Annotations["cyphra.dev/backup-key"])
	}

	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := h.c.Status().Update(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	got = h.instance()
	if got.Status.Backup.ActiveJob != "" || got.Status.Backup.LastSuccessfulKey == "" {
		t.Fatalf("backup should be recorded: %+v", got.Status.Backup)
	}
	if *h.sts().Spec.Replicas != 1 {
		t.Fatal("instance must be started again after the backup")
	}

	// Next run is tomorrow 03:00.
	h.setReady(1)
	res = h.reconcile()
	if res.RequeueAfter != 15*time.Hour {
		t.Errorf("requeue = %v, want 15h", res.RequeueAfter)
	}
}

func TestFailedBackupRestartsInstance(t *testing.T) {
	inst := newInstance()
	inst.Annotations = map[string]string{BackupRequestAnnotation: "now"}
	inst.Spec.Backup = &cyphrav1.BackupSpec{
		Schedule: "0 3 1 1 *", Suspend: true,
		S3: cyphrav1.S3Spec{Bucket: "dumps", CredentialsSecretName: "s3"},
	}
	h := newHarness(t, inst)
	h.reconcile()
	h.setReady(1)
	h.reconcile() // on-demand request starts a backup even while the schedule is suspended
	got := h.instance()
	if got.Status.Backup.ActiveJob == "" {
		t.Fatal("on-demand backup did not start")
	}
	if got.Annotations[handledRequestAnnotation] != "now" {
		t.Error("request not marked handled")
	}
	h.reconcile()
	var job batchv1.Job
	_ = h.c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: got.Status.Backup.ActiveJob}, &job)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "boom"}}
	_ = h.c.Status().Update(context.Background(), &job)
	h.reconcile()

	got = h.instance()
	if !meta.IsStatusConditionTrue(got.Status.Conditions, cyphrav1.ConditionBackupFailed) {
		t.Fatalf("BackupFailed condition missing: %v", got.Status.Conditions)
	}
	if *h.sts().Spec.Replicas != 1 {
		t.Fatal("instance must be restarted after a failed backup")
	}
	// The same request is not served twice.
	h.setReady(1)
	h.reconcile()
	if h.instance().Status.Backup.ActiveJob != "" {
		t.Fatal("handled request started another backup")
	}
}

func TestStoppedScalesToZero(t *testing.T) {
	inst := newInstance()
	inst.Spec.Stopped = true
	h := newHarness(t, inst)
	h.reconcile()
	if *h.sts().Spec.Replicas != 0 || h.instance().Status.Phase != cyphrav1.PhaseStopped {
		t.Fatalf("stopped instance: replicas=%d phase=%s", *h.sts().Spec.Replicas, h.instance().Status.Phase)
	}
}
