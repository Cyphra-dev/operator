package controller

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cyphrav1 "github.com/Cyphra-dev/operator/api/v1alpha1"
)

// BackupRequestAnnotation triggers an on-demand backup when its value changes
// (e.g. `kubectl annotate neo4jinstance blog cyphra.dev/backup-requested-at="$(date -u +%FT%TZ)" --overwrite`).
const BackupRequestAnnotation = "cyphra.dev/backup-requested-at"

// handledRequestAnnotation remembers the last on-demand request that was served.
const handledRequestAnnotation = "cyphra.dev/backup-request-handled"

// Neo4jInstanceReconciler reconciles Neo4jInstance objects.
type Neo4jInstanceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	// Now is overridable in tests.
	Now func() time.Time
	// StatsSummary reads kubelet volume stats for status.storage.usedBytes; nil disables it.
	StatsSummary StatsSummaryFunc
	// UsageInterval is how often volume usage is measured (default DefaultUsageInterval).
	UsageInterval time.Duration
}

// +kubebuilder:rbac:groups=cyphra.dev,resources=neo4jinstances,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=cyphra.dev,resources=neo4jinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cyphra.dev,resources=neo4jinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=nodes/proxy,verbs=get
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;create;update;patch

func (r *Neo4jInstanceReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Neo4jInstanceReconciler) event(inst *cyphrav1.Neo4jInstance, kind, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(inst, nil, kind, reason, reason, "%s", msg)
	}
}

// Reconcile drives an instance towards its spec.
func (r *Neo4jInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var inst cyphrav1.Neo4jInstance
	if err := r.Get(ctx, req.NamespacedName, &inst); err != nil {
		if apierrors.IsNotFound(err) {
			deleteMetrics(req.Namespace, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !inst.DeletionTimestamp.IsZero() {
		deleteMetrics(inst.Namespace, inst.Name)
		return ctrl.Result{}, nil // owned objects are garbage-collected
	}

	if err := r.ensurePasswordSecret(ctx, &inst); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureServices(ctx, &inst); err != nil {
		return ctrl.Result{}, err
	}
	certReady, err := r.ensureCertificate(ctx, &inst)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("certificate: %w", err)
	}

	// Decide whether a backup should be running before shaping the StatefulSet, using the live
	// readiness of the current StatefulSet (the phase in status may be one reconcile behind).
	var current appsv1.StatefulSet
	ready := false
	if err := r.Get(ctx, req.NamespacedName, &current); err == nil {
		ready = stsReady(&current)
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	requeue, err := r.reconcileBackup(ctx, &inst, ready)
	if err != nil {
		return ctrl.Result{}, err
	}

	replicas := int32(1)
	if inst.Status.Backup.ActiveJob != "" || inst.Spec.Stopped {
		replicas = 0
	}
	tlsHash, err := r.tlsHash(ctx, &inst)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("tls secret: %w", err)
	}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: inst.Name, Namespace: inst.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		desired := StatefulSetSpec(&inst, replicas)
		SetTLSHash(&desired.Template, tlsHash)
		if sts.CreationTimestamp.IsZero() {
			sts.Spec = desired
		} else {
			// Selector, serviceName and volumeClaimTemplates are immutable.
			sts.Spec.Replicas = desired.Replicas
			sts.Spec.Template = desired.Template
			sts.Spec.PersistentVolumeClaimRetentionPolicy = desired.PersistentVolumeClaimRetentionPolicy
		}
		sts.Labels = Labels(&inst)
		return controllerutil.SetControllerReference(&inst, sts, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("statefulset: %w", err)
	}

	usageRequeue, err := r.reconcileStorage(ctx, &inst)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("storage: %w", err)
	}
	requeue = minRequeue(requeue, usageRequeue)
	if !certReady {
		requeue = minRequeue(requeue, 15*time.Second)
	}

	if err := r.updateStatus(ctx, &inst, sts, certReady); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	recordMetrics(&inst)
	log.V(1).Info("reconciled", "phase", inst.Status.Phase)
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// minRequeue returns the shorter non-zero delay (0 means "no requeue").
func minRequeue(a, b time.Duration) time.Duration {
	if a == 0 || (b != 0 && b < a) {
		return b
	}
	return a
}

func randomPassword() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ensurePasswordSecret creates `<name>-auth` once; it is never rotated by the operator.
func (r *Neo4jInstanceReconciler) ensurePasswordSecret(ctx context.Context, inst *cyphrav1.Neo4jInstance) error {
	if inst.Spec.PasswordSecretRef != nil {
		return nil
	}
	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: inst.Namespace, Name: authSecretName(inst)}, &existing)
	if err == nil {
		// Keep `uri` current (e.g. when a public hostname is added); the password is never rotated.
		if string(existing.Data["uri"]) == Endpoint(inst) {
			return nil
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data["uri"] = []byte(Endpoint(inst))
		return r.Update(ctx, &existing)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	pw, err := randomPassword()
	if err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: authSecretName(inst), Namespace: inst.Namespace, Labels: Labels(inst)},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"username": "neo4j",
			"password": pw,
			"uri":      Endpoint(inst),
		},
	}
	if err := controllerutil.SetControllerReference(inst, secret, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, secret)
}

