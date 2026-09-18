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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	chronosv1alpha1 "github.com/v1k0d3n/chronos/api/v1alpha1"
	"github.com/v1k0d3n/chronos/internal/redact"
	"github.com/v1k0d3n/chronos/internal/watcher"
)

// fieldManager identifies Chronos as the owner of fields it reverts, via
// server-side apply.
const fieldManager = "chronos-revert"

// RevertOperationReconciler reconciles a RevertOperation object by restoring a
// target object to a prior ResourceSnapshot via server-side apply.
type RevertOperationReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIReader reads the target object straight from the API server. The
	// cached client would start a cluster-wide informer for every kind it was
	// asked about, including Secrets.
	APIReader client.Reader
	// Admission establishes that spec.requestedBy can be believed. Required: a
	// reconciler without one refuses every revert.
	Admission AdmissionGuard
	// SystemNamespace is where records for cluster-scoped objects are kept, and
	// so the only namespace from which they can be reverted.
	SystemNamespace string
	// AttributionWait bounds how long to wait for the watcher to record an
	// applied revert before giving up on attributing it. Zero means 5s.
	AttributionWait time.Duration
}

// +kubebuilder:rbac:groups=chronos.ocp.run,resources=revertoperations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=chronos.ocp.run,resources=revertoperations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=chronos.ocp.run,resources=resourcesnapshots;changeevents,verbs=get;list;watch
// Attributing the revert's own record, and marking the undone change as reverted.
// +kubebuilder:rbac:groups=chronos.ocp.run,resources=changeevents,verbs=patch
// +kubebuilder:rbac:groups=chronos.ocp.run,resources=changeevents/status,verbs=patch;update
// Write access to the kinds Chronos can revert (server-side apply needs patch).
// +kubebuilder:rbac:groups="",resources=configmaps;secrets;services;serviceaccounts,verbs=get;create;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;daemonsets;statefulsets,verbs=get;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings;clusterroles;clusterrolebindings,verbs=get;create;update;patch
// Restoring a Role means writing permissions the reverter does not itself hold,
// and restoring a binding means binding a role it could not otherwise grant.
// The API server allows that only to a caller with escalate/bind. The reverter
// holds them so that any RBAC object Chronos recorded can be put back -- and
// that is safe only because the requester must hold them too (see authorize).
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;clusterroles,verbs=escalate;bind
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;create;update;patch

