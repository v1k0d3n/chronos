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

// Package audit correlates API-server audit events with the change timeline so
// that a ChangeEvent's actor can be upgraded from best-effort (managedFields)
// attribution to a verified identity — the real username, groups, source IP,
// and user agent behind the change.
//
// It reads audit logs through the node log API (the same endpoint
// `oc adm node-logs` uses), so it needs no privileged DaemonSet and no
// API-server audit reconfiguration: OpenShift's default audit profile already
// records the Metadata level (user, verb, objectRef, sourceIPs, userAgent,
// timestamp) that attribution requires.
//
// The correlator is deliberately lazy: it only fetches audit logs when there
// are recent, not-yet-verified ChangeEvents to attribute. On an idle cluster it
// does no work; during active troubleshooting it fetches small tail batches and
// joins them to the pending events by target + verb + time.
package audit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	chronosv1alpha1 "github.com/v1k0d3n/chronos/api/v1alpha1"
	"github.com/v1k0d3n/chronos/internal/metrics"
)

const (
	labelPrefix     = "chronos.ocp.run/"
	confidenceLabel = labelPrefix + "confidence"
	// revertFieldManager is the field manager the revert controller applies
	// under, and so the manager name the watcher credits until the controller
	// replaces it with the requester.
	revertFieldManager = "chronos-revert"

	// controlPlaneRoleLabel selects the nodes whose kube-apiserver writes the
	// audit log.
	controlPlaneRoleLabel = "node-role.kubernetes.io/master"
)

// These knobs favor correctness on a POC-scale cluster; they are intentionally
// modest. Production collection would tail the audit log via a node-local agent
// or an audit webhook rather than polling the node log API.
const (
	defaultInterval = 15 * time.Second

	// defaultTailBytes is how much of each node's audit log is read per pass,
	// measured from the end of the file.
	//
	// This used to be a line count, passed as ?tailLines= to the node log API.
	// The kubelet only honors that parameter for the journal; for a file it
	// serves the whole thing. So every pass pulled the entire audit log — 166 MB
	// on the cluster where this was found — held it in memory, and did so every
	// 15 seconds for as long as anything was left to attribute. The operator
	// reached its memory limit in about ten minutes.
	//
	// The kubelet does honor an HTTP Range header on files, so the tail is now
	// asked for by bytes and parsed as a stream. 32 MiB covered several minutes
	// on that cluster against a 2-minute match window; it is overridable with
	// CHRONOS_AUDIT_TAIL_BYTES because the right size depends entirely on how
	// busy the API server is. The horizon check in reconcile is what makes a
	// wrong value visible instead of silently wrong.
	defaultTailBytes = 32 << 20

	// maxAuditLine bounds a single audit record. Metadata-level events are a few
	// KB; a line longer than this is not one we could use anyway.
	maxAuditLine = 4 << 20

	defaultMatchWindow = 2 * time.Minute
	// recentWindow bounds how long we keep trying to attribute a change. Beyond
	// it, the audit log has usually rotated away and the change is left with its
	// best-effort attribution.
	recentWindow = 12 * time.Minute
	// passTimeout bounds one pass end to end. Without it a stalled read of the
	// node log API — which has happened — parks the loop forever, and nothing
	// downstream can tell a hung correlator from an idle one.
	passTimeout = 45 * time.Second
)

// attributionLabel marks a change the correlator has stopped working on.
// pendingEvents excludes anything carrying it, so a change whose audit record
// scrolled away before it could be read is looked at once, labelled, and never
// re-polled — rather than keeping the audit read running every 15 seconds for
// twelve minutes for evidence that cannot arrive.
const (
	attributionLabel   = labelPrefix + "attribution"
	attributionExpired = "expired"
)

