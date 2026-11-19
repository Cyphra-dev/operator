package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cyphrav1 "github.com/Cyphra-dev/operator/api/v1alpha1"
)

// DefaultUsageInterval is how often volume usage is measured.
const DefaultUsageInterval = 5 * time.Minute

// StatsSummary is the subset of the kubelet's /stats/summary the operator reads.
type StatsSummary struct {
	Pods []PodStats `json:"pods"`
}

// PodStats are a pod's entries in the kubelet stats summary.
type PodStats struct {
	PodRef  struct{ Name, Namespace string } `json:"podRef"`
	Volumes []VolumeStats                    `json:"volume"`
}

// VolumeStats is one volume of a pod in the kubelet stats summary.
type VolumeStats struct {
	Name          string  `json:"name"`
	UsedBytes     *uint64 `json:"usedBytes"`
	CapacityBytes *uint64 `json:"capacityBytes"`
	PVCRef        *struct {
		Name, Namespace string
	} `json:"pvcRef"`
}

// StatsSummaryFunc returns the kubelet stats summary of a node.
type StatsSummaryFunc func(ctx context.Context, node string) (*StatsSummary, error)

// KubeletStatsSummary reads the stats summary through the API server's node proxy
// (`GET /api/v1/nodes/{node}/proxy/stats/summary`, RBAC `nodes/proxy` get). It works with every
// volume type the kubelet measures, without exec'ing into the database pod.
func KubeletStatsSummary(cfg *rest.Config) (StatsSummaryFunc, error) {
	core, err := corev1client.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, node string) (*StatsSummary, error) {
		raw, err := core.RESTClient().Get().AbsPath("/api/v1/nodes", node, "proxy", "stats", "summary").DoRaw(ctx)
		if err != nil {
			return nil, err
		}
		var s StatsSummary
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("decoding stats summary of node %s: %w", node, err)
		}
		return &s, nil
	}, nil
}

// VolumeUsage finds the data volume of an instance in a stats summary.
func VolumeUsage(s *StatsSummary, inst *cyphrav1.Neo4jInstance) (used, capacity uint64, ok bool) {
	for _, p := range s.Pods {
		if p.PodRef.Namespace != inst.Namespace || p.PodRef.Name != PodName(inst) {
			continue
		}
		for _, v := range p.Volumes {
			if (v.PVCRef != nil && v.PVCRef.Name == PVCName(inst)) || (v.PVCRef == nil && v.Name == dataVolume) {
				if v.UsedBytes == nil {
					return 0, 0, false
				}
				if v.CapacityBytes != nil {
					capacity = *v.CapacityBytes
				}
				return *v.UsedBytes, capacity, true
			}
		}
	}
	return 0, 0, false
}

func (r *Neo4jInstanceReconciler) usageInterval() time.Duration {
	if r.UsageInterval > 0 {
		return r.UsageInterval
	}
	return DefaultUsageInterval
}

// reconcileStorage expands the data PVC when spec.storage.size grows and refreshes
// status.storage. It returns when usage should be measured next (0: not scheduled).
func (r *Neo4jInstanceReconciler) reconcileStorage(ctx context.Context, inst *cyphrav1.Neo4jInstance) (time.Duration, error) {
	var pvc corev1.PersistentVolumeClaim
	err := r.Get(ctx, types.NamespacedName{Namespace: inst.Namespace, Name: PVCName(inst)}, &pvc)
	if apierrors.IsNotFound(err) {
		return 0, nil // the StatefulSet has not created it yet
	}
	if err != nil {
		return 0, err
	}
	if err := r.expandVolume(ctx, inst, &pvc); err != nil {
		return 0, err
	}
	if c, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		inst.Status.Storage.CapacityBytes = c.Value()
	}
	return r.refreshUsage(ctx, inst), nil
}

func (r *Neo4jInstanceReconciler) setStorageCondition(inst *cyphrav1.Neo4jInstance, failed bool, reason, msg string) {
	status := metav1.ConditionFalse
	if failed {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&inst.Status.Conditions, metav1.Condition{
		Type: cyphrav1.ConditionStorageResizeFailed, Status: status,
		Reason: reason, Message: msg, ObservedGeneration: inst.Generation,
	})
}

