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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// extraTokenKey carries the caller's raw bearer token from the authenticating
// middleware to the tool handler. Only set when the token is one the API
// server itself would accept; a token scoped to this server is never carried.
const extraTokenKey = "chronos.ocp.run/bearer-token"

// extraUserKey carries the identity the API server reported for the token, so
// the impersonating provider can act as it.
const extraUserKey = "chronos.ocp.run/user"

// reviewValidity is how long a single TokenReview answer is considered current.
// Every request is reviewed afresh, so this only has to outlive the request it
// authenticates.
const reviewValidity = time.Minute

// ClientProvider yields the Kubernetes client a request should be answered with.
//
// This exists so that the identity answering a query is the *caller's*, not the
// server's. Chronos's security model rests on two independent defenses, and the
// first is that a user only ever sees changes to objects they could already get.
// A server that queried under its own ServiceAccount would collapse every caller
// onto one cluster-wide identity and quietly discard that defense — leaving
// redaction as the only thing standing between a reader and the whole cluster's
// change history.
type ClientProvider interface {
	// ClientFor returns a client acting as the caller, or an error if the caller
	// cannot be identified.
	ClientFor(ctx context.Context) (client.Client, error)
	// Describe names the identity model in use, for logs and status.
	Describe() string
}

// StaticClientProvider answers every request with one client.
//
// Correct only where the process itself already runs as the user: the stdio
// transport, where the ambient kubeconfig belongs to the person who launched it,
// and tests. It must not be used behind a network listener.
type StaticClientProvider struct {
	Client client.Client
	Reason string
}

// ClientFor implements ClientProvider.
func (p *StaticClientProvider) ClientFor(context.Context) (client.Client, error) {
	if p.Client == nil {
		return nil, fmt.Errorf("no Kubernetes client configured")
	}
	return p.Client, nil
}

// Describe implements ClientProvider.
func (p *StaticClientProvider) Describe() string {
	if p.Reason != "" {
		return p.Reason
	}
	return "ambient credentials"
}

// TokenClientProvider builds a client per caller from their bearer token, so
// every query is evaluated by the API server against that caller's own RBAC.
type TokenClientProvider struct {
	// Base is the in-cluster configuration. Its credentials are stripped before
	// use; only the endpoint and CA are kept.
	Base *rest.Config
	// Scheme and Mapper are shared across callers. The RESTMapper is the
	// expensive part of building a client, and it is identical for everyone, so
	// reusing it keeps per-request construction cheap.
	Scheme *runtime.Scheme
	Mapper meta.RESTMapper

	mu    sync.RWMutex
	cache map[string]client.Client
}

// ClientFor implements ClientProvider.
func (p *TokenClientProvider) ClientFor(ctx context.Context) (client.Client, error) {
	info := auth.TokenInfoFromContext(ctx)
	if info == nil {
		return nil, fmt.Errorf("this request carried no authenticated identity")
	}
	raw, _ := info.Extra[extraTokenKey].(string)
	if raw == "" {
		return nil, fmt.Errorf("this request carried no bearer token")
	}

	// Keyed by digest so the token itself is never a map key that might be
	// printed, logged, or dumped in a panic.
	sum := sha256.Sum256([]byte(raw))
	key := hex.EncodeToString(sum[:])

	p.mu.RLock()
	if c, ok := p.cache[key]; ok {
		p.mu.RUnlock()
		return c, nil
	}
	p.mu.RUnlock()

	// AnonymousClientConfig drops the process's own credentials — certificates,
	// its ServiceAccount token, exec plugins — keeping only the endpoint and
	// trust anchors. Without it the caller's token would be layered onto the
	// server's identity rather than replacing it.
	cfg := rest.AnonymousClientConfig(p.Base)
	cfg.BearerToken = raw
	cfg.BearerTokenFile = ""

	c, err := client.New(cfg, client.Options{Scheme: p.Scheme, Mapper: p.Mapper})
	if err != nil {
		return nil, fmt.Errorf("building a client for the caller: %w", err)
	}

	p.mu.Lock()
	if p.cache == nil {
		p.cache = map[string]client.Client{}
	}
	// Bounded: a busy server should not accumulate a client per token forever.
	// Dropping the whole cache is crude but correct, and rebuilding is cheap
	// because the RESTMapper is shared.
	if len(p.cache) > 512 {
		p.cache = map[string]client.Client{}
	}
	p.cache[key] = c
	p.mu.Unlock()

	return c, nil
}

// Describe implements ClientProvider.
func (p *TokenClientProvider) Describe() string {
	return "per-caller bearer token"
}

// ImpersonatingClientProvider acts as the caller by impersonation, using the
// server's own credentials plus the identity the API server reported for the
// caller's token.
//
// This is what makes audience-scoped tokens possible. A token minted for this
// server (`oc create token <sa> --audience=chronos-mcp`) is refused by the API
// server itself, so it cannot be the credential a query runs under -- but the
// API server will still say who it belongs to, and impersonation lets every
// request run as that person, under their RBAC, exactly as before. The token
// a caller hands over is then worth nothing anywhere but here: leaked from an
// agent's config file or sniffed off the wire, it reads a timeline and no
// more. And it is never cached, because it is never needed again.
//
// The cost is that the server's ServiceAccount holds `impersonate`, which is a
// powerful permission. It is bounded by this code, not by RBAC: the server
// impersonates only an identity the API server has just authenticated, never
// one a caller merely names. A bug here is a real risk, and the tests are the
// control.
type ImpersonatingClientProvider struct {
	// Base is the server's own configuration, credentials included.
	Base   *rest.Config
	Scheme *runtime.Scheme
	Mapper meta.RESTMapper

	mu    sync.RWMutex
	cache map[string]client.Client
}