// kindToResource maps the kinds Chronos watches to the plural resource name that
// appears in an audit event's objectRef, so a ChangeEvent can be matched to its
// audit record. It mirrors watcher.DefaultResources.
var kindToResource = map[string]string{
	"Deployment":         "deployments",
	"DaemonSet":          "daemonsets",
	"StatefulSet":        "statefulsets",
	"ConfigMap":          "configmaps",
	"Secret":             "secrets",
	"Service":            "services",
	"ServiceAccount":     "serviceaccounts",
	"Role":               "roles",
	"RoleBinding":        "rolebindings",
	"ClusterRole":        "clusterroles",
	"ClusterRoleBinding": "clusterrolebindings",
	"NetworkPolicy":      "networkpolicies",
}

// watchedResources is the set of plural resource names above, for fast
// membership tests while parsing the (high-volume) audit stream.
var watchedResources = func() map[string]struct{} {
	m := make(map[string]struct{}, len(kindToResource))
	for _, r := range kindToResource {
		m[r] = struct{}{}
	}
	return m
}()

// Reading the audit log via the node log proxy and enriching change events.
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list
// +kubebuilder:rbac:groups="",resources=nodes/proxy,verbs=get
// +kubebuilder:rbac:groups=chronos.ocp.run,resources=changeevents,verbs=get;list;watch;update;patch

// Correlator is a manager Runnable that enriches ChangeEvent actors using the
// API-server audit log.
type Correlator struct {
	config *rest.Config
	// writer is a non-cached client so listing/patching ChangeEvents does not
	// start informers for the Chronos CRDs.
	writer      client.Client
	interval    time.Duration
	tailBytes   int
	matchWindow time.Duration
	// recordSourceIP controls whether a verified actor's source address is
	// kept. It is the one piece of attribution that is about a person's
	// location rather than their identity, and some deployments must not
	// hold it.
	recordSourceIP bool
}

// New builds a Correlator. writer should be the same non-cached client the watch
// controller uses to write Chronos records.
func New(config *rest.Config, writer client.Client) *Correlator {
	return &Correlator{
		config:      config,
		writer:      writer,
		interval:    defaultInterval,
		tailBytes:   envInt("CHRONOS_AUDIT_TAIL_BYTES", defaultTailBytes),
		matchWindow: defaultMatchWindow,
		// Kept unless switched off: knowing where a change came from is part
		// of forensics. CHRONOS_RECORD_SOURCE_IP=false drops it.
		recordSourceIP: strings.ToLower(strings.TrimSpace(os.Getenv("CHRONOS_RECORD_SOURCE_IP"))) != "false",
	}
}

// NeedLeaderElection ensures only the elected leader correlates, avoiding
// duplicate patches when the manager runs with multiple replicas.
func (c *Correlator) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (c *Correlator) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("audit-correlator")

	cs, err := kubernetes.NewForConfig(c.config)
	if err != nil {
		return fmt.Errorf("building kubernetes client: %w", err)
	}

	log.Info("audit correlator started",
		"interval", c.interval.String(), "tailBytes", c.tailBytes)
	if os.Getenv("CHRONOS_AUDIT_TAIL_LINES") != "" {
		log.Info("CHRONOS_AUDIT_TAIL_LINES is no longer used; the node log API ignores line counts for files. Set CHRONOS_AUDIT_TAIL_BYTES instead.")
	}

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.runPass(ctx, cs, log)
		}
	}
}

