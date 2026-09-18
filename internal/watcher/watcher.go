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

// Package watcher observes live cluster objects via dynamic informers and
// records each user-meaningful change as a ChangeEvent backed by redacted
// before/after ResourceSnapshots. It is the Phase 1 engine of the Chronos
// timeline: it needs only read access to the watched resources, so it can run
// without any API-server audit configuration. Attribution is best-effort
// (derived from managedFields) and marked "partial"/"unattributed" until the
// Phase 2 audit correlator supplies verified identities.
package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	chronosv1alpha1 "github.com/v1k0d3n/chronos/api/v1alpha1"
	chronoscfg "github.com/v1k0d3n/chronos/internal/config"
	"github.com/v1k0d3n/chronos/internal/diff"
	"github.com/v1k0d3n/chronos/internal/metrics"
	"github.com/v1k0d3n/chronos/internal/redact"
	"github.com/v1k0d3n/chronos/internal/risk"
)

const (
	chronosGroup      = "chronos.ocp.run"
	labelPrefix       = "chronos.ocp.run/"
	maxChangedField   = 100
	chronosConfigName = "cluster"
)

// The watcher needs read access to every watched kind and create access to the
// Chronos records. Reading Secrets is inherent to snapshotting them; the
// redactor guarantees their values are never persisted.
//
// +kubebuilder:rbac:groups="",resources=configmaps;secrets;services;serviceaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;daemonsets;statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings;clusterroles;clusterrolebindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=chronos.ocp.run,resources=changeevents;resourcesnapshots,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=chronos.ocp.run,resources=chronosconfigs,verbs=get;list;watch

// Watcher is a manager.Runnable that populates the Chronos timeline.
type Watcher struct {
	config          *rest.Config
	writer          client.Client
	systemNamespace string
	resources       []schema.GroupVersionResource
	// namespaces, when non-empty, scopes the informers to just these namespaces
	// instead of watching cluster-wide. Cluster-scoped kinds (ClusterRole,
	// ClusterRoleBinding) are always watched cluster-wide. Scoping keeps the
	// informer caches small, which is essential on large clusters — a
	// cluster-wide watch over every Secret/ConfigMap is memory-heavy.
	namespaces []string
	log        logr.Logger

	// resolver holds the current noise-filter policy (built-in defaults overlaid
	// by the ChronosConfig singleton). Swapped atomically when the config
	// changes, so filtering re-tunes live without a restart.
	resolver atomic.Pointer[chronoscfg.Resolver]

	// synced gates event handling until the initial informer list has drained,
	// so pre-existing objects are not misrecorded as fresh creates.
	synced atomic.Bool

	// redactionKey signs redaction hashes. Set once before Start.
	redactionKey *redact.Key
}

// SetRedactionKey installs the key redaction hashes are made under.
func (w *Watcher) SetRedactionKey(k *redact.Key) { w.redactionKey = k }

// New builds a Watcher. writer must be a non-cached client so that creating
// Chronos records does not spin up informers for our own CRDs. systemNamespace
// is where records for cluster-scoped targets are stored. namespaces, when
// non-empty, scopes the watch to just those namespaces; empty watches all.
func New(config *rest.Config, writer client.Client, systemNamespace string, namespaces []string) *Watcher {
	w := &Watcher{
		config:          config,
		writer:          writer,
		systemNamespace: systemNamespace,
		resources:       DefaultResources(),
		namespaces:      namespaces,
		log:             ctrl.Log.WithName("watcher"),
	}
	// Start with built-in defaults; reloadConfig overlays any ChronosConfig.
	w.resolver.Store(chronoscfg.NewResolver(nil))
	return w
}

// reloadConfig re-reads the ChronosConfig singleton ("cluster") and swaps in a
// freshly resolved policy. Called on startup and whenever the config changes,
// so filtering re-tunes live.
func (w *Watcher) reloadConfig(ctx context.Context) {
	cfg := &chronosv1alpha1.ChronosConfig{}
	if err := w.writer.Get(ctx, client.ObjectKey{Name: chronosConfigName}, cfg); err != nil {
		if apierrors.IsNotFound(err) {
			w.resolver.Store(chronoscfg.NewResolver(nil))
			return
		}
		w.log.Error(err, "loading ChronosConfig")
		return
	}
	w.resolver.Store(chronoscfg.NewResolver(cfg))
	w.log.Info("ChronosConfig applied", "overrides", len(cfg.Spec.NamespaceOverrides))
}