// ClientFor implements ClientProvider.
func (p *ImpersonatingClientProvider) ClientFor(ctx context.Context) (client.Client, error) {
	info := auth.TokenInfoFromContext(ctx)
	if info == nil {
		return nil, fmt.Errorf("this request carried no authenticated identity")
	}
	user, _ := info.Extra[extraUserKey].(authnv1.UserInfo)
	if user.Username == "" {
		return nil, fmt.Errorf("this request carried no reviewed identity")
	}

	// Keyed by the identity, which is all a client depends on.
	sum := sha256.Sum256([]byte(user.Username + "\x00" + user.UID + "\x00" + strings.Join(user.Groups, ",")))
	key := hex.EncodeToString(sum[:])

	p.mu.RLock()
	if c, ok := p.cache[key]; ok {
		p.mu.RUnlock()
		return c, nil
	}
	p.mu.RUnlock()

	cfg := rest.CopyConfig(p.Base)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: user.Username,
		UID:      user.UID,
		Groups:   user.Groups,
		Extra:    impersonableExtras(user.Extra),
	}
	c, err := client.New(cfg, client.Options{Scheme: p.Scheme, Mapper: p.Mapper})
	if err != nil {
		return nil, fmt.Errorf("building an impersonating client for the caller: %w", err)
	}

	p.mu.Lock()
	if p.cache == nil {
		p.cache = map[string]client.Client{}
	}
	if len(p.cache) > 512 {
		p.cache = map[string]client.Client{}
	}
	p.cache[key] = c
	p.mu.Unlock()
	return c, nil
}

// Describe implements ClientProvider.
func (p *ImpersonatingClientProvider) Describe() string {
	return "impersonation of the reviewed caller"
}

// impersonableExtras keeps the extra attributes the server is allowed to
// impersonate. Forwarding one it is not allowed to would fail every request
// with 403, so the set is explicit: OpenShift OAuth scopes, which restrict
// what a token may do and must travel with the identity to keep doing so.
func impersonableExtras(extra map[string]authnv1.ExtraValue) map[string][]string {
	const scopes = "scopes.authorization.openshift.io"
	if v, ok := extra[scopes]; ok && len(v) > 0 {
		return map[string][]string{scopes: v}
	}
	return nil
}

// NewTokenReviewVerifier authenticates a bearer token by asking the API server
// who it belongs to.
//
// With audiences set, a token is accepted if it was minted for one of them.
// A token that was not -- an `oc whoami -t` token, say, which is good for the
// whole API server -- is accepted only when allowUnscoped is set, because
// handing such a token to any service is handing it a cluster credential.
//
// Verification is delegated rather than performed locally: the API server is the
// only authority on whether a token is valid, and it already handles rotation,
// revocation, projected service account tokens, and OIDC. Doing it here would be
// a second implementation that can only be wrong.
//
// Authenticating at the door also means an invalid token fails once with a clear
// 401 rather than producing a confusing authorization error on every tool call.
func NewTokenReviewVerifier(reviewer client.Client, audiences []string, allowUnscoped bool) auth.TokenVerifier {
	review := func(ctx context.Context, token string, auds []string) (*authnv1.TokenReview, error) {
		tr := &authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{Token: token, Audiences: auds}}
		if err := reviewer.Create(ctx, tr); err != nil {
			return nil, fmt.Errorf("%w: token review failed: %v", auth.ErrInvalidToken, err)
		}
		return tr, nil
	}
	return func(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		tr, err := review(ctx, token, audiences)
		if err != nil {
			return nil, err
		}
		scoped := len(audiences) > 0
		if !tr.Status.Authenticated && scoped && allowUnscoped {
			// Not minted for us. See whether it is a general API server token.
			if tr, err = review(ctx, token, nil); err != nil {
				return nil, err
			}
			scoped = false
		}
		if !tr.Status.Authenticated {
			reason := tr.Status.Error
			if reason == "" {
				reason = "the API server did not recognise this token"
			}
			if len(audiences) > 0 && !allowUnscoped {
				reason += fmt.Sprintf(" (tokens must be minted for audience %q, e.g. `oc create token <sa> --audience=%s`)",
					strings.Join(audiences, ","), audiences[0])
			}
			return nil, fmt.Errorf("%w: %s", auth.ErrInvalidToken, reason)
		}

		extra := map[string]any{extraUserKey: tr.Status.User}
		if !scoped {
			// A general token can be the credential itself. A scoped one never
			// leaves this function.
			extra[extraTokenKey] = token
		}
		review := tr
		return &auth.TokenInfo{
			// UserID lets the transport pin a session to the identity that
			// created it, so a leaked session id cannot be replayed by someone
			// else.
			UserID: review.Status.User.Username,
			// The middleware refuses a TokenInfo with no expiry, and a
			// Kubernetes TokenReview does not report one — it answers only
			// whether the token is valid right now.
			//
			// So this is not the token's lifetime. It is how long this
			// particular answer is treated as current, and it is deliberately
			// short: a TokenReview runs on every request anyway, so a token
			// revoked upstream stops working on the next call rather than
			// lingering until some cached expiry passes.
			Expiration: time.Now().Add(reviewValidity),
			Extra:      extra,
		}, nil
	}
}
