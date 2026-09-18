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
	"fmt"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	authzv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	chronosv1alpha1 "github.com/v1k0d3n/chronos/api/v1alpha1"
	"github.com/v1k0d3n/chronos/internal/watcher"
)

// A revert is carried out by the operator, whose ServiceAccount can write
// Secrets and RBAC objects anywhere in the cluster. Everything in this file
// exists so that authority is only ever exercised on behalf of someone who
// already holds it themselves:
//
//   - AdmissionGuard establishes that spec.requestedBy can be believed.
//   - checkBinding establishes that what would be applied is the object the
//     request names, and nothing else.
//   - authorize asks the API server whether the requester could have made this
//     change directly.
//
// Each is necessary. Without the first the identity is forgeable; without the
// second a permitted target can smuggle in a different object; without the
// third the first two merely describe an escalation accurately.

const rbacGroup = "rbac.authorization.k8s.io"

// AdmissionGuard reports whether spec.requestedBy on a RevertOperation was
// written by the Chronos admission webhook rather than by whoever created it.
type AdmissionGuard interface {
	Verify(ctx context.Context, ro *chronosv1alpha1.RevertOperation) error
}

// WebhookConfigGuard trusts spec.requestedBy only when the cluster is configured
// so that no RevertOperation can exist without having passed through the
// stamping webhook.
//
// The webhook overwrites requestedBy on every create, but only if the API server
// actually calls it. If its MutatingWebhookConfiguration is missing — an install
// that skipped it, or one where it was removed — the field is simply whatever
// the client wrote, and a user could name any identity they liked. So rather than
// assume the webhook ran, this checks that it must have.
type WebhookConfigGuard struct {
	// Reader must not be a cached client: a cache would need list/watch on every
	// webhook configuration in the cluster to answer a single Get.
	Reader client.Reader
	// Name is the MutatingWebhookConfiguration that carries the stamping webhook.
	Name string
}

// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations,verbs=get

// Verify implements AdmissionGuard.
func (g *WebhookConfigGuard) Verify(ctx context.Context, ro *chronosv1alpha1.RevertOperation) error {
	cfg := &admissionregv1.MutatingWebhookConfiguration{}
	if err := g.Reader.Get(ctx, client.ObjectKey{Name: g.Name}, cfg); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("the Chronos admission webhook (%s) is not installed, so the requester of this revert cannot be established", g.Name)
		}
		return fmt.Errorf("checking the Chronos admission webhook: %w", err)
	}

	var problems []string
	for i := range cfg.Webhooks {
		wh := &cfg.Webhooks[i]
		if !coversRevertOperationCreate(wh) {
			continue
		}
		// Found the webhook. Anything that lets a request skip it makes the
		// identity forgeable for that request, so each is disqualifying.
		switch {
		case wh.FailurePolicy == nil || *wh.FailurePolicy != admissionregv1.Fail:
			problems = append(problems, "its failurePolicy is not Fail, so requests are admitted unstamped whenever it is unreachable")
		case wh.NamespaceSelector != nil && len(wh.NamespaceSelector.MatchLabels)+len(wh.NamespaceSelector.MatchExpressions) > 0:
			problems = append(problems, "it has a namespaceSelector, so some namespaces bypass it")
		case wh.ObjectSelector != nil && len(wh.ObjectSelector.MatchLabels)+len(wh.ObjectSelector.MatchExpressions) > 0:
			problems = append(problems, "it has an objectSelector, so a requester can label their way past it")
		case len(wh.MatchConditions) > 0:
			problems = append(problems, "it has matchConditions, so some requests bypass it")
		default:
			// A RevertOperation older than the configuration was admitted before
			// the webhook existed, and carries whatever its creator wrote.
			if ro.CreationTimestamp.Before(&cfg.CreationTimestamp) {
				return fmt.Errorf("this RevertOperation predates the Chronos admission webhook, so its requester cannot be established; create a new one")
			}
			return nil
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("the Chronos admission webhook cannot be relied on: %s", problems[0])
	}
	return fmt.Errorf("%s has no webhook covering RevertOperation creation, so the requester of this revert cannot be established", g.Name)
}

func coversRevertOperationCreate(wh *admissionregv1.MutatingWebhook) bool {
	for _, rule := range wh.Rules {
		if !containsOrWildcard(rule.APIGroups, chronosv1alpha1.GroupVersion.Group) ||
			!containsOrWildcard(rule.Resources, "revertoperations") {
			continue
		}
		for _, op := range rule.Operations {
			if op == admissionregv1.Create || op == admissionregv1.OperationAll {
				return true
			}
		}
	}
	return false
}

