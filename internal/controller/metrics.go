package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	cyphrav1 "github.com/Cyphra-dev/operator/api/v1alpha1"
)

// Phases lists every phase so that the phase gauge reports 0 for the inactive ones.
var Phases = []cyphrav1.Phase{
	cyphrav1.PhasePending, cyphrav1.PhaseProvisioning, cyphrav1.PhaseReady,
	cyphrav1.PhaseBackingUp, cyphrav1.PhaseStopped, cyphrav1.PhaseFailed,
}

var (
	instanceLabels = []string{"namespace", "name"}

	phaseGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cyphra_neo4j_instance_phase",
		Help: "1 for the current phase of a Neo4jInstance, 0 for the others.",
	}, []string{"namespace", "name", "phase"})
	storageUsedGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cyphra_neo4j_storage_used_bytes",
		Help: "Bytes used on the data volume of a Neo4jInstance, as last measured.",
	}, instanceLabels)
	storageCapacityGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cyphra_neo4j_storage_capacity_bytes",
		Help: "Provisioned size of the data volume of a Neo4jInstance.",
	}, instanceLabels)
	lastBackupGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cyphra_neo4j_last_successful_backup_timestamp_seconds",
		Help: "Unix time of the last successful backup of a Neo4jInstance.",
	}, instanceLabels)
)

func init() {
	metrics.Registry.MustRegister(phaseGauge, storageUsedGauge, storageCapacityGauge, lastBackupGauge)
}

// recordMetrics publishes an instance's status as gauges.
func recordMetrics(inst *cyphrav1.Neo4jInstance) {
	for _, p := range Phases {
		v := 0.0
		if inst.Status.Phase == p {
			v = 1
		}
		phaseGauge.WithLabelValues(inst.Namespace, inst.Name, string(p)).Set(v)
	}
	if st := inst.Status.Storage; st.LastUpdateTime != nil {
		storageUsedGauge.WithLabelValues(inst.Namespace, inst.Name).Set(float64(st.UsedBytes))
	}
	if st := inst.Status.Storage; st.CapacityBytes > 0 {
		storageCapacityGauge.WithLabelValues(inst.Namespace, inst.Name).Set(float64(st.CapacityBytes))
	}
	if t := inst.Status.Backup.LastSuccessfulTime; t != nil {
		lastBackupGauge.WithLabelValues(inst.Namespace, inst.Name).Set(float64(t.Unix()))
	}
}

// deleteMetrics removes every series of a deleted instance.
func deleteMetrics(namespace, name string) {
	l := prometheus.Labels{"namespace": namespace, "name": name}
	phaseGauge.DeletePartialMatch(l)
	storageUsedGauge.Delete(l)
	storageCapacityGauge.Delete(l)
	lastBackupGauge.Delete(l)
}
