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

// Package config resolves the layered Chronos noise-filter policy — built-in
// defaults, overlaid by the cluster ChronosConfig defaults, overlaid by
// per-namespace overrides — into a concrete FilterSettings the watcher applies
// at record time. Everything keys off stable Kubernetes concepts (namespace
// globs, Secret types, field-path prefixes) so it is version-agnostic.
package config

import (
	"os"
	"regexp"
	"strings"

	chronosv1alpha1 "github.com/v1k0d3n/chronos/api/v1alpha1"
	"github.com/v1k0d3n/chronos/internal/redact"
)

// FilterSettings is a fully-resolved policy for a single namespace.
type FilterSettings struct {
	ExcludeControllerActors bool
	ExcludeNoiseSecrets     bool
	ExcludeNamespaces       []string
	ExcludeSecretTypes      map[string]bool
	ExcludeFields           []string
	IncludeFields           []string
	// Redaction is what to strip beyond the built-in rules, compiled once.
	Redaction redact.Policy
}

// Resolver holds the resolved global policy and per-namespace overrides.
type Resolver struct {
	global    FilterSettings
	overrides map[string]FilterSettings
}

// NewResolver builds a Resolver from an optional ChronosConfig (nil = built-in
// defaults only).
func NewResolver(cfg *chronosv1alpha1.ChronosConfig) *Resolver {
	global := applyPolicy(builtinDefaults(), defaultsOf(cfg))
	overrides := map[string]FilterSettings{}
	if cfg != nil {
		for _, ns := range cfg.Spec.NamespaceOverrides {
			overrides[ns.Namespace] = applyPolicy(global, ns.FilterPolicy)
		}
	}
	return &Resolver{global: global, overrides: overrides}
}

// EffectiveFor returns the resolved settings for a namespace.
func (r *Resolver) EffectiveFor(namespace string) FilterSettings {
	if namespace != "" {
		if fs, ok := r.overrides[namespace]; ok {
			return fs
		}
	}
	return r.global
}

func defaultsOf(cfg *chronosv1alpha1.ChronosConfig) chronosv1alpha1.FilterPolicy {
	if cfg == nil {
		return chronosv1alpha1.FilterPolicy{}
	}
	return cfg.Spec.Defaults
}

func builtinDefaults() FilterSettings {
	return FilterSettings{
		ExcludeControllerActors: envBool("CHRONOS_EXCLUDE_CONTROLLER_ACTORS", true),
		ExcludeNoiseSecrets:     envBool("CHRONOS_EXCLUDE_NOISE_SECRETS", true),
		ExcludeNamespaces:       defaultExcludeNamespaces(),
		ExcludeSecretTypes: map[string]bool{
			"kubernetes.io/service-account-token": true,
			"kubernetes.io/dockercfg":             true,
			"kubernetes.io/dockerconfigjson":      true,
		},
	}
}

// defaultExcludeNamespaces seeds the namespace globs; overridable via
// CHRONOS_IGNORE_NAMESPACES (comma-separated; trailing "*" = prefix).
func defaultExcludeNamespaces() []string {
	raw := os.Getenv("CHRONOS_IGNORE_NAMESPACES")
	if raw == "" {
		raw = "openshift-*,kube-*,openshift,default,chronos-system,chronos-demo"
	}
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// applyPolicy overlays a FilterPolicy onto base. A nil pointer / omitted slice
// inherits; a set value replaces.
func applyPolicy(base FilterSettings, p chronosv1alpha1.FilterPolicy) FilterSettings {
	out := base
	if p.ExcludeControllerActors != nil {
		out.ExcludeControllerActors = *p.ExcludeControllerActors
	}
	if p.ExcludeNoiseSecrets != nil {
		out.ExcludeNoiseSecrets = *p.ExcludeNoiseSecrets
	}
	if p.ExcludeNamespaces != nil {
		out.ExcludeNamespaces = append([]string(nil), p.ExcludeNamespaces...)
	}
	if p.ExcludeSecretTypes != nil {
		m := make(map[string]bool, len(p.ExcludeSecretTypes))
		for _, t := range p.ExcludeSecretTypes {
			m[t] = true
		}
		out.ExcludeSecretTypes = m
	}
	if p.ExcludeFields != nil {
		out.ExcludeFields = append([]string(nil), p.ExcludeFields...)
	}
	if p.IncludeFields != nil {
		out.IncludeFields = append([]string(nil), p.IncludeFields...)
	}
	// Redaction rules accumulate: a namespace can only add to what the cluster
	// redacts, never make it store more.
	if len(p.RedactKeys) > 0 || len(p.RedactFields) > 0 {
		red := redact.Policy{
			KeyPatterns: append([]*regexp.Regexp(nil), out.Redaction.KeyPatterns...),
			FieldPaths:  append([]string(nil), out.Redaction.FieldPaths...),
		}
		for _, expr := range p.RedactKeys {
			// A pattern that does not compile is skipped rather than fatal:
			// refusing the whole config over a typo would drop every other
			// rule in it, which stores more, not less.
			if re, err := regexp.Compile(expr); err == nil {
				red.KeyPatterns = append(red.KeyPatterns, re)
			}
		}
		for _, ptr := range p.RedactFields {
			if _, err := redact.Split(ptr); err == nil && ptr != "" {
				red.FieldPaths = append(red.FieldPaths, ptr)
			}
		}
		out.Redaction = red
	}
	return out
}

// NamespaceIgnored reports whether changes in ns should be dropped. Empty
// namespace (cluster-scoped) is never ignored here.
func (fs FilterSettings) NamespaceIgnored(ns string) bool {
	if ns == "" {
		return false
	}
	for _, pattern := range fs.ExcludeNamespaces {
		if matchGlob(pattern, ns) {
			return true
		}
	}
	return false
}

// SecretTypeExcluded reports whether a Secret of this type is filtered.
func (fs FilterSettings) SecretTypeExcluded(secretType string) bool {
	return fs.ExcludeSecretTypes[secretType]
}

// MeaningfulFields returns the changed fields that survive the field
// include/exclude policy. An empty result means the change carries no
// meaningful field change and should be dropped.
func (fs FilterSettings) MeaningfulFields(fields []string) []string {
	if len(fs.ExcludeFields) == 0 {
		return fields
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if !fs.fieldExcluded(f) {
			out = append(out, f)
		}
	}
	return out
}

func (fs FilterSettings) fieldExcluded(field string) bool {
	excluded := false
	for _, p := range fs.ExcludeFields {
		if pathHasPrefix(field, p) {
			excluded = true
			break
		}
	}
	if !excluded {
		return false
	}
	for _, p := range fs.IncludeFields {
		if pathHasPrefix(field, p) {
			return false
		}
	}
	return true
}

// pathHasPrefix reports whether a field path is at or under a prefix, treating
// "." and "[" as path boundaries (so "spec" matches "spec.replicas" but not
// "specials").
func pathHasPrefix(field, prefix string) bool {
	if field == prefix {
		return true
	}
	return strings.HasPrefix(field, prefix+".") || strings.HasPrefix(field, prefix+"[")
}

func matchGlob(pattern, s string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(s, strings.TrimSuffix(pattern, "*"))
	}
	return s == pattern
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "":
		return def
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return def
	}
}