// isSecret reports whether u is a core/v1 Secret.
func isSecret(u *unstructured.Unstructured) bool {
	gvk := u.GroupVersionKind()
	return gvk.Group == "" && gvk.Kind == "Secret"
}

// secretTypeOf returns a Secret's .type value, or "" for non-Secrets.
func secretTypeOf(u *unstructured.Unstructured) string {
	t, _, _ := unstructured.NestedString(u.Object, "type")
	return t
}

// isControllerActor reports whether an actor name looks like a controller or
// operator rather than a human client. It keys off naming convention, not
// specific component names, so it works on any cluster version. Human clients
// (kubectl-*, oc, console) never match.
func isControllerActor(username string) bool {
	if username == "" {
		return false
	}
	u := strings.ToLower(username)
	return u == "kube-controller-manager" ||
		strings.Contains(u, "controller") ||
		strings.Contains(u, "operator")
}

// NeedLeaderElection ensures only the elected leader records changes, avoiding
// duplicate events when the manager runs with multiple replicas.
func (w *Watcher) NeedLeaderElection() bool { return true }

// clusterScopedResources are the watched kinds that have no namespace, so they
// must always be watched cluster-wide even when the rest of the watch is scoped.
var clusterScopedResources = map[string]bool{
	"clusterroles":        true,
	"clusterrolebindings": true,
}

func isClusterScoped(gvr schema.GroupVersionResource) bool {
	return clusterScopedResources[gvr.Resource]
}

// Start implements manager.Runnable.
func (w *Watcher) Start(ctx context.Context) error {
	dyn, err := dynamic.NewForConfig(w.config)
	if err != nil {
		return fmt.Errorf("building dynamic client: %w", err)
	}

	handlers := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { w.onAdd(ctx, obj) },
		UpdateFunc: func(oldObj, newObj interface{}) { w.onUpdate(ctx, oldObj, newObj) },
		DeleteFunc: func(obj interface{}) { w.onDelete(ctx, obj) },
	}
	register := func(f dynamicinformer.DynamicSharedInformerFactory, gvrs []schema.GroupVersionResource) {
		for _, gvr := range gvrs {
			if _, err := f.ForResource(gvr).Informer().AddEventHandler(handlers); err != nil {
				w.log.Error(err, "registering event handler", "resource", gvr.String())
			}
		}
	}

	var factories []dynamicinformer.DynamicSharedInformerFactory
	if len(w.namespaces) == 0 {
		// Cluster-wide: a single factory over all namespaces.
		f := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, metav1.NamespaceAll, nil)
		register(f, w.resources)
		w.addConfigInformer(ctx, f)
		factories = append(factories, f)
		w.log.Info("starting informers", "mode", "cluster-wide", "resources", len(w.resources))
	} else {
		// Namespace-scoped: one factory per target namespace for namespaced
		// kinds, plus one cluster-wide factory for the cluster-scoped kinds and
		// the ChronosConfig CRD. This keeps the heavy caches (Secrets/ConfigMaps)
		// bounded to the namespaces that matter.
		var namespaced, clusterScoped []schema.GroupVersionResource
		for _, gvr := range w.resources {
			if isClusterScoped(gvr) {
				clusterScoped = append(clusterScoped, gvr)
			} else {
				namespaced = append(namespaced, gvr)
			}
		}
		for _, ns := range w.namespaces {
			f := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, ns, nil)
			register(f, namespaced)
			factories = append(factories, f)
		}
		clusterF := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, metav1.NamespaceAll, nil)
		register(clusterF, clusterScoped)
		w.addConfigInformer(ctx, clusterF)
		factories = append(factories, clusterF)
		w.log.Info("starting informers", "mode", "namespace-scoped",
			"namespaces", w.namespaces, "namespacedKinds", len(namespaced), "clusterScopedKinds", len(clusterScoped))
	}

	for _, f := range factories {
		f.Start(ctx.Done())
	}
	for _, f := range factories {
		for gvr, ok := range f.WaitForCacheSync(ctx.Done()) {
			if !ok {
				w.log.Info("cache failed to sync (missing RBAC or CRD?)", "resource", gvr.String())
			}
		}
	}
	w.reloadConfig(ctx)
	w.synced.Store(true)

	// Anything that changed while this process was down is absent from the
	// timeline, and an absence leaves no trace of itself. Chronos records what
	// it observes; between the last event it wrote and this moment, it observed
	// nothing.
	//
	// Say so, loudly and countably. A ledger that admits where it stopped
	// looking is worth more than one that reads as complete because the gap is
	// invisible — this was measured: a change landed one second before
	// leadership was acquired and produced no ChangeEvent at all, with nothing
	// to indicate one was missing.
	metrics.WatchGapsTotal.Inc()
	w.log.Info("watch resumed; changes made while this process was down were not recorded",
		"resumedAt", time.Now().UTC().Format(time.RFC3339),
		"hint", "alert on chronos_watch_gaps_total to know when the timeline has a hole")

	<-ctx.Done()
	return nil
}