// runPass runs one bounded, measured pass.
func (c *Correlator) runPass(ctx context.Context, cs kubernetes.Interface, log logr.Logger) {
	passCtx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()

	start := time.Now()
	result, err := c.reconcile(passCtx, cs, log)
	metrics.AuditPassSeconds.Observe(time.Since(start).Seconds())

	switch {
	case err == nil:
		metrics.AuditPassesTotal.WithLabelValues(result).Inc()
	case errors.Is(passCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		metrics.AuditPassesTotal.WithLabelValues("timeout").Inc()
		log.Error(err, "audit correlation pass timed out", "after", passTimeout.String(),
			"hint", "lower CHRONOS_AUDIT_TAIL_BYTES if this recurs")
	default:
		metrics.AuditPassesTotal.WithLabelValues("error").Inc()
		log.Error(err, "audit correlation pass failed")
	}
}

// reconcile runs one attribution pass: find recent unverified changes, and if
// any exist, join them to fresh audit records.
func (c *Correlator) reconcile(ctx context.Context, cs kubernetes.Interface, log logr.Logger) (result string, err error) {
	pending, err := c.pendingEvents(ctx)
	if err != nil {
		return "", fmt.Errorf("listing pending change events: %w", err)
	}
	if len(pending) == 0 {
		// Nothing to attribute — do not touch the audit log at all.
		return "idle", nil
	}

	buffer, err := c.collectAudit(ctx, cs, log)
	if err != nil {
		return "", fmt.Errorf("collecting audit events: %w", err)
	}
	if len(buffer) == 0 {
		return "ok", nil
	}

	horizon, haveHorizon := buffer.oldest()

	for i := range pending {
		ce := &pending[i]
		// If the audit tail does not reach back to this change, we have no
		// evidence about who made it. Matching anyway would pick the nearest
		// surviving record and label it "verified", which is how an agent's
		// change came to be recorded as a person's. Leaving it partial is the
		// honest outcome.
		//
		// It is also final. Every later pass reads a newer window, so the
		// evidence is not coming back; the change is marked so it stops being
		// polled for. The audit read only happens while something is pending,
		// so one such change left unmarked would keep it running for the whole
		// recentWindow — twelve minutes of pulling the log for nothing.
		if haveHorizon && ce.Spec.ObservedAt.Time.Before(horizon) {
			log.Info("attribution expired: the change predates the collected audit window",
				"name", ce.Name, "namespace", ce.Namespace,
				"observedAt", ce.Spec.ObservedAt.Time, "auditHorizon", horizon,
				"hint", "raise CHRONOS_AUDIT_TAIL_BYTES if this recurs")
			if err := c.expire(ctx, ce); err != nil {
				log.Error(err, "marking change event expired", "name", ce.Name, "namespace", ce.Namespace)
			}
			continue
		}
		rec, ok := buffer.match(ce, c.matchWindow)
		if !ok {
			continue
		}
		if err := c.enrich(ctx, ce, rec); err != nil {
			log.Error(err, "enriching change event", "name", ce.Name, "namespace", ce.Namespace)
			continue
		}
		log.Info("attributed change to a verified identity",
			"name", ce.Name, "namespace", ce.Namespace,
			"target", ce.Spec.Target.Kind+"/"+ce.Spec.Target.Name,
			"verb", ce.Spec.Verb, "username", rec.username, "sourceIP", rec.sourceIP)
	}
	return "ok", nil
}

// expire marks a change the correlator will not look at again.
func (c *Correlator) expire(ctx context.Context, ce *chronosv1alpha1.ChangeEvent) error {
	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, attributionLabel, attributionExpired)
	if err := c.writer.Patch(ctx, ce, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		return err
	}
	metrics.AuditExpiredTotal.Inc()
	return nil
}

// pendingEvents returns recent ChangeEvents whose actor is not yet verified.
func (c *Correlator) pendingEvents(ctx context.Context) ([]chronosv1alpha1.ChangeEvent, error) {
	sel, err := labels.Parse(confidenceLabel + "!=verified," + attributionLabel + "!=" + attributionExpired)
	if err != nil {
		return nil, err
	}
	list := &chronosv1alpha1.ChangeEventList{}
	if err := c.writer.List(ctx, list, &client.ListOptions{LabelSelector: sel}); err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-recentWindow)
	out := make([]chronosv1alpha1.ChangeEvent, 0, len(list.Items))
	for _, ce := range list.Items {
		if ce.Spec.Actor.Confidence == chronosv1alpha1.AttributionVerified {
			continue
		}
		if _, ok := kindToResource[ce.Spec.Target.Kind]; !ok {
			continue
		}
		// A revert is attributed by the revert controller, to the person who
		// asked for it. The audit log would only ever name the operator.
		if ce.Spec.Actor.Username == revertFieldManager {
			continue
		}
		if ce.Spec.ObservedAt.Time.Before(cutoff) {
			continue
		}
		out = append(out, ce)
	}
	return out, nil
}