// Reconcile actuates a RevertOperation once (it is a one-shot action, not a
// continuously reconciled desired state).
func (r *RevertOperationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	ro := &chronosv1alpha1.RevertOperation{}
	if err := r.Get(ctx, req.NamespacedName, ro); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Terminal already — nothing to do.
	switch ro.Status.Phase {
	case chronosv1alpha1.RevertSucceeded,
		chronosv1alpha1.RevertFailed,
		chronosv1alpha1.RevertSkipped:
		return ctrl.Result{}, nil
	}

	// A strategy this controller does not implement is refused, not quietly
	// replaced by one it does: a person who asked for delete-and-recreate and
	// got a server-side apply would have no way to tell.
	if ro.Spec.Strategy == chronosv1alpha1.StrategyDeleteAndRecreate {
		return r.fail(ctx, ro, "strategy DeleteAndRecreate is not implemented; use ServerSideApply (the default)")
	}

	// Who is asking? Everything after this acts with the operator's authority
	// on that person's behalf, so it is settled first and it fails closed.
	if r.Admission == nil {
		return r.fail(ctx, ro, "this operator has no way to establish who requested a revert, so it performs none")
	}
	if err := r.Admission.Verify(ctx, ro); err != nil {
		return r.fail(ctx, ro, err.Error())
	}
	who := ro.Spec.RequestedBy
	if who == nil || who.Username == "" {
		return r.fail(ctx, ro, "this RevertOperation does not record who requested it, so it cannot be authorized")
	}

	// Resolve which snapshot to restore to.
	snapshotName := ro.Spec.ToSnapshot
	if snapshotName == "" && ro.Spec.ChangeEventRef != "" {
		ce := &chronosv1alpha1.ChangeEvent{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: ro.Namespace, Name: ro.Spec.ChangeEventRef}, ce); err != nil {
			return r.fail(ctx, ro, fmt.Sprintf("resolving changeEventRef: %v", err))
		}
		if !sameObject(ce.Spec.Target, ro.Spec.Target) {
			return r.fail(ctx, ro, fmt.Sprintf("ChangeEvent %q records a change to %s, not to the requested target %s",
				ce.Name, describe(ce.Spec.Target), describe(ro.Spec.Target)))
		}
		snapshotName = ce.Spec.BeforeSnapshot
	}
	if snapshotName == "" {
		return r.fail(ctx, ro, "no snapshot to revert to (set toSnapshot or a changeEventRef with a beforeSnapshot)")
	}

	snap := &chronosv1alpha1.ResourceSnapshot{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ro.Namespace, Name: snapshotName}, snap); err != nil {
		return r.fail(ctx, ro, fmt.Sprintf("loading snapshot %q: %v", snapshotName, err))
	}
	if snap.Spec.Content == nil || len(snap.Spec.Content.Raw) == 0 {
		return r.fail(ctx, ro, "snapshot has no stored content")
	}

	// Reconstruct the prior object from the snapshot.
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(snap.Spec.Content.Raw, &obj.Object); err != nil {
		return r.fail(ctx, ro, fmt.Sprintf("decoding snapshot content: %v", err))
	}

	// Is what we are about to apply the thing that was asked for?
	target, err := checkBinding(r.RESTMapper(), ro, snap, obj, r.SystemNamespace)
	if err != nil {
		return r.fail(ctx, ro, err.Error())
	}

	// Could the requester have done this themselves?
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(obj.GroupVersionKind())
	exists := true
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: target.namespace, Name: target.name}, current); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("reading the current %s: %w", ro.Spec.Target.Kind, err)
		}
		exists = false
	}
	denied, err := r.authorize(ctx, who, target, obj, exists)
	if err != nil {
		// Not a verdict. Retry rather than record a refusal nobody issued.
		return ctrl.Result{}, err
	}
	if denied != "" {
		log.Info("revert refused", "requester", who.Username, "target", ro.Spec.Target.Name, "reason", denied)
		return r.fail(ctx, ro, denied)
	}

	sanitizeForApply(obj)

	// A redacted value was never stored, so it cannot be restored — and it must
	// not be overwritten with the blank that stands in for it. Each redacted
	// field is left out of the apply entirely: server-side apply leaves fields
	// it is not given exactly as they are, so the live value survives and the
	// rest of the object is still reverted. The person is told which fields
	// they have to deal with themselves.
	var manualSteps []string
	for _, red := range snap.Spec.Redactions {
		if red.Reason == chronosv1alpha1.RedactionLastApplied {
			continue // removed at capture; nothing to leave out
		}
		if redact.Remove(obj.Object, red.FieldPath) {
			manualSteps = append(manualSteps, fmt.Sprintf(
				"%s was redacted at capture and has not been restored; its live value was left as it is.", red.FieldPath))
		}
	}
	if snap.Spec.Redacted && len(snap.Spec.Redactions) == 0 {
		// A snapshot from before redactions were itemised: fall back to the
		// only thing it could have redacted.
		unstructured.RemoveNestedField(obj.Object, "data")
		unstructured.RemoveNestedField(obj.Object, "stringData")
		manualSteps = append(manualSteps, "Redacted values were not restored; re-supply them manually.")
	}

	// ForceOwnership is not optional here. A revert exists to put back values
	// that some other field manager — kubectl, the console, a controller — wrote
	// over, so without it every revert worth performing would stop on a conflict.
	patchOpts := []client.PatchOption{
		client.FieldOwner(fieldManager),
		client.ForceOwnership,
	}
	if ro.Spec.DryRun {
		patchOpts = append(patchOpts, client.DryRunAll)
	}

	if err := r.Patch(ctx, obj, client.Apply, patchOpts...); err != nil {
		return r.fail(ctx, ro, fmt.Sprintf("applying revert: %v", err))
	}

	now := metav1.Now()
	ro.Status.Phase = chronosv1alpha1.RevertSucceeded
	ro.Status.AppliedStrategy = chronosv1alpha1.StrategyServerSideApply
	ro.Status.ManualStepsRequired = manualSteps
	ro.Status.CompletedAt = &now
	ro.Status.ObservedGeneration = ro.Generation

	if !ro.Spec.DryRun {
		// The watcher records this apply like any other change, credited to
		// the field manager "chronos-revert". That is the wrong answer to the
		// question the timeline exists to answer: the change was made by
		// Chronos on behalf of a person, and it is that person who should
		// appear. The record's name is content-addressed from the version the
		// apply produced, so it can be found and corrected.
		verb := chronosv1alpha1.VerbUpdate
		if !exists {
			verb = chronosv1alpha1.VerbCreate
		}
		eventName := watcher.EventName(obj.GetKind(), obj.GetName(), string(verb), obj.GetResourceVersion())
		if err := r.attributeResult(ctx, ro, eventName, who); err != nil {
			log.Info("the revert succeeded, but its timeline record could not be attributed to the requester",
				"changeEvent", eventName, "err", err.Error())
		} else {
			ro.Status.ResultingChangeEvent = eventName
		}
		if ro.Spec.ChangeEventRef != "" {
			if err := r.markReverted(ctx, ro); err != nil {
				log.Info("could not mark the undone change as reverted", "changeEvent", ro.Spec.ChangeEventRef, "err", err.Error())
			}
		}
	}
	if ro.Spec.DryRun {
		ro.Status.Message = fmt.Sprintf("Dry run: would revert %s/%s to snapshot %s (no changes applied)",
			ro.Spec.Target.Kind, ro.Spec.Target.Name, snapshotName)
	} else {
		ro.Status.Message = fmt.Sprintf("Reverted %s/%s to snapshot %s via server-side apply",
			ro.Spec.Target.Kind, ro.Spec.Target.Name, snapshotName)
	}
	log.Info("revert applied", "requester", who.Username, "target", ro.Spec.Target.Name, "snapshot", snapshotName, "dryRun", ro.Spec.DryRun)
	return ctrl.Result{}, r.Status().Update(ctx, ro)
}

