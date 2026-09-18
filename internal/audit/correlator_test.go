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

package audit

import (
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"bytes"
	"context"
	"errors"
	"github.com/go-logr/logr"
	chronosv1alpha1 "github.com/v1k0d3n/chronos/api/v1alpha1"
	"io"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"net/http"
	"net/http/httptest"
	"strings"
)

// humanUser is the identity the audit fixtures use for a person at a terminal.
const humanUser = "kube:admin"

// auditLine renders a single JSON audit event line matching the fields the
// correlator decodes.
func auditLine(id, stage, verb, user, resource, ns, name, ip, ua string, t time.Time) string {
	return fmt.Sprintf(
		`{"auditID":%q,"stage":%q,"verb":%q,"user":{"username":%q,"groups":["system:authenticated"]},`+
			`"objectRef":{"resource":%q,"namespace":%q,"name":%q},"sourceIPs":[%q],"userAgent":%q,`+
			`"requestReceivedTimestamp":%q}`,
		id, stage, verb, user, resource, ns, name, ip, ua, t.UTC().Format(time.RFC3339Nano))
}

func changeEvent(kind, ns, name string, verb chronosv1alpha1.ChangeVerb, at time.Time) *chronosv1alpha1.ChangeEvent {
	return &chronosv1alpha1.ChangeEvent{
		Spec: chronosv1alpha1.ChangeEventSpec{
			ObservedAt: metav1.NewTime(at),
			Verb:       verb,
			Target: chronosv1alpha1.TargetObjectReference{
				Kind:      kind,
				Namespace: ns,
				Name:      name,
			},
			Actor: chronosv1alpha1.Actor{Confidence: chronosv1alpha1.AttributionPartial},
		},
	}
}