// expandVolume applies a larger spec.storage.size to the PVC. StatefulSet volumeClaimTemplates are
// immutable, so the claim is patched directly; this needs a StorageClass with
// allowVolumeExpansion. Shrinking is not possible and is reported, not applied.
func (r *Neo4jInstanceReconciler) expandVolume(ctx context.Context, inst *cyphrav1.Neo4jInstance, pvc *corev1.PersistentVolumeClaim) error {
	desired := DesiredStorage(inst)
	requested := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	tracked := meta.FindStatusCondition(inst.Status.Conditions, cyphrav1.ConditionStorageResizeFailed) != nil

	switch cmp := desired.Cmp(requested); {
	case cmp < 0:
		r.setStorageCondition(inst, true, "ShrinkNotSupported", fmt.Sprintf(
			"storage.size %s is smaller than the volume (%s); volumes cannot shrink", desired.String(), requested.String()))
		return nil
	case cmp > 0:
		expandable, why, err := r.expandable(ctx, pvc)
		if err != nil {
			return err
		}
		if !expandable {
			r.setStorageCondition(inst, true, "ExpansionNotSupported", fmt.Sprintf(
				"cannot grow the volume from %s to %s: %s", requested.String(), desired.String(), why))
			return nil
		}
		patch := client.MergeFrom(pvc.DeepCopy())
		if pvc.Spec.Resources.Requests == nil {
			pvc.Spec.Resources.Requests = corev1.ResourceList{}
		}
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = desired
		if err := r.Patch(ctx, pvc, patch); err != nil {
			return fmt.Errorf("expanding %s: %w", pvc.Name, err)
		}
		r.event(inst, corev1.EventTypeNormal, "VolumeExpansion",
			fmt.Sprintf("requested %s for %s (was %s)", desired.String(), pvc.Name, requested.String()))
		r.setStorageCondition(inst, false, "Expanding", "volume expansion to "+desired.String()+" requested")
		return nil
	}

	if !tracked {
		return nil // nothing was ever resized: no condition noise
	}
	if c, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok && c.Cmp(desired) < 0 {
		msg := "waiting for the volume to grow to " + desired.String()
		for _, pc := range pvc.Status.Conditions {
			if pc.Type == corev1.PersistentVolumeClaimFileSystemResizePending && pc.Status == corev1.ConditionTrue {
				msg += " (file system resize pending: it completes when the pod restarts)"
			}
		}
		r.setStorageCondition(inst, false, "Expanding", msg)
		return nil
	}
	r.setStorageCondition(inst, false, "InSync", "volume size matches storage.size")
	return nil
}

func (r *Neo4jInstanceReconciler) expandable(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (bool, string, error) {
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return false, "the claim has no StorageClass", nil
	}
	var sc storagev1.StorageClass
	if err := r.Get(ctx, types.NamespacedName{Name: *pvc.Spec.StorageClassName}, &sc); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "StorageClass " + *pvc.Spec.StorageClassName + " not found", nil
		}
		return false, "", err
	}
	if sc.AllowVolumeExpansion == nil || !*sc.AllowVolumeExpansion {
		return false, "StorageClass " + sc.Name + " does not allow volume expansion", nil
	}
	return true, "", nil
}

// refreshUsage measures the data volume when the last measurement is older than the usage
// interval and the pod is running. Failures are logged and retried on the next interval; the
// last known value is kept.
func (r *Neo4jInstanceReconciler) refreshUsage(ctx context.Context, inst *cyphrav1.Neo4jInstance) time.Duration {
	if r.StatsSummary == nil {
		return 0
	}
	interval := r.usageInterval()
	st := &inst.Status.Storage
	if st.LastUpdateTime != nil {
		if age := r.now().Sub(st.LastUpdateTime.Time); age < interval {
			return interval - age
		}
	}

	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: inst.Namespace, Name: PodName(inst)}, &pod); err != nil ||
		pod.Spec.NodeName == "" || pod.Status.Phase != corev1.PodRunning {
		return interval // stopped or not scheduled yet: keep the last value
	}
	summary, err := r.StatsSummary(ctx, pod.Spec.NodeName)
	if err != nil {
		logf.FromContext(ctx).Info("reading volume usage failed", "node", pod.Spec.NodeName, "error", err.Error())
		return interval
	}
	used, capacity, ok := VolumeUsage(summary, inst)
	if !ok {
		// The kubelet measures volumes periodically (and not every volume type); try again later.
		return interval
	}
	now := metav1.NewTime(r.now())
	st.UsedBytes = int64(used)
	if st.CapacityBytes == 0 {
		st.CapacityBytes = int64(capacity)
	}
	st.LastUpdateTime = &now
	return interval
}