// addConfigInformer registers a handler on the ChronosConfig singleton so filter
// policy re-tunes live. ChronosConfig is cluster-scoped, so this must be on a
// cluster-wide factory.
func (w *Watcher) addConfigInformer(ctx context.Context, f dynamicinformer.DynamicSharedInformerFactory) {
	cfgGVR := schema.GroupVersionResource{Group: chronosGroup, Version: "v1alpha1", Resource: "chronosconfigs"}
	if _, err := f.ForResource(cfgGVR).Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { w.reloadConfig(ctx) },
		UpdateFunc: func(interface{}, interface{}) { w.reloadConfig(ctx) },
		DeleteFunc: func(interface{}) { w.reloadConfig(ctx) },
	}); err != nil {
		w.log.Error(err, "registering ChronosConfig handler")
	}
}

func (w *Watcher) onAdd(ctx context.Context, obj interface{}) {
	// Skip the initial list: those objects predate the watch and are not creates.
	if !w.synced.Load() {
		return
	}
	if u := asUnstructured(obj); u != nil {
		w.record(ctx, string(chronosv1alpha1.VerbCreate), nil, u)
	}
}

func (w *Watcher) onUpdate(ctx context.Context, oldObj, newObj interface{}) {
	oldU, newU := asUnstructured(oldObj), asUnstructured(newObj)
	if oldU == nil || newU == nil {
		return
	}
	// Resync re-delivers the same object; ignore.
	if oldU.GetResourceVersion() == newU.GetResourceVersion() {
		return
	}
	w.record(ctx, string(chronosv1alpha1.VerbUpdate), oldU, newU)
}

func (w *Watcher) onDelete(ctx context.Context, obj interface{}) {
	if !w.synced.Load() {
		return
	}
	// A delete may arrive wrapped when the final state was missed.
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	if u := asUnstructured(obj); u != nil {
		w.record(ctx, string(chronosv1alpha1.VerbDelete), u, nil)
	}
}