func (r *Neo4jInstanceReconciler) ensureServices(ctx context.Context, inst *cyphrav1.Neo4jInstance) error {
	headless := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: headlessServiceName(inst), Namespace: inst.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, headless, func() error {
		headless.Labels = Labels(inst)
		headless.Spec.ClusterIP = corev1.ClusterIPNone
		headless.Spec.Selector = SelectorLabels(inst)
		headless.Spec.PublishNotReadyAddresses = true
		headless.Spec.Ports = ServicePorts(inst)
		return controllerutil.SetControllerReference(inst, headless, r.Scheme)
	}); err != nil {
		return fmt.Errorf("headless service: %w", err)
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: inst.Name, Namespace: inst.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = Labels(inst)
		svc.Annotations = ServiceAnnotations(inst)
		t := ServiceType(inst)
		svc.Spec.Type = t
		svc.Spec.Selector = SelectorLabels(inst)
		// Keep allocated node ports stable across updates.
		desired := ServicePorts(inst)
		for i := range desired {
			for _, cur := range svc.Spec.Ports {
				if cur.Name == desired[i].Name && t != corev1.ServiceTypeClusterIP {
					desired[i].NodePort = cur.NodePort
				}
			}
		}
		svc.Spec.Ports = desired
		return controllerutil.SetControllerReference(inst, svc, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	return nil
}

func (r *Neo4jInstanceReconciler) setCertCondition(inst *cyphrav1.Neo4jInstance, ready bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&inst.Status.Conditions, metav1.Condition{
		Type: cyphrav1.ConditionCertificateReady, Status: status,
		Reason: reason, Message: msg, ObservedGeneration: inst.Generation,
	})
}

// ensureCertificate requests the TLS certificate from cert-manager when spec.tls.issuerRef is set,
// and reports whether it has been issued. Without an issuerRef the TLS Secret is the user's.
func (r *Neo4jInstanceReconciler) ensureCertificate(ctx context.Context, inst *cyphrav1.Neo4jInstance) (bool, error) {
	if inst.Spec.TLS == nil || inst.Spec.TLS.IssuerRef == nil {
		meta.RemoveStatusCondition(&inst.Status.Conditions, cyphrav1.ConditionCertificateReady)
		return true, nil
	}
	desired := Certificate(inst)
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(CertificateGVK)
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), cert)
	switch {
	case meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err):
		r.setCertCondition(inst, false, "CertManagerNotInstalled",
			"tls.issuerRef needs cert-manager (the cert-manager.io/v1 Certificate kind is not available)")
		return false, nil
	case apierrors.IsNotFound(err):
		if err := controllerutil.SetControllerReference(inst, desired, r.Scheme); err != nil {
			return false, err
		}
		if err := r.Create(ctx, desired); err != nil {
			return false, err
		}
		r.event(inst, corev1.EventTypeNormal, "CertificateRequested", "requested a certificate for "+TLSSecretName(inst))
		r.setCertCondition(inst, false, "Issuing", "waiting for cert-manager to issue the certificate")
		return false, nil
	case err != nil:
		return false, err
	}

	if !equality.Semantic.DeepEqual(cert.Object["spec"], desired.Object["spec"]) ||
		!equality.Semantic.DeepEqual(cert.GetLabels(), desired.GetLabels()) {
		cert.Object["spec"] = desired.Object["spec"]
		cert.SetLabels(desired.GetLabels())
		if err := r.Update(ctx, cert); err != nil {
			return false, err
		}
	}
	ready, msg := CertificateReady(cert)
	if ready {
		r.setCertCondition(inst, true, "Issued", "certificate issued into Secret "+TLSSecretName(inst))
	} else {
		r.setCertCondition(inst, false, "Issuing", msg)
	}
	return ready, nil
}

// ---------------------------------------------------------------------------
// Backups
// ---------------------------------------------------------------------------

// NextBackupTime returns the next scheduled time strictly after `after`.
func NextBackupTime(schedule string, after time.Time) (time.Time, error) {
	sched, err := cron.ParseStandard(schedule)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(after.UTC()), nil
}

