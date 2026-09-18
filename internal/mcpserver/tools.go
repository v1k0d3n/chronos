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

// Package mcpserver exposes the Chronos timeline to AI agents over the Model
// Context Protocol.
//
// It deliberately offers only what a generic Kubernetes MCP server cannot do.
// Reading a ChangeEvent or a ResourceSnapshot by name is already possible with
// any tool that can fetch a custom resource, and duplicating that here would be
// maintenance without benefit. What is not possible generically is:
//
//   - bounding the timeline by time, because Chronos labels carry verb, risk,
//     confidence and target but no time dimension; and
//   - diffing two snapshots, which generically would mean pulling two whole
//     object bodies into a model's context and asking it to compare them.
//
// The second is also why this lives in Chronos rather than in a consumer.
// Snapshots are redacted at capture, and a diff must honour those redactions
// and never reconstruct a withheld value. That rule belongs with the code that
// applies it; split across two repositories, the half that is wrong leaks.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	chronosv1alpha1 "github.com/v1k0d3n/chronos/api/v1alpha1"
	"github.com/v1k0d3n/chronos/internal/diff"
)

// maxWindowResults bounds a single response. A timeline query is meant to focus
// an investigation, not to page the whole store into a model's context.
const maxWindowResults = 200

// Server answers Chronos questions for an MCP client.
//
// It holds no client of its own. Every query is answered through a client that
// represents the *caller*, so the API server evaluates it against that caller's
// RBAC — see ClientProvider.
type Server struct {
	Provider ClientProvider
}

// ChangesInWindowArgs selects a slice of the timeline.
type ChangesInWindowArgs struct {
	// Since bounds the window's start: RFC3339, or a Go duration such as "30m"
	// meaning "that long before Until".
	Since string `json:"since" jsonschema:"start of the window: an RFC3339 timestamp, or a duration before 'until' such as 30m or 2h"`
	// Until bounds the end. Defaults to now.
	Until string `json:"until,omitempty" jsonschema:"end of the window as an RFC3339 timestamp; defaults to now"`
	// Namespace limits results to one namespace.
	Namespace string `json:"namespace,omitempty" jsonschema:"only changes to objects in this namespace"`
	// TargetKind limits by kind, e.g. ConfigMap.
	TargetKind string `json:"targetKind,omitempty" jsonschema:"only changes to this kind, e.g. ConfigMap or Deployment"`
	// TargetName limits by object name.
	TargetName string `json:"targetName,omitempty" jsonschema:"only changes to the object with this name"`
	// Verb limits by create, update, or delete.
	Verb string `json:"verb,omitempty" jsonschema:"only changes with this verb: create, update or delete"`
	// MinRisk drops changes below a risk level.
	MinRisk string `json:"minRisk,omitempty" jsonschema:"drop changes below this risk level: low, medium, high or critical"`
	// Limit caps returned rows.
	Limit int `json:"limit,omitempty" jsonschema:"maximum rows to return; default 50"`
}

// DiffSnapshotsArgs names two snapshots to compare.
type DiffSnapshotsArgs struct {
	Namespace string `json:"namespace" jsonschema:"namespace holding both snapshots"`
	Before    string `json:"before" jsonschema:"name of the earlier ResourceSnapshot"`
	After     string `json:"after" jsonschema:"name of the later ResourceSnapshot"`
}

var riskOrder = map[string]int{"low": 0, "medium": 1, "high": 2, "critical": 3}