// collectAudit reads a tail of the audit log from each control-plane node and
// indexes the mutating events on watched resources.
func (c *Correlator) collectAudit(ctx context.Context, cs kubernetes.Interface, log logr.Logger) (auditIndex, error) {
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: controlPlaneRoleLabel})
	if err != nil {
		return nil, fmt.Errorf("listing control-plane nodes: %w", err)
	}

	index := auditIndex{}
	seen := map[string]struct{}{}
	for _, node := range nodes.Items {
		n, err := c.readNodeAudit(ctx, cs, node.Name, func(r io.Reader) { index.ingestStream(r, seen) })
		metrics.AuditBytesReadTotal.Add(float64(n))
		if err != nil {
			// A single unreadable node should not abort the whole pass — unless
			// the pass itself is out of time, which is not the node's fault.
			if ctx.Err() != nil {
				return nil, fmt.Errorf("reading node %s audit log: %w", node.Name, err)
			}
			log.V(1).Info("reading node audit log failed", "node", node.Name, "err", err.Error())
			continue
		}
	}
	return index, nil
}

// readNodeAudit streams the last tailBytes of a node's kube-apiserver audit log
// through the node log proxy API into consume, and reports how many bytes were
// read.
//
// The tail is requested with an HTTP Range header. That is the only form of
// tailing the kubelet honors for a file (it serves files with http.ServeFile);
// the ?tailLines= parameter applies to the journal alone and is silently
// ignored here, which is how a "20,000 line" read turned out to be the whole
// log. The first line of a byte-addressed tail is almost always cut in half;
// it fails to parse and is skipped, which loses nothing usable.
func (c *Correlator) readNodeAudit(ctx context.Context, cs kubernetes.Interface, node string, consume func(io.Reader)) (int64, error) {
	body, err := cs.CoreV1().RESTClient().Get().
		AbsPath("api", "v1", "nodes", node, "proxy", "logs", "kube-apiserver", "audit.log").
		SetHeader("Range", fmt.Sprintf("bytes=-%d", c.tailBytes)).
		Stream(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = body.Close() }()

	// The node might not honor the range (an older kubelet, a proxy in the
	// way). Whatever it sends, never read more than asked for.
	counted := &countingReader{r: io.LimitReader(body, int64(c.tailBytes))}
	consume(counted)
	return counted.n, ctx.Err()
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// enrich patches a ChangeEvent's actor to the verified identity and marks it as
// correlated. It uses a JSON merge patch so it only touches attribution fields.
func (c *Correlator) enrich(ctx context.Context, ce *chronosv1alpha1.ChangeEvent, rec auditRecord) error {
	actor := map[string]any{
		"confidence": string(chronosv1alpha1.AttributionVerified),
		"username":   rec.username,
	}
	if len(rec.groups) > 0 {
		actor["groups"] = rec.groups
	}
	if rec.sourceIP != "" && c.recordSourceIP {
		actor["sourceIP"] = rec.sourceIP
	}
	if rec.userAgent != "" {
		actor["userAgent"] = rec.userAgent
	}
	if rec.shared {
		actor["shared"] = true
	}
	if rec.impersonatedUser != "" {
		actor["impersonatedUser"] = rec.impersonatedUser
		if len(rec.impersonatedGroups) > 0 {
			actor["impersonatedGroups"] = rec.impersonatedGroups
		}
	}

	patch := map[string]any{
		"metadata": map[string]any{
			"labels": map[string]any{confidenceLabel: string(chronosv1alpha1.AttributionVerified)},
		},
		"spec": map[string]any{
			"actor":  actor,
			"source": string(chronosv1alpha1.SourceCorrelated),
		},
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return c.writer.Patch(ctx, ce, client.RawPatch(types.MergePatchType, data))
}

// --- audit event parsing & indexing ----------------------------------------

// auditRecord is the attribution-relevant subset of an audit event.
type auditRecord struct {
	username  string
	groups    []string
	sourceIP  string
	userAgent string
	shared    bool
	at        time.Time
	// impersonatedUser and impersonatedGroups are the identity the caller
	// asserted with --as, when they used one.
	impersonatedUser   string
	impersonatedGroups []string
}

// human reports whether the record's user looks like an individual rather than a
// service account or controller. Used to prefer human actors when several audit
// events touched the same object in the same window.
func (r auditRecord) human() bool {
	return r.username != "" && !strings.HasPrefix(r.username, "system:")
}

// auditIndex maps "resource|namespace|name|verb" to the audit records seen for
// that object.
type auditIndex map[string][]auditRecord

// oldest returns the earliest audit record in the index, which is how far back
// this pass can actually see. A change older than this cannot be attributed
// from the evidence collected, whatever else happens to be in the buffer.
func (idx auditIndex) oldest() (time.Time, bool) {
	var out time.Time
	for _, recs := range idx {
		for _, r := range recs {
			if out.IsZero() || r.at.Before(out) {
				out = r.at
			}
		}
	}
	return out, !out.IsZero()
}

// auditEvent is the minimal shape we decode from an audit log line.
type auditEvent struct {
	AuditID string `json:"auditID"`
	Stage   string `json:"stage"`
	Verb    string `json:"verb"`
	User    struct {
		Username string   `json:"username"`
		Groups   []string `json:"groups"`
	} `json:"user"`
	// ImpersonatedUser is set when the request used impersonation. The audit
	// log keeps the authenticated caller in User and the asserted identity
	// here; reading only User credits a change to whoever ran `--as`.
	ImpersonatedUser struct {
		Username string   `json:"username"`
		Groups   []string `json:"groups"`
	} `json:"impersonatedUser"`
	ObjectRef struct {
		Resource  string `json:"resource"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"objectRef"`
	SourceIPs                []string  `json:"sourceIPs"`
	UserAgent                string    `json:"userAgent"`
	RequestReceivedTimestamp time.Time `json:"requestReceivedTimestamp"`
}

// ingest parses audit log lines and adds mutating events on watched resources to
// the index, deduplicating by audit ID across overlapping tail reads.
func (idx auditIndex) ingest(raw []byte, seen map[string]struct{}) {
	idx.ingestStream(bytes.NewReader(raw), seen)
}

// ingestStream is ingest over a reader, so a tail is parsed line by line as it
// arrives rather than materialised whole. A line longer than maxAuditLine is
// drained and dropped; parsing continues with the next one. (bufio.Scanner
// cannot do this — it stops for good at the first over-long token.)
func (idx auditIndex) ingestStream(r io.Reader, seen map[string]struct{}) {
	br := bufio.NewReaderSize(r, maxAuditLine)
	for {
		line, isPrefix, err := br.ReadLine()
		if isPrefix {
			// Longer than the buffer: read the rest of it away and move on.
			for isPrefix && err == nil {
				_, isPrefix, err = br.ReadLine()
			}
			if err != nil {
				return
			}
			continue
		}
		if err != nil {
			return
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var e auditEvent
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		// One request logs multiple stages; keep only the completed one.
		if e.Stage != "ResponseComplete" {
			continue
		}
		verb := normalizeVerb(e.Verb)
		if verb == "" || e.ObjectRef.Name == "" {
			continue
		}
		if _, ok := watchedResources[e.ObjectRef.Resource]; !ok {
			continue
		}
		if e.AuditID != "" {
			if _, dup := seen[e.AuditID]; dup {
				continue
			}
			seen[e.AuditID] = struct{}{}
		}

		rec := auditRecord{
			username:           e.User.Username,
			groups:             e.User.Groups,
			userAgent:          e.UserAgent,
			shared:             isSharedIdentity(e.User.Username),
			at:                 e.RequestReceivedTimestamp,
			impersonatedUser:   e.ImpersonatedUser.Username,
			impersonatedGroups: e.ImpersonatedUser.Groups,
		}
		if len(e.SourceIPs) > 0 {
			rec.sourceIP = e.SourceIPs[0]
		}
		key := indexKey(e.ObjectRef.Resource, e.ObjectRef.Namespace, e.ObjectRef.Name, verb)
		idx[key] = append(idx[key], rec)
	}
}

// looksLikeController reports whether an identity is a controller or operator
// reconciling, rather than an actor making a deliberate change.
//
// Keyed off naming convention rather than a list of components, so it holds on
// any cluster version. This mirrors the same rule in the watcher; the two should
// move together.
//
// The distinction that matters here is not human vs machine. An agent acting
// through its own ServiceAccount is a deliberate actor and belongs on the
// timeline under its own name — it just is not a person. Lumping it in with
// controller bookkeeping is what produced the bug this guards against.
func looksLikeController(username string) bool {
	u := strings.ToLower(username)
	return strings.Contains(u, "controller") || strings.Contains(u, "operator")
}

// match finds the best audit record for a ChangeEvent within window.
//
// A human outranks controller bookkeeping, which is what the reconcile loop
// writes back after somebody edits an object. Between any other records, the one
// closest in time wins.
//
// The rule used to be "any human beats any non-human anywhere in the window",
// and that produced a confidently wrong answer in the case this system exists to
// support. An agent remediated a Deployment that a person had broken a couple of
// minutes earlier. Both writes are "update Deployment web", so the human record
// won and the agent's fix was recorded as having been made by that person — with
// `verified` confidence. Naming the wrong actor is worse than admitting
// uncertainty, and worst of all on the one event an operator is most likely to
// scrutinise.
//
// An agent acting through its own ServiceAccount is not bookkeeping. It is an
// actor, and it gets attributed by time like any other.
func (idx auditIndex) match(ce *chronosv1alpha1.ChangeEvent, window time.Duration) (auditRecord, bool) {
	resource := kindToResource[ce.Spec.Target.Kind]
	verb := normalizeVerb(string(ce.Spec.Verb))
	key := indexKey(resource, ce.Spec.Target.Namespace, ce.Spec.Target.Name, verb)
	recs := idx[key]
	if len(recs) == 0 {
		return auditRecord{}, false
	}

	observed := ce.Spec.ObservedAt.Time
	var best *auditRecord
	var bestDelta time.Duration
	for i := range recs {
		r := &recs[i]
		delta := absDuration(r.at.Sub(observed))
		if delta > window {
			continue
		}
		if best == nil {
			best, bestDelta = r, delta
			continue
		}
		// A person always outranks a controller reconciling the same object,
		// however far apart they landed: the reconcile exists because of the
		// edit.
		rCtl, bestCtl := looksLikeController(r.username), looksLikeController(best.username)
		if rCtl != bestCtl {
			if bestCtl && r.human() {
				best, bestDelta = r, delta
			}
			continue
		}
		// Otherwise these are separate changes by separate actors, and the
		// nearer one produced this ChangeEvent.
		if delta < bestDelta {
			best, bestDelta = r, delta
		}
	}
	if best == nil {
		return auditRecord{}, false
	}
	return *best, true
}

// envInt reads a positive integer from the environment, falling back to def.
//
// A malformed or non-positive value falls back rather than failing: this tunes
// how much evidence is collected, and refusing to start over a typo would be a
// worse outcome than running with the default and logging nothing unusual.
func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func indexKey(resource, namespace, name, verb string) string {
	return resource + "|" + namespace + "|" + name + "|" + verb
}

// normalizeVerb collapses HTTP-level audit verbs to the create/update/delete
// vocabulary the watch controller records. A console or oc edit issues PATCH,
// while oc replace issues PUT; both are "update" changes on the timeline.
func normalizeVerb(v string) string {
	switch v {
	case "create":
		return "create"
	case "update", "patch":
		return "update"
	case "delete":
		return "delete"
	default:
		return ""
	}
}

// isSharedIdentity flags accounts that do not map to a single human, so the UI
// can surface them as an attribution blind spot.
func isSharedIdentity(username string) bool {
	switch username {
	case "kube:admin", "system:admin":
		return true
	default:
		return false
	}
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