func TestNormalizeVerb(t *testing.T) {
	cases := map[string]string{
		"create": "create",
		"update": "update",
		"patch":  "update", // a console/oc edit issues PATCH; still an "update" change
		"delete": "delete",
		"get":    "",
		"list":   "",
		"watch":  "",
	}
	for in, want := range cases {
		if got := normalizeVerb(in); got != want {
			t.Errorf("normalizeVerb(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsSharedIdentity(t *testing.T) {
	shared := []string{humanUser, "system:admin"}
	for _, u := range shared {
		if !isSharedIdentity(u) {
			t.Errorf("isSharedIdentity(%q) = false, want true", u)
		}
	}
	notShared := []string{"alice", "system:serviceaccount:foo:builder", ""}
	for _, u := range notShared {
		if isSharedIdentity(u) {
			t.Errorf("isSharedIdentity(%q) = true, want false", u)
		}
	}
}

func TestIngestFiltersToWatchedMutations(t *testing.T) {
	now := time.Now()
	lines := []string{
		// kept: a human patch on a watched configmap
		auditLine("a1", "ResponseComplete", "patch", humanUser, "configmaps", "team-a", "app-config", "10.0.0.5", "oc/4.22", now),
		// dropped: get is not mutating
		auditLine("a2", "ResponseComplete", "get", humanUser, "configmaps", "team-a", "app-config", "10.0.0.5", "oc/4.22", now),
		// dropped: pods are not a watched resource
		auditLine("a3", "ResponseComplete", "update", humanUser, "pods", "team-a", "web", "10.0.0.5", "oc/4.22", now),
		// dropped: not the completed stage
		auditLine("a4", "ResponseStarted", "patch", humanUser, "configmaps", "team-a", "app-config", "10.0.0.5", "oc/4.22", now),
	}
	idx := auditIndex{}
	idx.ingest([]byte(joinLines(lines)), map[string]struct{}{})

	if len(idx) != 1 {
		t.Fatalf("expected exactly one indexed key, got %d: %v", len(idx), idx)
	}
	key := indexKey("configmaps", "team-a", "app-config", "update")
	if recs := idx[key]; len(recs) != 1 || recs[0].username != humanUser {
		t.Fatalf("expected one kube:admin record under %q, got %v", key, idx[key])
	}
}

func TestIngestDedupesByAuditID(t *testing.T) {
	now := time.Now()
	line := auditLine("dup", "ResponseComplete", "patch", "alice", "configmaps", "ns", "cm", "10.0.0.1", "oc", now)
	idx := auditIndex{}
	seen := map[string]struct{}{}
	idx.ingest([]byte(line), seen)
	idx.ingest([]byte(line), seen) // second tail read overlaps the first
	key := indexKey("configmaps", "ns", "cm", "update")
	if got := len(idx[key]); got != 1 {
		t.Fatalf("expected dedup to a single record, got %d", got)
	}
}

func TestMatchPrefersHumanOverController(t *testing.T) {
	now := time.Now()
	// A controller patch and a human patch on the same object, close in time.
	lines := []string{
		auditLine("c1", "ResponseComplete", "patch", "system:serviceaccount:openshift:some-operator", "configmaps", "ns", "cm", "10.1.1.1", "operator", now.Add(2*time.Second)),
		auditLine("h1", "ResponseComplete", "patch", humanUser, "configmaps", "ns", "cm", "10.0.0.9", "oc/4.22", now.Add(5*time.Second)),
	}
	idx := auditIndex{}
	idx.ingest([]byte(joinLines(lines)), map[string]struct{}{})

	ce := changeEvent("ConfigMap", "ns", "cm", chronosv1alpha1.VerbUpdate, now)
	rec, ok := idx.match(ce, defaultMatchWindow)
	if !ok {
		t.Fatal("expected a match")
	}
	if rec.username != humanUser {
		t.Fatalf("expected the human (kube:admin) to win over the controller, got %q", rec.username)
	}
	if !rec.shared {
		t.Errorf("kube:admin should be flagged as a shared identity")
	}
}

func TestMatchRespectsWindow(t *testing.T) {
	now := time.Now()
	line := auditLine("old", "ResponseComplete", "patch", "alice", "configmaps", "ns", "cm", "10.0.0.1", "oc", now.Add(-10*time.Minute))
	idx := auditIndex{}
	idx.ingest([]byte(line), map[string]struct{}{})

	ce := changeEvent("ConfigMap", "ns", "cm", chronosv1alpha1.VerbUpdate, now)
	if _, ok := idx.match(ce, defaultMatchWindow); ok {
		t.Fatal("expected no match for an audit event well outside the window")
	}
}

func TestMatchDeleteAttribution(t *testing.T) {
	now := time.Now()
	// A delete has no managedFields, so watch leaves it unattributed; audit
	// still knows who did it.
	line := auditLine("d1", "ResponseComplete", "delete", "bob", "deployments", "team-b", "api", "10.2.2.2", "kubectl", now)
	idx := auditIndex{}
	idx.ingest([]byte(line), map[string]struct{}{})

	ce := changeEvent("Deployment", "team-b", "api", chronosv1alpha1.VerbDelete, now)
	rec, ok := idx.match(ce, defaultMatchWindow)
	if !ok || rec.username != "bob" {
		t.Fatalf("expected delete attributed to bob, got ok=%v rec=%v", ok, rec)
	}
}

func joinLines(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}

// An agent's change must not be attributed to a person who touched the same
// object minutes earlier.
//
// Regression test for a measured failure on a live cluster: a person scaled a
// Deployment to zero, an autonomous agent scaled it back three minutes later
// through its own ServiceAccount, and the timeline recorded the agent's fix as
// having been made by that person — with `verified` confidence. Both writes are
// "update Deployment web", so the old "prefer any human" rule picked the human
// record anywhere in the two-minute window.
//
// Attribution is the product here. Naming the wrong actor confidently is worse
// than saying nothing, and this is the single event an operator is most likely
// to scrutinise.
func TestAgentChangeIsNotAttributedToAnEarlierHuman(t *testing.T) {
	observed := time.Date(2026, 7, 26, 17, 4, 32, 0, time.UTC)
	key := indexKey("deployments", "demo-chronos", "web", "update")

	idx := auditIndex{key: []auditRecord{
		// The person, three minutes earlier.
		{username: "alice", userAgent: "oc/4.22.0", at: observed.Add(-3 * time.Minute)},
		// The agent, at the moment of the change.
		{
			username:  "system:serviceaccount:agents:remediation-agent",
			userAgent: "kubernetes-mcp-server",
			at:        observed,
		},
	}}

	ce := changeEvent("Deployment", "demo-chronos", "web", chronosv1alpha1.VerbUpdate, observed)
	got, ok := idx.match(ce, 2*time.Minute)

	if !ok {
		t.Fatal("expected a match")
	}
	if got.username != "system:serviceaccount:agents:remediation-agent" {
		t.Errorf("attributed the agent's change to %q", got.username)
	}
}

// The original intent must survive: a human write plus the controller
// bookkeeping it triggers are one logical event, and the human gets the credit.
func TestNearSimultaneousControllerBookkeepingStillPrefersTheHuman(t *testing.T) {
	observed := time.Date(2026, 7, 26, 17, 4, 32, 0, time.UTC)
	key := indexKey("deployments", "demo-chronos", "web", "update")

	idx := auditIndex{key: []auditRecord{
		// The controller reacts a few hundred milliseconds later, so it is
		// marginally closer to when the watcher observed the change.
		{username: "system:kube-controller-manager", at: observed.Add(50 * time.Millisecond)},
		{username: "alice", userAgent: "oc/4.22.0", at: observed.Add(-200 * time.Millisecond)},
	}}

	ce := changeEvent("Deployment", "demo-chronos", "web", chronosv1alpha1.VerbUpdate, observed)
	got, ok := idx.match(ce, 2*time.Minute)

	if !ok {
		t.Fatal("expected a match")
	}
	if got.username != "alice" {
		t.Errorf("simultaneous bookkeeping should not outrank the human; got %q", got.username)
	}
}

// Two service accounts touching the same object: the nearer one wins, because
// neither is a human and there is nothing else to go on.
func TestClosestRecordWinsBetweenTwoServiceAccounts(t *testing.T) {
	observed := time.Date(2026, 7, 26, 17, 4, 32, 0, time.UTC)
	key := indexKey("deployments", "demo-chronos", "web", "update")

	idx := auditIndex{key: []auditRecord{
		{username: "system:serviceaccount:other:thing", at: observed.Add(-90 * time.Second)},
		{username: "system:serviceaccount:agents:remediation-agent", at: observed},
	}}

	ce := changeEvent("Deployment", "demo-chronos", "web", chronosv1alpha1.VerbUpdate, observed)
	got, _ := idx.match(ce, 2*time.Minute)

	if got.username != "system:serviceaccount:agents:remediation-agent" {
		t.Errorf("got %q, want the nearest record", got.username)
	}
}

// The index must report how far back it can actually see.
//
// This is the basis of the coverage check: a change older than the earliest
// collected audit record cannot be attributed from this pass's evidence, and
// attributing it anyway is how an agent's change came to be recorded as a
// person's — the nearest surviving record wins by default, and gets stamped
// "verified".
func TestIndexReportsItsOldestRecord(t *testing.T) {
	base := time.Date(2026, 7, 27, 11, 0, 0, 0, time.UTC)
	idx := auditIndex{
		indexKey("deployments", "ns", "a", "update"): []auditRecord{
			{username: humanUser, at: base.Add(2 * time.Minute)},
			{username: humanUser, at: base.Add(5 * time.Minute)},
		},
		indexKey("configmaps", "ns", "b", "update"): []auditRecord{
			{username: humanUser, at: base},
		},
	}

	got, ok := idx.oldest()
	if !ok {
		t.Fatal("a non-empty index must report a horizon")
	}
	if !got.Equal(base) {
		t.Errorf("oldest = %s, want %s", got, base)
	}

	if _, ok := (auditIndex{}).oldest(); ok {
		t.Error("an empty index has no horizon to report")
	}
}

// The tail size has to be tunable, because the right value depends entirely on
// how busy the API server is. 1 MiB covered about five seconds on a single
// node; the match window is two minutes.
func TestTailBytesIsConfigurable(t *testing.T) {
	if defaultTailBytes < 16<<20 {
		t.Errorf("defaultTailBytes is %d; 1 MiB was measured as ~5s of history", defaultTailBytes)
	}

	t.Setenv("CHRONOS_AUDIT_TAIL_BYTES", "12345")
	if got := envInt("CHRONOS_AUDIT_TAIL_BYTES", defaultTailBytes); got != 12345 {
		t.Errorf("env override ignored; got %d", got)
	}

	// A typo must not stop the correlator from running. Collecting evidence
	// with the default beats refusing to start.
	for _, bad := range []string{"", "banana", "0", "-5"} {
		t.Setenv("CHRONOS_AUDIT_TAIL_BYTES", bad)
		if got := envInt("CHRONOS_AUDIT_TAIL_BYTES", defaultTailBytes); got != defaultTailBytes {
			t.Errorf("%q should fall back to the default, got %d", bad, got)
		}
	}
}

// A byte-addressed tail starts mid-line. That fragment must be dropped without
// taking the records after it with it.
func TestIngestStreamSurvivesACutFirstLine(t *testing.T) {
	at := time.Date(2026, 9, 17, 21, 46, 0, 0, time.UTC)
	whole := auditLine("id-2", "ResponseComplete", "update", "alice", "configmaps", "demo", "app", "10.0.0.5", "oc", at)
	cut := whole[len(whole)/2:] // the tail happened to begin here
	data := cut + "\n" + whole + "\n"

	idx := auditIndex{}
	idx.ingestStream(strings.NewReader(data), map[string]struct{}{})

	recs := idx[indexKey("configmaps", "demo", "app", "update")]
	if len(recs) != 1 || recs[0].username != "alice" {
		t.Fatalf("expected exactly the intact record, got %+v", recs)
	}
}

// A single absurd line — a RequestResponse-level event with a multi-megabyte
// body, say — must not end parsing for the rest of the tail.
func TestIngestStreamSkipsOverlongLines(t *testing.T) {
	at := time.Date(2026, 9, 17, 21, 46, 0, 0, time.UTC)
	good := auditLine("id-1", "ResponseComplete", "create", "bob", "secrets", "demo", "db", "10.0.0.9", "oc", at)
	huge := `{"auditID":"id-huge","stage":"ResponseComplete","verb":"update","objectRef":{"resource":"configmaps","name":"x"},"pad":"` +
		strings.Repeat("x", maxAuditLine+1) + `"}`
	data := huge + "\n" + good + "\n"

	idx := auditIndex{}
	idx.ingestStream(strings.NewReader(data), map[string]struct{}{})

	if _, ok := idx[indexKey("configmaps", "", "x", "update")]; ok {
		t.Error("the overlong line should have been dropped")
	}
	if recs := idx[indexKey("secrets", "demo", "db", "create")]; len(recs) != 1 || recs[0].username != "bob" {
		t.Fatalf("the record after the overlong line was lost: %+v", recs)
	}
}

// The read must stop at tailBytes whatever the node sends, because the whole
// point is that the node once sent everything.
func TestReadNodeAuditNeverReadsPastTailBytes(t *testing.T) {
	const limit = 4096
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != fmt.Sprintf("bytes=-%d", limit) {
			t.Errorf("Range header = %q", r.Header.Get("Range"))
		}
		// Misbehave: ignore the range and send far more than asked.
		_, _ = w.Write(bytes.Repeat([]byte("{}\n"), limit))
	}))
	defer srv.Close()

	cs := kubernetes.NewForConfigOrDie(&rest.Config{Host: srv.URL})
	c := &Correlator{tailBytes: limit}
	var consumed int64
	n, err := c.readNodeAudit(context.Background(), cs, "node-a", func(r io.Reader) {
		consumed, _ = io.Copy(io.Discard, r)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != limit || consumed != limit {
		t.Fatalf("read %d bytes and consumed %d; must be capped at %d", n, consumed, limit)
	}
}

// A pass that has run out of time is reported as such, not as a quiet success
// with an empty index — and it is the timeout that ends it, not the node.
func TestCollectAuditReportsATimedOutPass(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/nodes") && !strings.Contains(r.URL.Path, "proxy"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"kind":"NodeList","apiVersion":"v1","items":[{"metadata":{"name":"node-a"}}]}`))
		default:
			<-release // a stalled node log read
		}
	}))
	defer func() { close(release); srv.Close() }()

	cs := kubernetes.NewForConfigOrDie(&rest.Config{Host: srv.URL})
	c := &Correlator{tailBytes: 1024}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := c.collectAudit(ctx, cs, logr.Discard())
	if err == nil {
		t.Fatal("a stalled read must surface as an error once the pass times out")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a deadline error, got: %v", err)
	}
}