// record builds and persists the ChangeEvent and its snapshots for one change.
func (w *Watcher) record(ctx context.Context, verb string, oldU, newU *unstructured.Unstructured) {
	target := newU
	if target == nil {
		target = oldU
	}
	gvk := target.GroupVersionKind()

	// Never record our own resources — that would be an infinite loop.
	if gvk.Group == chronosGroup {
		return
	}

	// Resolve the effective noise-filter policy for this target's namespace.
	fs := w.resolver.Load().EffectiveFor(target.GetNamespace())

	// Drop platform/controller noise from configured namespaces.
	if fs.NamespaceIgnored(target.GetNamespace()) {
		metrics.FilteredTotal.WithLabelValues("namespace").Inc()
		return
	}

	// Drop auto-generated Secret churn (SA token, dockercfg/dockerconfigjson) —
	// controller bookkeeping, never human-meaningful, values redacted anyway.
	if fs.ExcludeNoiseSecrets && isSecret(target) && fs.SecretTypeExcluded(secretTypeOf(target)) {
		metrics.FilteredTotal.WithLabelValues("noise-secret").Inc()
		return
	}

	var oldObj, newObj map[string]interface{}
	if oldU != nil {
		oldObj = oldU.Object
	}
	if newU != nil {
		newObj = newU.Object
	}
	result := diff.Compute(oldObj, newObj)

	// Apply the configured field include/exclude policy.
	meaningful := fs.MeaningfulFields(result.ChangedFields)

	// Drop updates with no meaningful change left — either pure status/
	// resourceVersion churn, or everything filtered out by field policy.
	if verb == string(chronosv1alpha1.VerbUpdate) && len(meaningful) == 0 {
		if len(result.ChangedFields) == 0 {
			metrics.SkippedUpdatesTotal.Inc()
		} else {
			metrics.FilteredTotal.WithLabelValues("field-policy").Inc()
		}
		return
	}

	// Who did it, and is it bookkeeping? Decided from the fields that changed
	// and who owns them — after the diff, never before it. Deciding from
	// whoever last touched the object dropped a person's Deployment edit
	// whenever the deployment controller wrote its annotation a moment later.
	actor, bookkeeping := attribution(target, verb, meaningfulChanges(result, meaningful))
	if fs.ExcludeControllerActors && bookkeeping {
		metrics.FilteredTotal.WithLabelValues("controller-actor").Inc()
		metrics.ControllerWritesTotal.WithLabelValues(actor.Username, gvk.Kind).Inc()
		return
	}

	storeNS := target.GetNamespace()
	if storeNS == "" {
		storeNS = w.systemNamespace
	}

	redacted := false
	beforeName := ""
	afterName := ""
	if oldU != nil {
		snap, wasRedacted := w.buildSnapshot(oldU, storeNS, fs.Redaction)
		beforeName = snap.Name
		redacted = redacted || wasRedacted
		w.createRecord(ctx, snap, "snapshot")
	}
	if newU != nil {
		snap, wasRedacted := w.buildSnapshot(newU, storeNS, fs.Redaction)
		afterName = snap.Name
		redacted = redacted || wasRedacted
		w.createRecord(ctx, snap, "snapshot")
	}

	riskLevel := risk.Classify(gvk.Group, gvk.Kind, verb)

	changed := meaningful
	if len(changed) > maxChangedField {
		changed = append(changed[:maxChangedField:maxChangedField], fmt.Sprintf("…+%d more", len(meaningful)-maxChangedField))
	}

	event := &chronosv1alpha1.ChangeEvent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      changeEventName(gvk.Kind, target.GetName(), verb, target.GetResourceVersion()),
			Namespace: storeNS,
			Labels:    eventLabels(gvk.Kind, target.GetNamespace(), verb, actor.Confidence, riskLevel),
		},
		Spec: chronosv1alpha1.ChangeEventSpec{
			ObservedAt:     observedAt(verb, target),
			Verb:           chronosv1alpha1.ChangeVerb(verb),
			Target:         targetRef(target),
			Actor:          actor,
			Source:         chronosv1alpha1.SourceWatch,
			RiskLevel:      riskLevel,
			Summary:        result.Summary,
			ChangedFields:  changed,
			BeforeSnapshot: beforeName,
			AfterSnapshot:  afterName,
			Redacted:       redacted,
		},
	}
	w.createRecord(ctx, event, "changeevent")

	metrics.ChangesTotal.WithLabelValues(verb, gvk.Kind, string(actor.Confidence), string(riskLevel)).Inc()
	if actor.Confidence == chronosv1alpha1.AttributionUnattributed {
		metrics.UnattributedChangesTotal.WithLabelValues(verb, gvk.Kind).Inc()
	}

	w.log.V(1).Info("recorded change",
		"verb", verb, "kind", gvk.Kind, "name", target.GetName(),
		"namespace", target.GetNamespace(), "confidence", actor.Confidence,
		"risk", riskLevel, "fields", diff.FieldsPreview(result.ChangedFields, 3))
}