// ChangesInWindow returns the changes recorded between two times.
func (s *Server) ChangesInWindow(ctx context.Context, args ChangesInWindowArgs) (string, error) {
	until := time.Now().UTC()
	if args.Until != "" {
		t, err := time.Parse(time.RFC3339, args.Until)
		if err != nil {
			return "", fmt.Errorf("until %q is not an RFC3339 timestamp: %w", args.Until, err)
		}
		until = t.UTC()
	}

	since, err := resolveSince(args.Since, until)
	if err != nil {
		return "", err
	}
	if !since.Before(until) {
		return "", fmt.Errorf("the window is empty: since (%s) is not before until (%s)",
			since.Format(time.RFC3339), until.Format(time.RFC3339))
	}

	// Narrow server-side with the labels Chronos already indexes, then filter by
	// time locally. Time is the one axis the labels do not carry.
	selector := client.MatchingLabels{}
	if args.Namespace != "" {
		selector["chronos.ocp.run/target-namespace"] = strings.ToLower(args.Namespace)
	}
	if args.TargetKind != "" {
		selector["chronos.ocp.run/target-kind"] = strings.ToLower(args.TargetKind)
	}
	if args.Verb != "" {
		selector["chronos.ocp.run/verb"] = strings.ToLower(args.Verb)
	}

	caller, err := s.Provider.ClientFor(ctx)
	if err != nil {
		return "", err
	}

	var list chronosv1alpha1.ChangeEventList
	opts := []client.ListOption{selector}
	if args.Namespace != "" {
		opts = append(opts, client.InNamespace(args.Namespace))
	}
	// Listing as the caller. A user without access to a namespace gets a
	// Forbidden here, or simply does not see its events — which is the point.
	if err := caller.List(ctx, &list, opts...); err != nil {
		return "", fmt.Errorf("listing change events: %w", err)
	}

	minRisk := -1
	if args.MinRisk != "" {
		v, ok := riskOrder[strings.ToLower(args.MinRisk)]
		if !ok {
			return "", fmt.Errorf("minRisk %q is not one of low, medium, high, critical", args.MinRisk)
		}
		minRisk = v
	}

	var matched []chronosv1alpha1.ChangeEvent
	for i := range list.Items {
		ev := list.Items[i]
		at := ev.Spec.ObservedAt.UTC()
		if at.Before(since) || at.After(until) {
			continue
		}
		if args.TargetName != "" && !strings.EqualFold(ev.Spec.Target.Name, args.TargetName) {
			continue
		}
		if minRisk >= 0 && riskOrder[strings.ToLower(string(ev.Spec.RiskLevel))] < minRisk {
			continue
		}
		matched = append(matched, ev)
	}

	// Most recent first, so that truncating to `limit` keeps the changes closest
	// to the alarm. The rendering reverses this into chronological order — see
	// renderWindow for why.
	sort.Slice(matched, func(i, j int) bool {
		return matched[i].Spec.ObservedAt.After(matched[j].Spec.ObservedAt.Time)
	})

	limit := args.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > maxWindowResults {
		limit = maxWindowResults
	}
	truncated := false
	if len(matched) > limit {
		matched = matched[:limit]
		truncated = true
	}

	return renderWindow(matched, since, until, truncated), nil
}

// resolveSince accepts either an absolute timestamp or a duration before until.
func resolveSince(since string, until time.Time) (time.Time, error) {
	if since == "" {
		return time.Time{}, fmt.Errorf("since is required: give an RFC3339 timestamp or a duration such as 30m")
	}
	if t, err := time.Parse(time.RFC3339, since); err == nil {
		return t.UTC(), nil
	}
	d, err := time.ParseDuration(since)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"since %q is neither an RFC3339 timestamp nor a duration such as 30m or 2h", since)
	}
	if d <= 0 {
		return time.Time{}, fmt.Errorf("since duration must be positive, got %q", since)
	}
	return until.Add(-d), nil
}

