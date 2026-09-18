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

// Package redact strips sensitive values from an object BEFORE Chronos ever
// persists it, so the snapshot store never becomes a honeypot of Secrets,
// dockerconfigs, or tokens. Redacted values are replaced with a hash so a diff
// can still show "value changed" without revealing the value.
//
// What is redacted, on every watched kind:
//
//   - every value of a Secret's data and stringData;
//   - the kubectl.kubernetes.io/last-applied-configuration annotation, which
//     embeds a full copy of the object as it was applied;
//   - a ConfigMap entry, an environment variable, or an annotation whose key
//     looks like it names a credential (password, token, secret, key, …);
//   - any string, wherever it sits, whose value looks like a credential: a
//     private key block, a JWT, a URL carrying a password, a cloud or SaaS
//     token with a recognisable prefix;
//   - whatever a ChronosConfig additionally names, by key pattern or by path.
//
// Each redaction is recorded as an RFC 6901 JSON Pointer to the field, so the
// revert controller can leave exactly those fields alone when it re-applies a
// snapshot, and so the path is unambiguous for keys that contain dots or
// slashes (application.properties, kubectl.kubernetes.io/…).
package redact

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	chronosv1alpha1 "github.com/v1k0d3n/chronos/api/v1alpha1"
)

const redactedPlaceholder = ""

// lastAppliedAnnotation embeds the whole object as last applied — including,
// for a Secret, its data — so it is dropped from every kind.
const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// Policy is what to redact beyond the built-in rules.
type Policy struct {
	// KeyPatterns are matched against ConfigMap keys, environment variable
	// names and annotation keys. A match redacts the value.
	KeyPatterns []*regexp.Regexp
	// FieldPaths are JSON Pointers to fields that are always redacted when
	// present, on any kind.
	FieldPaths []string
	// Key signs the hashes. Nil falls back to a key that lives only in this
	// process (see EphemeralKey).
	Key *Key
}

// builtinKeyPattern names a credential by its key. It is deliberately about
// credentials rather than everything security-adjacent: "ca.crt" and
// "tls.crt" are public material whose changes are worth seeing.
var builtinKeyPattern = regexp.MustCompile(`(?i)(` +
	`passw(or)?d|passphrase|secret|token|api[_-]?key|access[_-]?key|` +
	`private[_-]?key|credential|authorization|bearer|` +
	`\.key$|_key$|-key$|dsn|connection[_-]?string` +
	`)`)

// builtinValuePatterns recognise a credential by its shape, wherever it sits.
var builtinValuePatterns = []*regexp.Regexp{
	// PEM private keys (RSA, EC, OpenSSH, PKCS#8, encrypted, …).
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	// A JWT: three base64url segments, the first two JSON ("eyJ" is `{"`).
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
	// A URL with a password in it: scheme://user:password@host.
	regexp.MustCompile(`[a-z][a-z0-9+.-]*://[^/\s:@]+:[^/\s@]+@`),
	// Tokens with a recognisable prefix: AWS access key, GitHub, GitLab, Slack,
	// OpenAI-style, HashiCorp Vault, OpenShift OAuth.
	regexp.MustCompile(`\b(AKIA[0-9A-Z]{16}|ghp_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{22,}|` +
		`glpat-[A-Za-z0-9_-]{20,}|xox[abpr]-[A-Za-z0-9-]{10,}|sk-[A-Za-z0-9]{20,}|` +
		`hvs\.[A-Za-z0-9]{20,}|sha256~[A-Za-z0-9_-]{20,})`),
}

// Redact applies the built-in rules only. See RedactWith.
func Redact(u *unstructured.Unstructured) (*unstructured.Unstructured, []chronosv1alpha1.RedactionEntry, bool) {
	return RedactWith(u, Policy{})
}

// RedactWith returns a deep copy of u with sensitive values removed, the list
// of redactions performed, and whether anything was redacted. The input is
// never mutated.
func RedactWith(u *unstructured.Unstructured, policy Policy) (*unstructured.Unstructured, []chronosv1alpha1.RedactionEntry, bool) {
	red := u.DeepCopy()
	if policy.Key == nil {
		policy.Key = fallbackKey
	}
	r := &redactor{policy: policy}

	if isSecret(red) {
		secretType, _, _ := unstructured.NestedString(red.Object, "type")
		r.redactStringMap(red.Object, "data", secretType)
		r.redactStringMap(red.Object, "stringData", secretType)
	}

	// The last-applied annotation is removed outright rather than blanked: a
	// blank string there would itself be a lie about what was applied.
	if removeAnnotation(red, lastAppliedAnnotation) {
		r.record("/metadata/annotations/"+escape(lastAppliedAnnotation), chronosv1alpha1.RedactionLastApplied, nil)
	}

	for _, p := range policy.FieldPaths {
		r.redactPointer(red.Object, p)
	}

	r.walk(red.Object, "", "", isConfigMap(red))

	sort.Slice(r.entries, func(i, j int) bool { return r.entries[i].FieldPath < r.entries[j].FieldPath })
	return red, r.entries, len(r.entries) > 0
}

