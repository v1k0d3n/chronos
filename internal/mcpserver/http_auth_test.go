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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// spyProvider records what identity reached the tool handler.
//
// The question this answers cannot be answered by inspecting the code: the SDK
// owns the path from the HTTP request to the handler, and whether the
// authenticated context survives that journey is a property of the SDK, not of
// ours. If it did not, every request would fail closed — safe, but the server
// would be useless — so it has to be demonstrated rather than assumed.
type spyProvider struct {
	sawToken  string
	sawUserID string
	inner     client.Client
	called    bool
}

func (p *spyProvider) ClientFor(ctx context.Context) (client.Client, error) {
	p.called = true
	if info := auth.TokenInfoFromContext(ctx); info != nil {
		p.sawUserID = info.UserID
		p.sawToken, _ = info.Extra[extraTokenKey].(string)
	}
	return p.inner, nil
}

func (p *spyProvider) Describe() string { return "spy" }

func newAuthTestServer(t *testing.T, spy *spyProvider) *httptest.Server {
	t.Helper()
	verifier := NewTokenReviewVerifier(reviewInterceptor(t, true, "alice", ""), nil, false)
	h := NewHTTPHandler(&Server{Provider: spy}, verifier)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, token string, payload map[string]any) *http.Response {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func initPayload() map[string]any {
	return map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "1"},
		},
	}
}

// The whole point of the fix: an unauthenticated caller never reaches a tool.
func TestHTTPRefusesRequestWithoutToken(t *testing.T) {
	spy := &spyProvider{}
	srv := newAuthTestServer(t, spy)

	resp := post(t, srv.URL, "", initPayload())
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a request with no bearer token, got %d", resp.StatusCode)
	}
	if spy.called {
		t.Error("an unauthenticated request reached the tool layer")
	}
}

func TestHTTPRefusesRequestWithRejectedToken(t *testing.T) {
	spy := &spyProvider{}
	verifier := NewTokenReviewVerifier(reviewInterceptor(t, false, "", "token expired"), nil, false)
	srv := httptest.NewServer(NewHTTPHandler(&Server{Provider: spy}, verifier))
	defer srv.Close()

	resp := post(t, srv.URL, "stale-token", initPayload())
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a rejected token, got %d", resp.StatusCode)
	}
	if spy.called {
		t.Error("a request with an invalid token reached the tool layer")
	}
}

// Demonstrates that the authenticated identity actually survives the SDK's
// journey from HTTP request to tool handler.
func TestHTTPCarriesCallerIdentityIntoToolHandler(t *testing.T) {
	scheme := testScheme(t)
	fakeClient := newFakeClient(scheme)
	spy := &spyProvider{inner: fakeClient}
	srv := newAuthTestServer(t, spy)

	// initialize, to establish a session
	resp := post(t, srv.URL, "alice-token", initPayload())
	sessionID := resp.Header.Get("Mcp-Session-Id")
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize failed with %d", resp.StatusCode)
	}
	if sessionID == "" {
		t.Fatal("no session id returned")
	}

	// notifications/initialized
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer alice-token")
	req.Header.Set("Mcp-Session-Id", sessionID)
	nresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, nresp.Body)
	_ = nresp.Body.Close()

	// tools/call
	callReq, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"changes_in_window","arguments":{"since":"30m"}}}`))
	callReq.Header.Set("Content-Type", "application/json")
	callReq.Header.Set("Accept", "application/json, text/event-stream")
	callReq.Header.Set("Authorization", "Bearer alice-token")
	callReq.Header.Set("Mcp-Session-Id", sessionID)
	cresp, err := http.DefaultClient.Do(callReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cresp.Body.Close() }()
	if cresp.StatusCode != http.StatusOK {
		t.Fatalf("tools/call failed with %d", cresp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, cresp.Body)

	if !spy.called {
		t.Fatal("the tool handler never resolved a caller identity")
	}
	if spy.sawUserID != "alice" {
		t.Errorf("handler saw UserID %q, want alice", spy.sawUserID)
	}
	if spy.sawToken != "alice-token" {
		t.Errorf("handler saw token %q, want the caller's token", spy.sawToken)
	}
}
