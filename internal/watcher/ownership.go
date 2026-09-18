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

package watcher

import (
	"bytes"
	"reflect"
	"sort"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/structured-merge-diff/v4/fieldpath"

	"github.com/v1k0d3n/chronos/internal/diff"
)

// Who made a change is answered from what changed.
//
// managedFields records, per field manager, exactly which fields it owns. The
// old rule took the entry with the latest timestamp — whoever touched the
// object most recently — and that is the wrong question. A person edits a
// Deployment; the deployment controller writes a bookkeeping annotation to it
// a moment later; the controller is now "latest", the person's edit is dropped
// as controller noise, and Deployments end up nearly absent from the timeline.
//
// The right question is: who owns the fields that changed? That is what these
// functions answer.

// statusSubresource marks managedFields entries written through the status
// subresource, which say nothing about who changed the spec.
const statusSubresource = "status"

// owners maps a field manager's name to how many of the changed fields it owns.
type owners map[string]int

// managersOfChange returns, for an update, which managers own the fields that
// changed, counted per manager. A field nobody claims is not counted; if that
// leaves the map empty, ownership is unknown and the caller should not draw
// conclusions from it.
func managersOfChange(u *unstructured.Unstructured, changes []diff.Change) owners {
	sets := ownedSets(u)
	if len(sets) == 0 {
		return nil
	}
	out := owners{}
	for _, c := range changes {
		// A manager may claim a field exactly, or through an ancestor it owns
		// whole. managedFields cannot say whether an ancestor claim means "the
		// whole value" (an atomic struct) or only "the map exists" (its keys
		// belong to whoever set them); the two look the same. So the most
		// specific claim wins: if anyone owns the field more deeply, a
		// shallower claim is taken to be existence only.
		var best []string
		deepest := -1
		for _, s := range sets {
			depth, ok := owns(s.set, u.Object, c.Path)
			if !ok || depth < deepest {
				continue
			}
			if depth > deepest {
				best, deepest = best[:0], depth
			}
			best = append(best, s.manager)
		}
		for _, m := range best {
			out[m]++
		}
	}
	return out
}

// creatorOf returns the manager that created the object: the earliest
// non-status managedFields entry. Controllers that decorate an object after
// it is created come later, and a "latest wins" rule credited them with the
// creation.
func creatorOf(u *unstructured.Unstructured) (string, bool) {
	var best *metav1.ManagedFieldsEntry
	fields := u.GetManagedFields()
	for i := range fields {
		e := fields[i]
		if e.Subresource == statusSubresource || !mutating(e.Operation) {
			continue
		}
		if best == nil || earlierThan(e.Time, best.Time) {
			entry := e
			best = &entry
		}
	}
	if best == nil {
		return "", false
	}
	return best.Manager, true
}

// principal picks the manager to credit with a change: a human over a
// controller, and among equals, the one owning the most changed fields (ties
// broken by name, so the answer is stable).
func (o owners) principal() (manager string, allControllers bool) {
	if len(o) == 0 {
		return "", false
	}
	names := make([]string, 0, len(o))
	for m := range o {
		names = append(names, m)
	}
	sort.Strings(names)

	allControllers = true
	for _, m := range names {
		ctl := isControllerActor(m)
		if !ctl {
			allControllers = false
		}
		switch {
		case manager == "":
			manager = m
		case isControllerActor(manager) && !ctl:
			manager = m
		case isControllerActor(manager) == ctl && o[m] > o[manager]:
			manager = m
		}
	}
	return manager, allControllers
}

type ownedSet struct {
	manager string
	set     *fieldpath.Set
}

// ownedSets parses every mutating, non-status managedFields entry.
func ownedSets(u *unstructured.Unstructured) []ownedSet {
	var out []ownedSet
	for _, e := range u.GetManagedFields() {
		if e.Subresource == statusSubresource || !mutating(e.Operation) || e.FieldsV1 == nil {
			continue
		}
		s := &fieldpath.Set{}
		if err := s.FromJSON(bytes.NewReader(e.FieldsV1.Raw)); err != nil {
			continue
		}
		out = append(out, ownedSet{manager: e.Manager, set: s})
	}
	return out
}

// owns reports whether set claims the field at path in obj, or an ancestor
// of it, and how many path segments deep the claim is. Map keys become
// field-name elements. A list position becomes whichever key, value or index
// element the set uses for that item — which is resolved by looking at the
// item itself, so no schema is needed.
func owns(set *fieldpath.Set, node interface{}, path []string) (int, bool) {
	cur := set
	for depth, seg := range path {
		var pe fieldpath.PathElement
		var found bool
		switch n := node.(type) {
		case map[string]interface{}:
			name := seg
			pe = fieldpath.PathElement{FieldName: &name}
			found = true
			node = n[seg]
		case []interface{}:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(n) {
				return 0, false
			}
			node = n[idx]
			pe, found = elementFor(cur, node, idx)
		default:
			return 0, false
		}
		if !found {
			return 0, false
		}
		child, hasChildren := cur.Children.Get(pe)
		if cur.Members.Has(pe) && !hasChildren {
			// A member with nothing beneath it in the set: the leaf itself,
			// an atomic structure the API server tracks as one field, or a
			// map this manager created whose keys others now own. The caller
			// ranks claims by depth to tell the last case apart.
			return depth + 1, true
		}
		if !hasChildren {
			return 0, false
		}
		cur = child
	}
	// Walked the whole path and ended inside the set, on a node that has
	// children: the set owns things beneath this field, not the field.
	return 0, false
}

// elementFor finds the path element in cur that addresses item — by key
// fields for a list of maps, by value for a set of scalars, or by index.
func elementFor(cur *fieldpath.Set, item interface{}, idx int) (fieldpath.PathElement, bool) {
	var match fieldpath.PathElement
	found := false
	try := func(pe fieldpath.PathElement) {
		if found {
			return
		}
		switch {
		case pe.Key != nil:
			m, ok := item.(map[string]interface{})
			if !ok {
				return
			}
			for _, f := range *pe.Key {
				if !reflect.DeepEqual(normalizeScalar(m[f.Name]), normalizeScalar(f.Value.Unstructured())) {
					return
				}
			}
			match, found = pe, true
		case pe.Value != nil:
			if reflect.DeepEqual(normalizeScalar(item), normalizeScalar((*pe.Value).Unstructured())) {
				match, found = pe, true
			}
		case pe.Index != nil:
			if *pe.Index == idx {
				match, found = pe, true
			}
		}
	}
	cur.Members.Iterate(try)
	cur.Children.Iterate(try)
	return match, found
}

// normalizeScalar makes numbers comparable regardless of how they were decoded
// (int64 from unstructured, float64 or int from the fieldpath value).
func normalizeScalar(v interface{}) interface{} {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case float32:
		return float64(n)
	}
	return v
}

func mutating(op metav1.ManagedFieldsOperationType) bool {
	return op == metav1.ManagedFieldsOperationUpdate || op == metav1.ManagedFieldsOperationApply
}

func earlierThan(a, b *metav1.Time) bool {
	switch {
	case a == nil:
		return false
	case b == nil:
		return true
	default:
		return a.Before(b)
	}
}