type redactor struct {
	policy  Policy
	entries []chronosv1alpha1.RedactionEntry
	done    map[string]bool
}

func (r *redactor) record(path string, reason chronosv1alpha1.RedactionReason, val interface{}) {
	if r.done == nil {
		r.done = map[string]bool{}
	}
	if r.done[path] {
		return
	}
	r.done[path] = true
	e := chronosv1alpha1.RedactionEntry{FieldPath: path, Reason: reason}
	if val != nil {
		e.Hash = r.policy.Key.hash(val)
		e.KeyID = r.policy.Key.ID
	}
	r.entries = append(r.entries, e)
}

// redactStringMap blanks every value under obj[field] and records a hash of
// each. field is "data" (base64 values) or "stringData" (plaintext values).
func (r *redactor) redactStringMap(obj map[string]interface{}, field, secretType string) {
	raw, found, err := unstructured.NestedMap(obj, field)
	if err != nil || !found || len(raw) == 0 {
		return
	}
	for key, val := range raw {
		r.record("/"+field+"/"+escape(key), reasonFor(secretType, key), val)
		raw[key] = redactedPlaceholder
	}
	_ = unstructured.SetNestedMap(obj, raw, field)
}

// redactPointer redacts the field at a configured JSON Pointer, if present.
func (r *redactor) redactPointer(obj map[string]interface{}, pointer string) {
	segs, err := Split(pointer)
	if err != nil || len(segs) == 0 {
		return
	}
	parent, last, ok := descend(obj, segs)
	if !ok {
		return
	}
	switch p := parent.(type) {
	case map[string]interface{}:
		val, present := p[last]
		if !present {
			return
		}
		r.record(pointer, chronosv1alpha1.RedactionPolicy, val)
		p[last] = redactedPlaceholder
	case []interface{}:
		i, err := strconv.Atoi(last)
		if err != nil || i < 0 || i >= len(p) {
			return
		}
		r.record(pointer, chronosv1alpha1.RedactionPolicy, p[i])
		p[i] = redactedPlaceholder
	}
}

// walk visits every value, redacting by key where the key names a credential
// and by shape wherever a string looks like one.
//
// parentKey is the key this node sits under, so that entries of an "env" list
// and of an "annotations" map are recognised wherever they appear — in a Pod
// template inside a Deployment as much as at the top level.
func (r *redactor) walk(node interface{}, path, parentKey string, configMap bool) {
	switch n := node.(type) {
	case map[string]interface{}:
		// An env entry: {name, value} (or valueFrom, which is a reference and
		// carries no secret itself).
		if parentKey == "env" {
			name, _ := n["name"].(string)
			if val, ok := n["value"].(string); ok && name != "" {
				if r.keyMatches(name) || valueMatches(val) {
					r.record(path+"/value", chronosv1alpha1.RedactionSensitivePattern, val)
					n["value"] = redactedPlaceholder
					return
				}
			}
		}
		for k, v := range n {
			child := path + "/" + escape(k)
			// Maps whose keys are user-chosen names: annotations anywhere,
			// and a ConfigMap's data. Their keys can name credentials.
			if k == "annotations" || (configMap && path == "" && (k == "data" || k == "binaryData")) {
				m, ok := v.(map[string]interface{})
				if !ok {
					continue
				}
				for key, val := range m {
					s, isStr := val.(string)
					if !isStr {
						continue
					}
					if r.keyMatches(key) || valueMatches(s) {
						r.record(child+"/"+escape(key), chronosv1alpha1.RedactionSensitivePattern, s)
						m[key] = redactedPlaceholder
					}
				}
				continue
			}
			r.walkValue(n, k, v, child, k, configMap)
		}
	case []interface{}:
		for i, v := range n {
			child := path + "/" + strconv.Itoa(i)
			switch cv := v.(type) {
			case map[string]interface{}, []interface{}:
				r.walk(cv, child, parentKey, configMap)
			case string:
				if valueMatches(cv) {
					r.record(child, chronosv1alpha1.RedactionSensitivePattern, cv)
					n[i] = redactedPlaceholder
				}
			}
		}
	}
}

