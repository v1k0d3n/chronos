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

// Package v1alpha1 holds the admission webhooks for the chronos.ocp.run/v1alpha1
// API.
package v1alpha1

import (
	"context"
	"fmt"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	chronosv1alpha1 "github.com/v1k0d3n/chronos/api/v1alpha1"
)

// The webhook deliberately has no namespaceSelector, objectSelector or
// matchConditions. Any of them would give a requester a way to create a
// RevertOperation that skips this webhook, and the controller refuses to act
// while one is present.
//
// +kubebuilder:webhook:path=/mutate-chronos-ocp-run-v1alpha1-revertoperation,mutating=true,failurePolicy=fail,sideEffects=None,groups=chronos.ocp.run,resources=revertoperations,verbs=create,versions=v1alpha1,name=mrevertoperation.chronos.ocp.run,admissionReviewVersions=v1

// RequesterStamper records who asked for a revert.
//
// A RevertOperation is carried out by the operator, which can write Secrets and
// RBAC objects across the whole cluster. The object itself says nothing
// trustworthy about who created it, so without this the operator's authority
// would be available to anyone able to create one. Admission is the only moment
// the API server tells us who is on the other end of a request; this writes that
// identity onto the object so the controller can later ask whether *that person*
// could have made the change.
//
// It always overwrites. A client that fills in spec.requestedBy is not believed.
type RequesterStamper struct{}

var _ admission.CustomDefaulter = &RequesterStamper{}

// SetupWithManager registers the webhook with the manager's webhook server.
func (s *RequesterStamper) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&chronosv1alpha1.RevertOperation{}).
		WithDefaulter(s).
		Complete()
}

// Default implements admission.CustomDefaulter.
func (s *RequesterStamper) Default(ctx context.Context, obj runtime.Object) error {
	ro, ok := obj.(*chronosv1alpha1.RevertOperation)
	if !ok {
		return fmt.Errorf("expected a RevertOperation, got %T", obj)
	}

	// No request means no identity. Refuse rather than admit an unattributed
	// revert: the webhook's failurePolicy is Fail for the same reason.
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return fmt.Errorf("cannot establish who requested this revert: %w", err)
	}
	if req.Operation != admissionv1.Create {
		return nil
	}
	if req.UserInfo.Username == "" {
		return fmt.Errorf("cannot establish who requested this revert: the API server reported no user name")
	}

	requester := &chronosv1alpha1.Requester{
		Username: req.UserInfo.Username,
		UID:      req.UserInfo.UID,
		Groups:   append([]string(nil), req.UserInfo.Groups...),
	}
	if len(req.UserInfo.Extra) > 0 {
		requester.Extra = make(map[string][]string, len(req.UserInfo.Extra))
		for k, v := range req.UserInfo.Extra {
			requester.Extra[k] = append([]string(nil), v...)
		}
	}
	ro.Spec.RequestedBy = requester
	return nil
}