func containsOrWildcard(list []string, want string) bool {
	for _, v := range list {
		if v == want || v == "*" {
			return true
		}
	}
	return false
}

// revertTarget is a request's target resolved against the API server's own
// discovery data: which resource it is, and whether it lives in a namespace.
type revertTarget struct {
	gvr        schema.GroupVersionResource
	namespaced bool
	namespace  string
	name       string
}

// sameObject reports whether two references name the same object. UID and
// resourceVersion are deliberately not compared: a revert targets an object by
// what it is called, and those fields differ between every two snapshots.
func sameObject(a, b chronosv1alpha1.TargetObjectReference) bool {
	return a.APIVersion == b.APIVersion && a.Kind == b.Kind &&
		a.Namespace == b.Namespace && a.Name == b.Name
}

func describe(t chronosv1alpha1.TargetObjectReference) string {
	if t.Namespace == "" {
		return fmt.Sprintf("%s %s %q", t.APIVersion, t.Kind, t.Name)
	}
	return fmt.Sprintf("%s %s %q in namespace %q", t.APIVersion, t.Kind, t.Name, t.Namespace)
}

// checkBinding confirms that the object about to be applied is the one the
// request names, and that the request was made from a place entitled to name it.
//
// The object applied comes from a snapshot's stored content, which is opaque
// JSON. Its kind, namespace and name are whatever that JSON says, regardless of
// what the RevertOperation or the snapshot claim to be about. Authorizing one
// object and applying another is the whole attack, so all three must agree.
func checkBinding(
	mapper meta.RESTMapper,
	ro *chronosv1alpha1.RevertOperation,
	snap *chronosv1alpha1.ResourceSnapshot,
	obj *unstructured.Unstructured,
	systemNamespace string,
) (*revertTarget, error) {
	want := ro.Spec.Target

	if !sameObject(snap.Spec.Target, want) {
		return nil, fmt.Errorf("snapshot %q records %s, not the requested target %s",
			snap.Name, describe(snap.Spec.Target), describe(want))
	}
	content := chronosv1alpha1.TargetObjectReference{
		APIVersion: obj.GetAPIVersion(),
		Kind:       obj.GetKind(),
		Namespace:  obj.GetNamespace(),
		Name:       obj.GetName(),
	}
	if !sameObject(content, want) {
		return nil, fmt.Errorf("snapshot %q contains %s, which is not the requested target %s",
			snap.Name, describe(content), describe(want))
	}

	gv, err := schema.ParseGroupVersion(want.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("target apiVersion %q is not valid: %v", want.APIVersion, err)
	}
	mapping, err := mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: want.Kind}, gv.Version)
	if err != nil {
		return nil, fmt.Errorf("target kind %s %s is not known to this cluster: %v", want.APIVersion, want.Kind, err)
	}

	// Chronos reverts what Chronos records. Anything else reaching this point
	// did not come from the watcher.
	if !revertable(mapping.Resource) {
		return nil, fmt.Errorf("%s is not a kind Chronos reverts", mapping.Resource.GroupResource())
	}

	namespaced := mapping.Scope.Name() == meta.RESTScopeNameNamespace
	switch {
	case namespaced && want.Namespace == "":
		return nil, fmt.Errorf("%s is namespaced, but the target names no namespace", want.Kind)
	case namespaced && want.Namespace != ro.Namespace:
		// Records for a namespaced object live in that object's namespace. A
		// request from anywhere else is reaching across a namespace boundary.
		return nil, fmt.Errorf("a RevertOperation in namespace %q cannot revert an object in namespace %q",
			ro.Namespace, want.Namespace)
	case !namespaced && want.Namespace != "":
		return nil, fmt.Errorf("%s is cluster-scoped, but the target names namespace %q", want.Kind, want.Namespace)
	case !namespaced && ro.Namespace != systemNamespace:
		return nil, fmt.Errorf("cluster-scoped objects can only be reverted from namespace %q, where their records are kept",
			systemNamespace)
	}

	return &revertTarget{
		gvr:        mapping.Resource,
		namespaced: namespaced,
		namespace:  want.Namespace,
		name:       want.Name,
	}, nil
}