// backupDue reports whether a backup should start now, and when to check again otherwise.
func (r *Neo4jInstanceReconciler) backupDue(inst *cyphrav1.Neo4jInstance) (due bool, onDemand bool, wait time.Duration, err error) {
	b := inst.Spec.Backup
	now := r.now()
	if req := inst.Annotations[BackupRequestAnnotation]; req != "" && req != inst.Annotations[handledRequestAnnotation] {
		return true, true, 0, nil
	}
	if b.Suspend {
		return false, false, 0, nil
	}
	last := inst.CreationTimestamp.Time
	if inst.Status.Backup.LastScheduleTime != nil {
		last = inst.Status.Backup.LastScheduleTime.Time
	}
	next, err := NextBackupTime(b.Schedule, last)
	if err != nil {
		return false, false, 0, err
	}
	if !now.Before(next) {
		return true, false, 0, nil
	}
	return false, false, next.Sub(now), nil
}

// reconcileBackup starts, advances and finishes backups. It mutates inst.Status.Backup (persisted
// by updateStatus) and returns when to requeue.
func (r *Neo4jInstanceReconciler) reconcileBackup(ctx context.Context, inst *cyphrav1.Neo4jInstance, ready bool) (time.Duration, error) {
	b := inst.Spec.Backup
	st := &inst.Status.Backup

	if st.ActiveJob == "" {
		if b == nil {
			return 0, nil
		}
		due, onDemand, wait, err := r.backupDue(inst)
		if err != nil {
			meta.SetStatusCondition(&inst.Status.Conditions, metav1.Condition{
				Type: cyphrav1.ConditionBackupFailed, Status: metav1.ConditionTrue,
				Reason: "InvalidSchedule", Message: err.Error(), ObservedGeneration: inst.Generation,
			})
			return 0, nil
		}
		if !due {
			return wait, nil
		}
		// Only back up a healthy instance (and never while it is still being provisioned).
		if !ready {
			return 30 * time.Second, nil
		}
		now := metav1.NewTime(r.now())
		st.ActiveJob = fmt.Sprintf("%s-backup-%d", inst.Name, now.Unix())
		if !onDemand {
			st.LastScheduleTime = &now
		} else if err := r.markRequestHandled(ctx, inst); err != nil {
			return 0, err
		}
		r.event(inst, corev1.EventTypeNormal, "BackupStarted", "stopping the instance to take a consistent dump")
		// Persist the decision before scaling down, so a restart resumes the same backup.
		if err := r.Status().Update(ctx, inst); err != nil {
			return 0, err
		}
		return 5 * time.Second, nil
	}

	// A backup is active: wait for the pod to be gone, then run / watch the Job.
	var job batchv1.Job
	err := r.Get(ctx, types.NamespacedName{Namespace: inst.Namespace, Name: st.ActiveJob}, &job)
	if apierrors.IsNotFound(err) {
		if b == nil {
			st.ActiveJob = "" // backup removed from spec mid-flight
			return 0, nil
		}
		stopped, err := r.podStopped(ctx, inst)
		if err != nil || !stopped {
			return 5 * time.Second, err
		}
		key := fmt.Sprintf("%s/%s-%s.dump", BackupPrefix(inst), inst.Name, r.now().UTC().Format("20060102T150405Z"))
		j := BackupJob(inst, st.ActiveJob, key)
		j.Annotations = map[string]string{"cyphra.dev/backup-key": key}
		if err := controllerutil.SetControllerReference(inst, j, r.Scheme); err != nil {
			return 0, err
		}
		if err := r.Create(ctx, j); err != nil && !apierrors.IsAlreadyExists(err) {
			return 0, err
		}
		return 5 * time.Second, nil
	}
	if err != nil {
		return 0, err
	}

	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			now := metav1.NewTime(r.now())
			st.LastSuccessfulTime = &now
			st.LastSuccessfulKey = job.Annotations["cyphra.dev/backup-key"]
			st.ActiveJob = ""
			meta.SetStatusCondition(&inst.Status.Conditions, metav1.Condition{
				Type: cyphrav1.ConditionBackupFailed, Status: metav1.ConditionFalse,
				Reason: "BackupSucceeded", Message: "uploaded " + st.LastSuccessfulKey, ObservedGeneration: inst.Generation,
			})
			r.event(inst, corev1.EventTypeNormal, "BackupSucceeded", "uploaded "+st.LastSuccessfulKey)
			return time.Second, nil
		case batchv1.JobFailed:
			st.ActiveJob = ""
			meta.SetStatusCondition(&inst.Status.Conditions, metav1.Condition{
				Type: cyphrav1.ConditionBackupFailed, Status: metav1.ConditionTrue,
				Reason: "JobFailed", Message: fmt.Sprintf("backup job %s failed: %s", job.Name, c.Message),
				ObservedGeneration: inst.Generation,
			})
			r.event(inst, corev1.EventTypeWarning, "BackupFailed", "job "+job.Name+" failed; see its logs")
			return time.Second, nil
		}
	}
	return 10 * time.Second, nil
}

