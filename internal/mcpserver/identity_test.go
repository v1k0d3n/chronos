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

package mcpserver

import (
	"context"
	"strings"
	"testing"

	"k8s.io/client-go/rest"

	"github.com/modelcontextprotocol/go-sdk/auth"
	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// A request that carries no authenticated identity must be refused. Answering it
// under the server's own ServiceAccount would hand the caller a cluster-wide
// view of the change history regardless of their own permissions.
func TestTokenProviderRefusesUnauthenticatedRequest(t *testing.T) {
	p := &TokenClientProvider{}

	if _, err := p.ClientFor(context.Background()); err == nil {
		t.Fatal("a request with no identity must be refused, not served")
	}
}

// The tools must go through the provider. If a query could ever be answered
// without one, the identity boundary would be bypassable.
func TestToolsRefuseWhenIdentityCannotBeResolved(t *testing.T) {
	s := &Server{Provider: &TokenClientProvider{}}

	if _, err := s.ChangesInWindow(context.Background(), ChangesInWindowArgs{Since: "30m"}); err == nil {
		t.Error("changes_in_window must refuse an unidentified caller")
	}
	if _, err := s.DiffSnapshots(context.Background(), DiffSnapshotsArgs{
		Namespace: "app", Before: "a", After: "b",
	}); err == nil {
		t.Error("diff_snapshots must refuse an unidentified caller")
	}
}

func TestStaticProviderRequiresAClient(t *testing.T) {
	if _, err := (&StaticClientProvider{}).ClientFor(context.Background()); err == nil {
		t.Fatal("a provider with no client must error rather than return nil")
	}
}

// reviewInterceptor fakes the API server's answer to a TokenReview.
func reviewInterceptor(t *testing.T, authenticated bool, username, reason string) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(runtime.NewScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				review, ok := obj.(*authnv1.TokenReview)
				if !ok {
					t.Fatalf("expected a TokenReview, got %T", obj)
				}
				review.Status = authnv1.TokenReviewStatus{
					Authenticated: authenticated,
					User:          authnv1.UserInfo{Username: username},
					Error:         reason,
				}
				return nil
			},
		}).Build()
}

func TestVerifierAcceptsAnAuthenticatedToken(t *testing.T) {
	v := NewTokenReviewVerifier(reviewInterceptor(t, true, "alice", ""), nil, false)

	info, err := v(context.Background(), "a-real-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	// UserID pins the session to this identity so a leaked session id cannot be
	// replayed by somebody else.
	if info.UserID != "alice" {
		t.Errorf("UserID should be the authenticated username, got %q", info.UserID)
	}
	if info.Extra[extraTokenKey] != "a-real-token" {
		t.Error("the raw token must be carried through for the per-caller client")
	}
}

func TestVerifierRejectsAnUnauthenticatedToken(t *testing.T) {
	v := NewTokenReviewVerifier(reviewInterceptor(t, false, "", "token expired"), nil, false)

	_, err := v(context.Background(), "stale-token", nil)
	if err == nil {
		t.Fatal("an unauthenticated token must be rejected")
	}
	// The SDK middleware turns ErrInvalidToken into a 401; anything else would
	// surface as a 500 and read like a server fault rather than a bad credential.
	if !strings.Contains(err.Error(), auth.ErrInvalidToken.Error()) {
		t.Errorf("rejection must wrap ErrInvalidToken so it becomes a 401, got: %v", err)
	}
	if !strings.Contains(err.Error(), "token expired") {
		t.Errorf("the API server's reason should reach the caller, got: %v", err)
	}
}

// audienceAwareReviewer answers like the API server does: a token minted for
// the requested audience is authenticated; a general token is authenticated
// only when no audience is requested.
func audienceAwareReviewer(t *testing.T) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(runtime.NewScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				review := obj.(*authnv1.TokenReview)
				scopedToken := review.Spec.Token == "scoped-for-chronos"
				generalToken := review.Spec.Token == "oc-whoami-token"
				wantScoped := len(review.Spec.Audiences) > 0
				switch {
				case scopedToken && wantScoped:
					review.Status = authnv1.TokenReviewStatus{Authenticated: true, Audiences: review.Spec.Audiences,
						User: authnv1.UserInfo{Username: "system:serviceaccount:agents:remediation-agent", UID: "u1", Groups: []string{"system:serviceaccounts"}}}
				case generalToken && !wantScoped:
					review.Status = authnv1.TokenReviewStatus{Authenticated: true,
						User: authnv1.UserInfo{Username: "alice", Groups: []string{"ocp-admins"},
							Extra: map[string]authnv1.ExtraValue{"scopes.authorization.openshift.io": {"user:full"}, "not-forwardable": {"x"}}}}
				case generalToken && wantScoped:
					review.Status = authnv1.TokenReviewStatus{Authenticated: false, Error: "token audiences [] is invalid"}
				default:
					review.Status = authnv1.TokenReviewStatus{Authenticated: false, Error: "unknown token"}
				}
				return nil
			},
		}).Build()
}

func TestScopedTokenIsAcceptedAndNeverCarried(t *testing.T) {
	v := NewTokenReviewVerifier(audienceAwareReviewer(t), []string{"chronos-mcp"}, false)

	info, err := v(context.Background(), "scoped-for-chronos", nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.UserID != "system:serviceaccount:agents:remediation-agent" {
		t.Errorf("UserID = %q", info.UserID)
	}
	if _, carried := info.Extra[extraTokenKey]; carried {
		t.Error("a scoped token must never be carried on: it is not a credential for anything but this server")
	}
	if u, _ := info.Extra[extraUserKey].(authnv1.UserInfo); u.Username == "" {
		t.Error("the reviewed identity must be carried for impersonation")
	}
}

func TestGeneralTokenIsRefusedInScopedModeUnlessAllowed(t *testing.T) {
	strict := NewTokenReviewVerifier(audienceAwareReviewer(t), []string{"chronos-mcp"}, false)
	if _, err := strict(context.Background(), "oc-whoami-token", nil); err == nil {
		t.Fatal("a general token must be refused when only scoped tokens are allowed")
	} else if !strings.Contains(err.Error(), "oc create token") {
		t.Errorf("the refusal should tell the caller how to mint a usable token, got: %v", err)
	}

	lenient := NewTokenReviewVerifier(audienceAwareReviewer(t), []string{"chronos-mcp"}, true)
	info, err := lenient(context.Background(), "oc-whoami-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.UserID != "alice" || info.Extra[extraTokenKey] != "oc-whoami-token" {
		t.Errorf("an allowed general token should authenticate as its user and be usable directly: %+v", info)
	}
}

func TestImpersonatingProviderActsOnlyAsTheReviewedIdentity(t *testing.T) {
	p := &ImpersonatingClientProvider{Base: &rest.Config{Host: "https://api.example"}, Scheme: runtime.NewScheme()}

	// No identity at all: refuse.
	if _, err := p.ClientFor(context.Background()); err == nil {
		t.Fatal("a request with no reviewed identity must not get a client")
	}

	// The extras forwarded are only those the server may impersonate.
	got := impersonableExtras(map[string]authnv1.ExtraValue{
		"scopes.authorization.openshift.io": {"user:full"},
		"something-else":                    {"secret"},
	})
	if len(got) != 1 || got["scopes.authorization.openshift.io"][0] != "user:full" {
		t.Errorf("forwarded extras = %v; only OAuth scopes may travel", got)
	}
}
