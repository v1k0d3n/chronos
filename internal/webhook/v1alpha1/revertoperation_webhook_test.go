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

package v1alpha1

import (
	"context"
	"reflect"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authnv1 "k8s.io/api/authentication/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	chronosv1alpha1 "github.com/v1k0d3n/chronos/api/v1alpha1"
)

func requestFrom(user authnv1.UserInfo, op admissionv1.Operation) context.Context {
	return admission.NewContextWithRequest(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{Operation: op, UserInfo: user},
	})
}

func TestStamperRecordsTheAuthenticatedCaller(t *testing.T) {
	ro := &chronosv1alpha1.RevertOperation{}
	caller := authnv1.UserInfo{
		Username: "alice",
		UID:      "u-123",
		Groups:   []string{"developers", "system:authenticated"},
		Extra:    map[string]authnv1.ExtraValue{"scopes.authorization.openshift.io": {"user:full"}},
	}

	if err := (&RequesterStamper{}).Default(requestFrom(caller, admissionv1.Create), ro); err != nil {
		t.Fatal(err)
	}

	want := &chronosv1alpha1.Requester{
		Username: "alice",
		UID:      "u-123",
		Groups:   []string{"developers", "system:authenticated"},
		Extra:    map[string][]string{"scopes.authorization.openshift.io": {"user:full"}},
	}
	if !reflect.DeepEqual(ro.Spec.RequestedBy, want) {
		t.Fatalf("requestedBy = %+v, want %+v", ro.Spec.RequestedBy, want)
	}
}

// The point of the webhook: what the client claims is discarded.
func TestStamperOverwritesAClaimedIdentity(t *testing.T) {
	ro := &chronosv1alpha1.RevertOperation{Spec: chronosv1alpha1.RevertOperationSpec{
		RequestedBy: &chronosv1alpha1.Requester{
			Username: "system:admin",
			Groups:   []string{"system:masters"},
			Extra:    map[string][]string{"anything": {"at-all"}},
		},
	}}

	err := (&RequesterStamper{}).Default(requestFrom(authnv1.UserInfo{Username: "alice"}, admissionv1.Create), ro)
	if err != nil {
		t.Fatal(err)
	}

	got := ro.Spec.RequestedBy
	if got.Username != "alice" || len(got.Groups) != 0 || len(got.Extra) != 0 {
		t.Fatalf("a claimed identity survived admission: %+v", got)
	}
}

func TestStamperRefusesWhenItCannotTellWhoIsAsking(t *testing.T) {
	stamper := &RequesterStamper{}

	if err := stamper.Default(context.Background(), &chronosv1alpha1.RevertOperation{}); err == nil {
		t.Error("admitted a revert with no admission request to take an identity from")
	}
	if err := stamper.Default(requestFrom(authnv1.UserInfo{}, admissionv1.Create), &chronosv1alpha1.RevertOperation{}); err == nil {
		t.Error("admitted a revert from a caller with no user name")
	}
}