func (r *Neo4jInstanceReconciler) markRequestHandled(ctx context.Context, inst *cyphrav1.Neo4jInstance) error {
	patch := client.MergeFrom(inst.DeepCopy())
	if inst.Annotations == nil {
		inst.Annotations = map[string]string{}
	}
	inst.Annotations[handledRequestAnnotation] = inst.Annotations[BackupRequestAnnotation]
	status := inst.Status.DeepCopy()
	if err := r.Patch(ctx, inst, patch); err != nil {
		return err
	}
	inst.Status = *status // Patch refreshes inst from the server; keep the in-memory status decision
	return nil
}

func (r *Neo4jInstanceReconciler) podStopped(ctx context.Context, inst *cyphrav1.Neo4jInstance) (bool, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(inst.Namespace), client.MatchingLabels(SelectorLabels(inst))); err != nil {
		return false, err
	}
	return len(pods.Items) == 0, nil
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

func (r *Neo4jInstanceReconciler) updateStatus(ctx context.Context, inst *cyphrav1.Neo4jInstance, sts *appsv1.StatefulSet, certReady bool) error {
	ref := PasswordRef(inst)
	inst.Status.Endpoint = Endpoint(inst)
	inst.Status.PasswordSecret = ref.Name + "/" + ref.Key
	inst.Status.ObservedGeneration = inst.Generation

	ready := stsReady(sts)
	switch {
	case inst.Status.Backup.ActiveJob != "":
		inst.Status.Phase = cyphrav1.PhaseBackingUp
	case inst.Spec.Stopped:
		inst.Status.Phase = cyphrav1.PhaseStopped
	case ready && certReady:
		inst.Status.Phase = cyphrav1.PhaseReady
	default:
		inst.Status.Phase = cyphrav1.PhaseProvisioning
	}
	cond := metav1.Condition{
		Type: cyphrav1.ConditionReady, Status: metav1.ConditionFalse,
		Reason: string(inst.Status.Phase), Message: "Neo4j is not accepting connections yet",
		ObservedGeneration: inst.Generation,
	}
	if inst.Status.Phase == cyphrav1.PhaseReady {
		cond.Status, cond.Message = metav1.ConditionTrue, "Neo4j accepts Bolt connections"
	}
	if inst.Status.Phase == cyphrav1.PhaseProvisioning && !certReady {
		cond.Message = "waiting for the TLS certificate"
	}
	if inst.Status.Phase == cyphrav1.PhaseBackingUp {
		cond.Message = "stopped for a backup"
	}
	if inst.Status.Phase == cyphrav1.PhaseStopped {
		cond.Message = "stopped (spec.stopped is true)"
	}
	meta.SetStatusCondition(&inst.Status.Conditions, cond)
	return r.Status().Update(ctx, inst)
}

func stsReady(sts *appsv1.StatefulSet) bool {
	return sts.Status.ReadyReplicas >= 1 && sts.Status.ObservedGeneration >= sts.Generation
}

// SetupWithManager registers the controller.
func (r *Neo4jInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cyphrav1.Neo4jInstance{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&batchv1.Job{}).
		// TLS Secrets are usually not ours (cert-manager or the user creates them); a renewal must
		// roll the pod because Neo4j only loads certificates at startup.
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.instancesForTLSSecret)).
		Named("neo4jinstance").
		Complete(r)
}

// tlsHash fingerprints the instance's TLS Secret ("" without TLS or before the Secret exists).
func (r *Neo4jInstanceReconciler) tlsHash(ctx context.Context, inst *cyphrav1.Neo4jInstance) (string, error) {
	if inst.Spec.TLS == nil {
		return "", nil
	}
	var secret corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: inst.Namespace, Name: TLSSecretName(inst)}, &secret)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return TLSHash(&secret), nil
}

// instancesForTLSSecret maps a Secret to the instances in its namespace that use it for TLS.
func (r *Neo4jInstanceReconciler) instancesForTLSSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	var list cyphrav1.Neo4jInstanceList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		inst := &list.Items[i]
		if inst.Spec.TLS != nil && TLSSecretName(inst) == obj.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: inst.Namespace, Name: inst.Name}})
		}
	}
	return reqs
}