// buildSnapshot produces a redacted ResourceSnapshot for u.
func (w *Watcher) buildSnapshot(u *unstructured.Unstructured, storeNS string, policy redact.Policy) (*chronosv1alpha1.ResourceSnapshot, bool) {
	policy.Key = w.redactionKey
	red, entries, wasRedacted := redact.RedactWith(u, policy)

	var content *runtime.RawExtension
	if raw, err := json.Marshal(red.Object); err == nil {
		content = &runtime.RawExtension{Raw: raw}
	} else {
		w.log.Error(err, "marshaling snapshot content", "name", u.GetName())
		metrics.Errors.WithLabelValues("marshal").Inc()
	}

	snap := &chronosv1alpha1.ResourceSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      snapshotName(u),
			Namespace: storeNS,
			Labels:    targetOnlyLabels(u.GetKind(), u.GetNamespace()),
		},
		Spec: chronosv1alpha1.ResourceSnapshotSpec{
			Target:     targetRef(u),
			CapturedAt: metav1.Now(),
			Content:    content,
			Redactions: entries,
			Redacted:   wasRedacted,
			// A redacted snapshot lacks the values needed to fully restore the
			// object, so it is not revertable on its own.
			Revertable: !wasRedacted,
		},
	}
	metrics.SnapshotsTotal.WithLabelValues(fmt.Sprintf("%t", wasRedacted)).Inc()
	return snap, wasRedacted
}

// createRecord creates a Chronos record, treating AlreadyExists as success
// (records are content-addressed by resourceVersion, so a retry is harmless).
func (w *Watcher) createRecord(ctx context.Context, obj client.Object, stage string) {
	if err := w.writer.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
		w.log.Error(err, "creating record", "stage", stage, "name", obj.GetName(), "namespace", obj.GetNamespace())
		metrics.Errors.WithLabelValues(stage).Inc()
	}
}

// --- attribution -----------------------------------------------------------

// meaningfulChanges keeps the structured changes whose field survived the
// field policy.
func meaningfulChanges(result diff.Result, meaningful []string) []diff.Change {
	keep := make(map[string]bool, len(meaningful))
	for _, f := range meaningful {
		keep[f] = true
	}
	out := make([]diff.Change, 0, len(meaningful))
	for _, c := range result.Changes {
		if keep[c.Field] {
			out = append(out, c)
		}
	}
	return out
}

// attribution names the field manager behind a change and reports whether the
// change is controller bookkeeping.
//
// For a create, that is whoever created the object. For an update, it is
// whoever owns the fields that changed — a person if any of them are a
// person's, and bookkeeping only when every changed field belongs to a
// controller. When managedFields cannot say (no entry claims the changed
// fields), the change is kept and credited to the most recent writer: not
// knowing is a reason to record, not to drop.
//
// managedFields.manager is a client-chosen name, not a verified identity —
// hence "partial" confidence, and hence the audit correlator, which replaces
// it with the authenticated user.
func attribution(u *unstructured.Unstructured, verb string, changes []diff.Change) (chronosv1alpha1.Actor, bool) {
	partial := func(manager string) chronosv1alpha1.Actor {
		return chronosv1alpha1.Actor{Username: manager, UserAgent: manager, Confidence: chronosv1alpha1.AttributionPartial}
	}
	switch verb {
	case string(chronosv1alpha1.VerbDelete):
		// Watch alone cannot identify who performed a delete — managedFields
		// reflect the last writer, not the deleter.
		return chronosv1alpha1.Actor{Confidence: chronosv1alpha1.AttributionUnattributed}, false
	case string(chronosv1alpha1.VerbCreate):
		if creator, ok := creatorOf(u); ok {
			return partial(creator), isControllerActor(creator)
		}
	default:
		if manager, allControllers := managersOfChange(u, changes).principal(); manager != "" {
			return partial(manager), allControllers
		}
	}
	if latest, ok := latestWriter(u); ok {
		return partial(latest), false
	}
	return chronosv1alpha1.Actor{Confidence: chronosv1alpha1.AttributionUnattributed}, false
}

// latestWriter is the most recent non-status writer: the fallback when field
// ownership is unavailable.
func latestWriter(u *unstructured.Unstructured) (string, bool) {
	var best *metav1.ManagedFieldsEntry
	fields := u.GetManagedFields()
	for i := range fields {
		e := fields[i]
		if e.Subresource == "status" || !mutating(e.Operation) {
			continue
		}
		if best == nil || laterThan(e.Time, best.Time) {
			entry := e
			best = &entry
		}
	}
	if best == nil {
		return "", false
	}
	return best.Manager, true
}

func laterThan(a, b *metav1.Time) bool {
	switch {
	case a == nil:
		return false
	case b == nil:
		return true
	default:
		return a.After(b.Time)
	}
}