// attributeResult waits for the watcher to record the apply, then credits it
// to the requester. Confidence is verified: the identity came from the API
// server at admission, which is the same authority the audit log speaks with.
func (r *RevertOperationReconciler) attributeResult(ctx context.Context, ro *chronosv1alpha1.RevertOperation, eventName string, who *chronosv1alpha1.Requester) error {
	wait := r.AttributionWait
	if wait == 0 {
		wait = 5 * time.Second
	}
	deadline := time.Now().Add(wait)
	ce := &chronosv1alpha1.ChangeEvent{}
	for {
		err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: ro.Namespace, Name: eventName}, ce)
		if err == nil {
			break
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no ChangeEvent %q appeared within %s", eventName, wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}

	actor := chronosv1alpha1.Actor{
		Username:   who.Username,
		UID:        who.UID,
		Groups:     who.Groups,
		UserAgent:  "RevertOperation/" + ro.Name,
		Confidence: chronosv1alpha1.AttributionVerified,
	}
	patch := map[string]any{
		"metadata": map[string]any{"labels": map[string]any{
			"chronos.ocp.run/confidence": string(chronosv1alpha1.AttributionVerified),
		}},
		"spec": map[string]any{
			"actor":  actor,
			"source": string(chronosv1alpha1.SourceRevert),
		},
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return r.Patch(ctx, ce, client.RawPatch(types.MergePatchType, data))
}

// markReverted records on the undone ChangeEvent that it has been reverted,
// and by which request.
func (r *RevertOperationReconciler) markReverted(ctx context.Context, ro *chronosv1alpha1.RevertOperation) error {
	ce := &chronosv1alpha1.ChangeEvent{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: ro.Namespace, Name: ro.Spec.ChangeEventRef}, ce); err != nil {
		return err
	}
	patch := fmt.Sprintf(`{"status":{"reverted":true,"revertOperation":%q}}`, ro.Name)
	return r.Status().Patch(ctx, ce, client.RawPatch(types.MergePatchType, []byte(patch)))
}

func (r *RevertOperationReconciler) fail(ctx context.Context, ro *chronosv1alpha1.RevertOperation, msg string) (ctrl.Result, error) {
	now := metav1.Now()
	ro.Status.Phase = chronosv1alpha1.RevertFailed
	ro.Status.Message = msg
	ro.Status.CompletedAt = &now
	ro.Status.ObservedGeneration = ro.Generation
	return ctrl.Result{}, r.Status().Update(ctx, ro)
}

// sanitizeForApply strips server-managed fields so the reconstructed object is
// a clean desired state for server-side apply.
func sanitizeForApply(obj *unstructured.Unstructured) {
	unstructured.RemoveNestedField(obj.Object, "status")
	for _, f := range []string{
		"resourceVersion", "uid", "creationTimestamp", "generation",
		"managedFields", "selfLink", "deletionTimestamp",
	} {
		unstructured.RemoveNestedField(obj.Object, "metadata", f)
	}
	unstructured.RemoveNestedField(obj.Object, "metadata", "annotations",
		"kubectl.kubernetes.io/last-applied-configuration")
}

// SetupWithManager sets up the controller with the Manager.
func (r *RevertOperationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&chronosv1alpha1.RevertOperation{}).
		Named("revertoperation").
		Complete(r)
}
