/*
Copyright 2026 Chronos project and its authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package metrics defines the chronos_* Prometheus series. They register with
// controller-runtime's global registry, so they are exposed on the manager's
// existing /metrics endpoint and scraped by OpenShift's built-in monitoring —
// no separate metrics server. The ChangesTotal breakdown by confidence is the
// raw material for the "attribution health" view.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// ControllerWritesTotal counts changes dropped as controller bookkeeping,
	// by the manager name that caused the drop. A manager name is chosen by
	// the client, so this is how a person hiding behind one becomes visible:
	// an unfamiliar name here is worth a look.
	ControllerWritesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "chronos_controller_writes_suppressed_total",
			Help: "Changes dropped as controller bookkeeping, by field manager and kind.",
		},
		[]string{"manager", "kind"},
	)
	// AuditPassesTotal counts audit-correlation passes by outcome. A pass that
	// never finishes shows up as a counter that stops moving, which is the whole
	// point: the correlator once hung silently for hours with nothing to show it.
	AuditPassesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "chronos_audit_passes_total",
			Help: "Audit correlation passes, by result (idle, ok, error, timeout).",
		},
		[]string{"result"},
	)
	// AuditBytesReadTotal is how much audit log has been pulled from nodes. Read
	// against the pass count it says what each pass costs.
	AuditBytesReadTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "chronos_audit_bytes_read_total",
		Help: "Bytes of kube-apiserver audit log read from control-plane nodes.",
	})
	// AuditPassSeconds is how long a pass takes end to end.
	AuditPassSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "chronos_audit_pass_duration_seconds",
		Help:    "Duration of an audit correlation pass.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
	})
	// AuditExpiredTotal counts changes given up on because their audit record
	// had scrolled out of the tail before it could be read.
	AuditExpiredTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "chronos_audit_expired_total",
		Help: "Change events whose audit evidence expired before attribution.",
	})
	// ChangesTotal counts recorded ChangeEvents, labeled for the timeline and
	// attribution-health dashboards.
	ChangesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "chronos_changes_total",
			Help: "Total change events recorded, by verb, kind, attribution confidence, and risk.",
		},
		[]string{"verb", "kind", "confidence", "risk"},
	)

	// UnattributedChangesTotal counts changes Chronos could not tie to an
	// individual identity — the attribution blind spot.
	UnattributedChangesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "chronos_unattributed_changes_total",
			Help: "Total change events with unattributed actors, by verb and kind.",
		},
		[]string{"verb", "kind"},
	)

	// SnapshotsTotal counts stored ResourceSnapshots.
	SnapshotsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "chronos_snapshots_total",
			Help: "Total resource snapshots stored, by whether they were redacted.",
		},
		[]string{"redacted"},
	)

	// SkippedUpdatesTotal counts watch updates dropped as no-ops (only
	// status/resourceVersion churn, no user-meaningful change).
	SkippedUpdatesTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "chronos_skipped_updates_total",
			Help: "Total watch update events skipped as no-ops.",
		},
	)

	// WatchGapsTotal counts how many times the watch resumed after being down.
	//
	// Every increment is a hole in the timeline. Chronos only records what it
	// observes, so anything changed while it was restarting is absent — and an
	// absence is invisible by nature. A ledger you cannot audit for
	// completeness is worth less than one that admits where it stopped looking,
	// so this exists to be alerted on rather than merely collected.
	WatchGapsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "chronos_watch_gaps_total",
			Help: "Times the watch resumed after downtime; each is a period of unrecorded changes.",
		},
	)

	// FilteredTotal counts changes dropped by the noise filters, by reason
	// (e.g. system namespace, ServiceAccount secret churn).
	FilteredTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "chronos_filtered_total",
			Help: "Total changes dropped by noise filters, by reason.",
		},
		[]string{"reason"},
	)

	// Errors counts failures while recording, by stage.
	Errors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "chronos_record_errors_total",
			Help: "Total errors while recording changes, by stage.",
		},
		[]string{"stage"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		ControllerWritesTotal,
		AuditPassesTotal,
		AuditBytesReadTotal,
		AuditPassSeconds,
		AuditExpiredTotal,
		ChangesTotal,
		UnattributedChangesTotal,
		SnapshotsTotal,
		SkippedUpdatesTotal,
		FilteredTotal,
		WatchGapsTotal,
		Errors,
	)
}