// observedAt is when this change happened, as accurately as we can establish it.
//
// It used to be the latest timestamp across the object's managedFields, and that
// was wrong in a way that quietly corrupted attribution. A managedFields entry
// records when a *field manager's ownership set* last changed, not when the
// object last changed. Scaling a Deployment touches spec.replicas, which the
// same manager already owns, so the entry keeps its old timestamp — sometimes an
// old one.
//
// Measured on a live cluster:
//
//	change                 recorded at   observedAt was   skew
//	a person scaled 3->0   12:14:33      11:59:06         -15m27s
//	an agent scaled 0->3   12:14:54      12:14:34         -20s
//
// The audit correlator matches an event to an audit record by time. A timestamp
// twenty seconds early landed squarely on the *previous* actor's audit record,
// so the agent's change was attributed to the person who had broken the
// workload — and labelled "verified", because from the correlator's point of
// view the evidence lined up perfectly.
//
// For a create, creationTimestamp is authoritative and exact. For anything else
// the honest answer is when the watch saw it: that lags the real change by the
// informer delivery delay, which is milliseconds, rather than by minutes in an
// unpredictable direction.
func observedAt(verb string, u *unstructured.Unstructured) metav1.Time {
	if verb == string(chronosv1alpha1.VerbCreate) {
		if ts := u.GetCreationTimestamp(); !ts.IsZero() {
			return ts
		}
	}
	return metav1.Now()
}

func targetRef(u *unstructured.Unstructured) chronosv1alpha1.TargetObjectReference {
	return chronosv1alpha1.TargetObjectReference{
		APIVersion:      u.GetAPIVersion(),
		Kind:            u.GetKind(),
		Namespace:       u.GetNamespace(),
		Name:            u.GetName(),
		UID:             string(u.GetUID()),
		ResourceVersion: u.GetResourceVersion(),
	}
}

// --- labels & names --------------------------------------------------------

func eventLabels(kind, namespace, verb string, confidence chronosv1alpha1.AttributionConfidence, riskLevel chronosv1alpha1.RiskLevel) map[string]string {
	labels := map[string]string{
		labelPrefix + "target-kind": strings.ToLower(kind),
		labelPrefix + "verb":        verb,
		labelPrefix + "confidence":  string(confidence),
		labelPrefix + "risk":        string(riskLevel),
	}
	if namespace != "" {
		labels[labelPrefix+"target-namespace"] = namespace
	}
	return labels
}

func targetOnlyLabels(kind, namespace string) map[string]string {
	labels := map[string]string{labelPrefix + "target-kind": strings.ToLower(kind)}
	if namespace != "" {
		labels[labelPrefix+"target-namespace"] = namespace
	}
	return labels
}

func snapshotName(u *unstructured.Unstructured) string {
	base := sanitize(fmt.Sprintf("%s-%s-%s", u.GetKind(), u.GetName(), u.GetResourceVersion()))
	seed := strings.Join([]string{u.GetKind(), u.GetNamespace(), u.GetName(), u.GetResourceVersion()}, "/")
	return withHash(base, seed)
}

// EventName is the name the watcher gives the ChangeEvent for a given object
// version. It is content-addressed, so another component that knows the
// resourceVersion a write produced can name — and find — the record of it.
func EventName(kind, name, verb, resourceVersion string) string {
	return changeEventName(kind, name, verb, resourceVersion)
}

func changeEventName(kind, name, verb, resourceVersion string) string {
	base := sanitize(fmt.Sprintf("%s-%s-%s-%s", kind, name, verb, resourceVersion))
	seed := strings.Join([]string{kind, name, verb, resourceVersion}, "/")
	return withHash(base, seed)
}

// withHash appends a short digest so names stay unique even after sanitization
// or truncation, and remain valid RFC 1123 subdomain names (<=253 chars).
func withHash(base, seed string) string {
	const maxBase = 240
	if len(base) > maxBase {
		base = strings.Trim(base[:maxBase], "-.")
	}
	if base == "" {
		base = "obj"
	}
	return base + "-" + shortHash(seed)
}

func sanitize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

func asUnstructured(obj interface{}) *unstructured.Unstructured {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	return u
}