func (r *redactor) walkValue(parent map[string]interface{}, key string, v interface{}, path, parentKey string, configMap bool) {
	switch cv := v.(type) {
	case map[string]interface{}, []interface{}:
		r.walk(cv, path, parentKey, configMap)
	case string:
		if valueMatches(cv) {
			r.record(path, chronosv1alpha1.RedactionSensitivePattern, cv)
			parent[key] = redactedPlaceholder
		}
	}
}

func (r *redactor) keyMatches(key string) bool {
	if builtinKeyPattern.MatchString(key) {
		return true
	}
	for _, p := range r.policy.KeyPatterns {
		if p.MatchString(key) {
			return true
		}
	}
	return false
}

func valueMatches(s string) bool {
	for _, p := range builtinValuePatterns {
		if p.MatchString(s) {
			return true
		}
	}
	return false
}

func isSecret(u *unstructured.Unstructured) bool {
	gvk := u.GroupVersionKind()
	return gvk.Group == "" && gvk.Kind == "Secret"
}

func isConfigMap(u *unstructured.Unstructured) bool {
	gvk := u.GroupVersionKind()
	return gvk.Group == "" && gvk.Kind == "ConfigMap"
}

func reasonFor(secretType, key string) chronosv1alpha1.RedactionReason {
	switch {
	case secretType == "kubernetes.io/dockerconfigjson" || key == ".dockerconfigjson",
		secretType == "kubernetes.io/dockercfg" || key == ".dockercfg":
		return chronosv1alpha1.RedactionDockerConfig
	case secretType == "kubernetes.io/service-account-token" || key == "token":
		return chronosv1alpha1.RedactionServiceAccountToken
	default:
		return chronosv1alpha1.RedactionSecretData
	}
}

// fallbackKey is used when a policy names no key. Generated once per process.
var fallbackKey = EphemeralKey()

func removeAnnotation(u *unstructured.Unstructured, key string) bool {
	ann := u.GetAnnotations()
	if _, ok := ann[key]; !ok {
		return false
	}
	delete(ann, key)
	u.SetAnnotations(ann)
	return true
}

// --- JSON Pointer (RFC 6901) ------------------------------------------------

func escape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func unescape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
}

// Split parses a JSON Pointer into its reference tokens.
func Split(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("%q is not a JSON Pointer: it must start with /", pointer)
	}
	parts := strings.Split(pointer[1:], "/")
	for i := range parts {
		parts[i] = unescape(parts[i])
	}
	return parts, nil
}

// descend follows all but the last token and returns the container the last
// token addresses, with the last token.
func descend(obj map[string]interface{}, segs []string) (parent interface{}, last string, ok bool) {
	var cur interface{} = obj
	for _, s := range segs[:len(segs)-1] {
		switch c := cur.(type) {
		case map[string]interface{}:
			cur, ok = c[s]
			if !ok {
				return nil, "", false
			}
		case []interface{}:
			i, err := strconv.Atoi(s)
			if err != nil || i < 0 || i >= len(c) {
				return nil, "", false
			}
			cur = c[i]
		default:
			return nil, "", false
		}
	}
	return cur, segs[len(segs)-1], true
}

// Remove deletes the field a JSON Pointer addresses from obj, then prunes any
// map left empty by the removal, so that a later server-side apply does not
// claim an empty container. It reports whether anything was removed.
//
// This is how the revert controller honours redactions: a field whose value
// was never stored is left out of the apply entirely, and the live value —
// whatever it is — stays as it is.
func Remove(obj map[string]interface{}, pointer string) bool {
	segs, err := Split(pointer)
	if err != nil || len(segs) == 0 {
		return false
	}
	return removeSegs(obj, segs)
}

func removeSegs(node interface{}, segs []string) bool {
	switch c := node.(type) {
	case map[string]interface{}:
		if len(segs) == 1 {
			if _, present := c[segs[0]]; !present {
				return false
			}
			delete(c, segs[0])
			return true
		}
		child, present := c[segs[0]]
		if !present {
			return false
		}
		removed := removeSegs(child, segs[1:])
		if cm, isMap := child.(map[string]interface{}); isMap && len(cm) == 0 {
			delete(c, segs[0])
		}
		return removed
	case []interface{}:
		i, err := strconv.Atoi(segs[0])
		if err != nil || i < 0 || i >= len(c) {
			return false
		}
		if len(segs) == 1 {
			// Removing an element would renumber its siblings and so invalidate
			// every other pointer into this list; blank it instead.
			c[i] = redactedPlaceholder
			return true
		}
		return removeSegs(c[i], segs[1:])
	}
	return false
}