// renderWindow formats a change window in chronological order.
//
// Oldest first, and it says so, because the caller is usually a language model
// reconstructing a sequence of events and it will read a bare list top to bottom
// as a narrative. That is not a hypothetical: an agent was handed a
// newest-first list showing "3 -> 0" above "0 -> 3", reported that the workload
// had been "scaled down to 0, then scaled back up to 3", concluded the problem
// had already resolved itself, and proposed no remediation — while the
// deployment sat at zero replicas and the alarm was still firing.
//
// Inverting a timeline inverts causality, which is the one thing a change
// ledger exists to get right. Selection still takes the most recent `limit`
// entries, so truncation keeps the changes closest to the alarm; only the
// display order is reversed.
func renderWindow(events []chronosv1alpha1.ChangeEvent, since, until time.Time, truncated bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Changes between %s and %s (UTC): %d, in chronological order (oldest first)\n",
		since.Format(time.RFC3339), until.Format(time.RFC3339), len(events))
	if len(events) == 0 {
		// State the negative explicitly. "Nothing changed" is a real and
		// frequently decisive finding in root cause analysis, and an empty table
		// reads like a failed query.
		b.WriteString("\nNo recorded changes in this window. " +
			"Nothing in scope was modified during that period.\n")
		return b.String()
	}
	if truncated {
		b.WriteString("(truncated to the changes closest to the end of the window; " +
			"narrow the window or add filters to see the rest)\n")
	}
	b.WriteString("\n")

	// Reverse into chronological order for display. The slice arrives newest
	// first so that truncation keeps the changes nearest the alarm.
	ordered := make([]chronosv1alpha1.ChangeEvent, len(events))
	for i, ev := range events {
		ordered[len(events)-1-i] = ev
	}

	for _, ev := range ordered {
		t := ev.Spec.Target
		scope := t.Kind + "/" + t.Name
		if t.Namespace != "" {
			scope = t.Namespace + "/" + scope
		}
		fmt.Fprintf(&b, "%s  %-6s %s\n",
			ev.Spec.ObservedAt.UTC().Format(time.RFC3339), ev.Spec.Verb, scope)
		fmt.Fprintf(&b, "    actor: %s (%s)", actorName(ev), ev.Spec.Actor.Confidence)
		if ev.Spec.Actor.Shared {
			// A shared account cannot be traced to a person; say so rather than
			// letting an agent attribute the change to an individual.
			b.WriteString(" [shared account: not attributable to an individual]")
		}
		if ev.Spec.Actor.SourceIP != "" {
			fmt.Fprintf(&b, " from %s", ev.Spec.Actor.SourceIP)
		}
		b.WriteString("\n")
		if ev.Spec.Summary != "" {
			fmt.Fprintf(&b, "    change: %s\n", ev.Spec.Summary)
		}
		if n := len(ev.Spec.ChangedFields); n > 0 {
			fmt.Fprintf(&b, "    fields: %s\n", diff.FieldsPreview(ev.Spec.ChangedFields, 6))
		}
		if ev.Spec.RiskLevel != "" {
			fmt.Fprintf(&b, "    risk: %s\n", ev.Spec.RiskLevel)
		}
		if ev.Spec.BeforeSnapshot != "" && ev.Spec.AfterSnapshot != "" {
			fmt.Fprintf(&b, "    diff with: diff_snapshots(namespace=%q, before=%q, after=%q)\n",
				ev.Namespace, ev.Spec.BeforeSnapshot, ev.Spec.AfterSnapshot)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func actorName(ev chronosv1alpha1.ChangeEvent) string {
	if ev.Spec.Actor.Username != "" {
		return ev.Spec.Actor.Username
	}
	if ev.Spec.Actor.UserAgent != "" {
		return "unattributed (" + ev.Spec.Actor.UserAgent + ")"
	}
	return "unattributed"
}

// DiffSnapshots compares two snapshots field by field.
func (s *Server) DiffSnapshots(ctx context.Context, args DiffSnapshotsArgs) (string, error) {
	if args.Before == "" || args.After == "" {
		return "", fmt.Errorf("both before and after snapshot names are required")
	}

	caller, err := s.Provider.ClientFor(ctx)
	if err != nil {
		return "", err
	}

	before, err := s.getSnapshot(ctx, caller, args.Namespace, args.Before)
	if err != nil {
		return "", err
	}
	after, err := s.getSnapshot(ctx, caller, args.Namespace, args.After)
	if err != nil {
		return "", err
	}

	if before.Spec.Target.UID != "" && after.Spec.Target.UID != "" &&
		before.Spec.Target.UID != after.Spec.Target.UID {
		// Different objects entirely. Diffing them would produce a plausible but
		// meaningless answer, which is worse than refusing.
		return "", fmt.Errorf(
			"these snapshots are of different objects (%s/%s vs %s/%s); a diff would be meaningless",
			before.Spec.Target.Kind, before.Spec.Target.Name,
			after.Spec.Target.Kind, after.Spec.Target.Name)
	}

	beforeContent, err := contentOf(before)
	if err != nil {
		return "", err
	}
	afterContent, err := contentOf(after)
	if err != nil {
		return "", err
	}
	result := diff.Compute(beforeContent, afterContent)

	var b strings.Builder
	t := after.Spec.Target
	scope := t.Kind + "/" + t.Name
	if t.Namespace != "" {
		scope = t.Namespace + "/" + scope
	}
	fmt.Fprintf(&b, "Diff of %s\n", scope)
	fmt.Fprintf(&b, "  before: %s (captured %s)\n", before.Name, before.Spec.CapturedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "  after:  %s (captured %s)\n\n", after.Name, after.Spec.CapturedAt.UTC().Format(time.RFC3339))

	if result.Summary != "" {
		fmt.Fprintf(&b, "summary: %s\n", result.Summary)
	}
	if len(result.ChangedFields) == 0 {
		b.WriteString("\nNo meaningful field changes. Status and metadata churn are ignored.\n")
	} else {
		fmt.Fprintf(&b, "\nchanged fields (%d):\n", len(result.ChangedFields))
		for _, f := range result.ChangedFields {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
	}

	// Redactions are reported, never resolved. Saying a field changed without
	// revealing it is the whole point of capture-time redaction; a diff tool that
	// quietly omitted this would let an agent conclude "nothing changed" about a
	// Secret that did.
	if notes := redactionNotes(before, after); notes != "" {
		b.WriteString("\n" + notes)
	}
	return b.String(), nil
}

func (s *Server) getSnapshot(ctx context.Context, caller client.Client, namespace, name string) (*chronosv1alpha1.ResourceSnapshot, error) {
	var snap chronosv1alpha1.ResourceSnapshot
	key := types.NamespacedName{Name: name, Namespace: namespace}
	if err := caller.Get(ctx, key, &snap); err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("no ResourceSnapshot %q in namespace %q", name, namespace)
		}
		return nil, fmt.Errorf("reading snapshot %q: %w", name, err)
	}
	return &snap, nil
}

// contentOf decodes a snapshot's inlined manifest.
//
// A snapshot whose content was offloaded to ContentRef has nothing to compare
// here. That must be an error, not an empty map: an empty map diffs as "no
// changes", and an agent would conclude nothing happened when in fact the
// evidence merely lives elsewhere.
func contentOf(s *chronosv1alpha1.ResourceSnapshot) (map[string]interface{}, error) {
	if s == nil {
		return nil, fmt.Errorf("snapshot is missing")
	}
	if s.Spec.Content == nil || len(s.Spec.Content.Raw) == 0 {
		if s.Spec.ContentRef != nil {
			return nil, fmt.Errorf(
				"snapshot %q has its content offloaded to external storage (%s/%s) and cannot be "+
					"diffed here", s.Name, s.Spec.ContentRef.Backend, s.Spec.ContentRef.Key)
		}
		// A deletion snapshot legitimately has no content; nil means "absent",
		// which diff.Compute reads as created/deleted.
		return nil, nil
	}

	var obj map[string]interface{}
	if err := json.Unmarshal(s.Spec.Content.Raw, &obj); err != nil {
		return nil, fmt.Errorf("decoding content of snapshot %q: %w", s.Name, err)
	}
	return obj, nil
}

// redactionNotes describes what was withheld, without revealing it.
func redactionNotes(before, after *chronosv1alpha1.ResourceSnapshot) string {
	paths := map[string]string{}
	for _, s := range []*chronosv1alpha1.ResourceSnapshot{before, after} {
		if s == nil {
			continue
		}
		for _, r := range s.Spec.Redactions {
			paths[r.FieldPath] = string(r.Reason)
		}
	}
	if len(paths) == 0 {
		return ""
	}

	keys := make([]string, 0, len(paths))
	for k := range paths {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("redacted fields (values withheld at capture; a change here will not appear above):\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "  - %s (%s)\n", k, paths[k])
	}
	b.WriteString("To compare these, inspect the objects directly with appropriate authorization.\n")
	return b.String()
}
