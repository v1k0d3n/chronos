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

// Package diff computes a redaction-safe, human-oriented diff between two
// versions of a Kubernetes object (as unstructured .Object maps). It reports
// the list of changed field paths and a one-line summary, deliberately
// ignoring server-managed noise (status, resourceVersion, managedFields, ...)
// so the timeline shows only changes a human would care about.
package diff

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Result is the outcome of comparing two object versions.
type Result struct {
	// ChangedFields are dot/bracket paths of leaves that differ, e.g.
	// "spec.replicas" or "spec.template.spec.containers[0].image".
	ChangedFields []string
	// Changes are the same leaves with their path kept as segments, for
	// callers that need to walk back into the object — a dotted string cannot
	// be split reliably once an annotation key with dots in it is involved.
	// Parallel to ChangedFields.
	Changes []Change
	// Summary is a one-line description, e.g. "spec.replicas: 3 -> 0",
	// "created", or "deleted".
	Summary string
}

// Change is one differing leaf.
type Change struct {
	// Field is the dotted path, identical to the ChangedFields entry.
	Field string
	// Path is the same location as segments: map keys as themselves, list
	// positions as decimal indexes.
	Path []string
}

// maxSummaryValueLen bounds how much of a scalar value is shown in a summary.
const maxSummaryValueLen = 40

// ignoredMetadata are metadata subfields that churn without user meaning.
var ignoredMetadata = map[string]bool{
	"resourceVersion":   true,
	"generation":        true,
	"managedFields":     true,
	"creationTimestamp": true,
	"uid":               true,
	"selfLink":          true,
}

// ignoredAnnotations carry serialized state or controller bookkeeping rather
// than a user's intent.
var ignoredAnnotations = map[string]bool{
	"kubectl.kubernetes.io/last-applied-configuration": true,
	"deployment.kubernetes.io/revision":                true,
}

// change is an internal record of a single differing leaf.
type change struct {
	path     string
	segs     []string
	old, new interface{}
}

// Compute diffs old against new. A nil old means the object was created; a nil
// new means it was deleted; both non-nil is an update.
func Compute(old, new map[string]interface{}) Result {
	switch {
	case old == nil && new != nil:
		return Result{Summary: "created"}
	case old != nil && new == nil:
		return Result{Summary: "deleted"}
	case old == nil && new == nil:
		return Result{Summary: "no-op"}
	}

	var changes []change
	walk("", nil, normalize(old), normalize(new), &changes)

	if len(changes) == 0 {
		return Result{Summary: "no-op"}
	}

	sort.Slice(changes, func(i, j int) bool { return changes[i].path < changes[j].path })

	fields := make([]string, len(changes))
	structured := make([]Change, len(changes))
	for i, c := range changes {
		fields[i] = c.path
		structured[i] = Change{Field: c.path, Path: c.segs}
	}
	return Result{ChangedFields: fields, Changes: structured, Summary: summarize(changes)}
}

// normalize returns a shallow-ish copy of obj with server-managed noise
// removed, so it does not pollute the diff.
func normalize(obj map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(obj))
	for k, v := range obj {
		switch k {
		case "status":
			// Status is controller-owned observed state, not a user change.
			continue
		case "metadata":
			if m, ok := v.(map[string]interface{}); ok {
				out[k] = normalizeMetadata(m)
				continue
			}
		}
		out[k] = v
	}
	return out
}

func normalizeMetadata(m map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		if ignoredMetadata[k] {
			continue
		}
		if k == "annotations" {
			if ann, ok := v.(map[string]interface{}); ok {
				cleaned := make(map[string]interface{}, len(ann))
				for ak, av := range ann {
					if !ignoredAnnotations[ak] {
						cleaned[ak] = av
					}
				}
				if len(cleaned) > 0 {
					out[k] = cleaned
				}
				continue
			}
		}
		out[k] = v
	}
	return out
}

// walk recursively compares a and b, appending leaf-level differences.
func walk(path string, segs []string, a, b interface{}, out *[]change) {
	if equalJSON(a, b) {
		return
	}

	am, aok := a.(map[string]interface{})
	bm, bok := b.(map[string]interface{})
	if aok || bok {
		for _, key := range unionKeys(am, bm) {
			walk(joinPath(path, key), appendSeg(segs, key), am[key], bm[key], out)
		}
		return
	}

	as, aok := a.([]interface{})
	bs, bok := b.([]interface{})
	if aok || bok {
		n := len(as)
		if len(bs) > n {
			n = len(bs)
		}
		for i := 0; i < n; i++ {
			var av, bv interface{}
			if i < len(as) {
				av = as[i]
			}
			if i < len(bs) {
				bv = bs[i]
			}
			walk(fmt.Sprintf("%s[%d]", path, i), appendSeg(segs, strconv.Itoa(i)), av, bv, out)
		}
		return
	}

	// Leaf (scalar, or a type mismatch): record the change.
	*out = append(*out, change{path: path, segs: segs, old: a, new: b})
}

// appendSeg copies, so sibling branches never share a backing array.
func appendSeg(segs []string, s string) []string {
	out := make([]string, len(segs)+1)
	copy(out, segs)
	out[len(segs)] = s
	return out
}

func summarize(changes []change) string {
	// Prefer a scalar-to-scalar change for a readable "path: old -> new".
	for _, c := range changes {
		oldStr, oldOK := scalarString(c.old)
		newStr, newOK := scalarString(c.new)
		if oldOK && newOK {
			return fmt.Sprintf("%s: %s -> %s", c.path, oldStr, newStr)
		}
	}
	if len(changes) == 1 {
		return fmt.Sprintf("%s changed", changes[0].path)
	}
	return fmt.Sprintf("%d fields changed", len(changes))
}

// scalarString renders a scalar value and reports whether it was a scalar.
// A nil value (field added or removed) is rendered as "<none>".
func scalarString(v interface{}) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "<none>", true
	case string, bool, int, int32, int64, float32, float64:
		return truncate(fmt.Sprintf("%v", t)), true
	default:
		return "", false
	}
}

func truncate(s string) string {
	if len(s) > maxSummaryValueLen {
		return s[:maxSummaryValueLen] + "…"
	}
	return s
}

func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

func unionKeys(a, b map[string]interface{}) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var keys []string
	for k := range a {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range b {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// equalJSON compares two decoded-JSON values for deep equality without caring
// about numeric representation (int64 vs float64), which unstructured mixes.
func equalJSON(a, b interface{}) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	switch at := a.(type) {
	case map[string]interface{}:
		bt, ok := b.(map[string]interface{})
		if !ok || len(at) != len(bt) {
			return false
		}
		for k, av := range at {
			bv, ok := bt[k]
			if !ok || !equalJSON(av, bv) {
				return false
			}
		}
		return true
	case []interface{}:
		bt, ok := b.([]interface{})
		if !ok || len(at) != len(bt) {
			return false
		}
		for i := range at {
			if !equalJSON(at[i], bt[i]) {
				return false
			}
		}
		return true
	case int, int32, int64, float32, float64:
		bf, ok := toFloat(b)
		af, _ := toFloat(a)
		return ok && af == bf
	default:
		return a == b
	}
}

func toFloat(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	case float32:
		return float64(t), true
	case float64:
		return t, true
	default:
		return 0, false
	}
}

// FieldsPreview returns the first n changed fields joined for logging.
func FieldsPreview(fields []string, n int) string {
	if len(fields) <= n {
		return strings.Join(fields, ", ")
	}
	return strings.Join(fields[:n], ", ") + fmt.Sprintf(", +%d more", len(fields)-n)
}