// An impersonated change must record both identities.
//
// `oc --as=alice` puts the authenticated caller in user.username and the
// asserted identity in impersonatedUser. Reading only the former credits the
// change to whoever ran --as, which is how a change made as one person was
// recorded as another.
//
// Neither identity alone is the truth: "kube:admin changed it" hides who was
// acting, "alice changed it" hides that they went through a shared admin
// account to do it. An auditor needs the pair.
func TestImpersonationIsRecordedAlongsideTheCaller(t *testing.T) {
	now := time.Now()
	line := `{"kind":"Event","stage":"ResponseComplete","auditID":"i1","verb":"patch",` +
		`"user":{"username":"kube:admin","groups":["system:cluster-admins"]},` +
		`"impersonatedUser":{"username":"alice","groups":["cluster-admins"]},` +
		`"objectRef":{"resource":"deployments","namespace":"demo","name":"web"},` +
		`"sourceIPs":["10.0.0.9"],"userAgent":"oc/4.22",` +
		`"requestReceivedTimestamp":"` + now.UTC().Format(time.RFC3339Nano) + `"}`

	idx := auditIndex{}
	idx.ingest([]byte(line), map[string]struct{}{})

	ce := changeEvent("Deployment", "demo", "web", chronosv1alpha1.VerbUpdate, now)
	rec, ok := idx.match(ce, defaultMatchWindow)
	if !ok {
		t.Fatal("expected a match")
	}

	if rec.username != humanUser {
		t.Errorf("username = %q; the authenticated caller must be preserved", rec.username)
	}
	if rec.impersonatedUser != "alice" {
		t.Errorf("impersonatedUser = %q, want alice", rec.impersonatedUser)
	}
	if len(rec.impersonatedGroups) != 1 || rec.impersonatedGroups[0] != "cluster-admins" {
		t.Errorf("impersonatedGroups = %v, want [ocp-admins]", rec.impersonatedGroups)
	}
}

// An ordinary change carries no impersonation fields, so nothing is invented.
func TestNonImpersonatedChangeLeavesTheFieldEmpty(t *testing.T) {
	now := time.Now()
	line := auditLine("n1", "ResponseComplete", "patch", humanUser,
		"deployments", "demo", "web", "10.0.0.9", "oc/4.22", now)

	idx := auditIndex{}
	idx.ingest([]byte(line), map[string]struct{}{})

	ce := changeEvent("Deployment", "demo", "web", chronosv1alpha1.VerbUpdate, now)
	rec, _ := idx.match(ce, defaultMatchWindow)

	if rec.impersonatedUser != "" {
		t.Errorf("impersonatedUser = %q on a non-impersonated change", rec.impersonatedUser)
	}
}