func revertable(gvr schema.GroupVersionResource) bool {
	for _, r := range watcher.DefaultResources() {
		if r.Group == gvr.Group && r.Resource == gvr.Resource {
			return true
		}
	}
	return false
}

// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

// authorize asks the API server whether the requester could have made this
// change themselves, and returns a non-nil denial message if not.
//
// The question is delegated rather than answered here for the same reason token
// verification is: the API server is the only authority on what a user may do,
// and it accounts for every authorizer in the chain, not just RBAC.
func (r *RevertOperationReconciler) authorize(
	ctx context.Context,
	who *chronosv1alpha1.Requester,
	target *revertTarget,
	obj *unstructured.Unstructured,
	exists bool,
) (denied string, err error) {
	// Server-side apply is a patch. Against an object that no longer exists it
	// creates one, and the API server requires create for that as well.
	verbs := []string{"patch"}
	if !exists {
		verbs = append(verbs, "create")
	}
	for _, verb := range verbs {
		attrs := &authzv1.ResourceAttributes{
			Group:     target.gvr.Group,
			Version:   target.gvr.Version,
			Resource:  target.gvr.Resource,
			Namespace: target.namespace,
			Name:      target.name,
			Verb:      verb,
		}
		if denied, err = r.review(ctx, who, attrs); denied != "" || err != nil {
			return denied, err
		}
	}

	if target.gvr.Group != rbacGroup {
		return "", nil
	}

	// RBAC objects carry a second rule. The API server will not let anyone write
	// a role granting permissions they lack, or bind a role they could not
	// otherwise grant — but it applies that rule to whoever makes the request,
	// which here is the operator. The operator holds broad permissions, so it
	// would pass a check the requester would fail. Apply it to the requester
	// instead.
	//
	// This is stricter than the API server, which also accepts a user who
	// already holds every permission concerned. Checking that would mean
	// re-implementing rule evaluation; requiring the explicit verb keeps the
	// decision with the API server and errs toward refusing.
	switch target.gvr.Resource {
	case "roles", "clusterroles":
		return r.review(ctx, who, &authzv1.ResourceAttributes{
			Group: rbacGroup, Version: "v1", Resource: target.gvr.Resource,
			Namespace: target.namespace, Name: target.name, Verb: "escalate",
		})
	case "rolebindings", "clusterrolebindings":
		refKind, _, _ := unstructured.NestedString(obj.Object, "roleRef", "kind")
		refName, _, _ := unstructured.NestedString(obj.Object, "roleRef", "name")
		attrs := &authzv1.ResourceAttributes{Group: rbacGroup, Version: "v1", Name: refName, Verb: "bind"}
		switch refKind {
		case "ClusterRole":
			attrs.Resource = "clusterroles"
			// A RoleBinding to a ClusterRole is checked in the binding's namespace.
			attrs.Namespace = target.namespace
		case "Role":
			attrs.Resource = "roles"
			attrs.Namespace = target.namespace
		default:
			return fmt.Sprintf("the binding refers to a %q, which Chronos does not know how to authorize", refKind), nil
		}
		return r.review(ctx, who, attrs)
	}
	return "", nil
}

func (r *RevertOperationReconciler) review(
	ctx context.Context,
	who *chronosv1alpha1.Requester,
	attrs *authzv1.ResourceAttributes,
) (denied string, err error) {
	sar := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:               who.Username,
			UID:                who.UID,
			Groups:             who.Groups,
			ResourceAttributes: attrs,
		},
	}
	if len(who.Extra) > 0 {
		sar.Spec.Extra = make(map[string]authzv1.ExtraValue, len(who.Extra))
		for k, v := range who.Extra {
			sar.Spec.Extra[k] = authzv1.ExtraValue(v)
		}
	}
	if err := r.Create(ctx, sar); err != nil {
		return "", fmt.Errorf("asking the API server whether %s may %s %s: %w", who.Username, attrs.Verb, attrs.Resource, err)
	}
	if sar.Status.Allowed {
		return "", nil
	}

	where := "cluster-wide"
	if attrs.Namespace != "" {
		where = fmt.Sprintf("in namespace %q", attrs.Namespace)
	}
	msg := fmt.Sprintf("%s is not allowed to %s %s %q %s. A revert can only do what the person requesting it could do directly.",
		who.Username, attrs.Verb, attrs.Resource, attrs.Name, where)
	if sar.Status.Reason != "" {
		msg += " (" + sar.Status.Reason + ")"
	}
	return msg, nil
}
