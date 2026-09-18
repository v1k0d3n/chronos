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

package redact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	chronosv1alpha1 "github.com/v1k0d3n/chronos/api/v1alpha1"
)

func secret(secretType string, data map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]interface{}{"name": "creds", "namespace": "team-a"},
		"type":       secretType,
		"data":       data,
	}}
}

func TestRedact_SecretDataBlankedAndHashed(t *testing.T) {
	orig := secret("Opaque", map[string]interface{}{
		"password": "c3VwZXItc2VjcmV0",
		"username": "YWxpY2U=",
	})

	red, entries, wasRedacted := Redact(orig)

	if !wasRedacted {
		t.Fatal("expected Secret to be redacted")
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 redaction entries, got %d", len(entries))
	}
	for _, e := range entries {
		if e.Hash == "" {
			t.Errorf("entry %s has empty hash", e.FieldPath)
		}
		if e.Reason != chronosv1alpha1.RedactionSecretData {
			t.Errorf("entry %s reason = %q, want secret-data", e.FieldPath, e.Reason)
		}
	}

	// Redacted copy must have blank values...
	data, _, _ := unstructured.NestedMap(red.Object, "data")
	for k, v := range data {
		if v != "" {
			t.Errorf("redacted data[%s] = %q, want empty", k, v)
		}
	}
	// ...and the original must be untouched.
	origData, _, _ := unstructured.NestedMap(orig.Object, "data")
	if origData["password"] != "c3VwZXItc2VjcmV0" {
		t.Errorf("original was mutated: %v", origData["password"])
	}
}

func TestRedact_HashesAreKeyed(t *testing.T) {
	k1, k2 := NewKey([]byte("first-installation-key-material!")), NewKey([]byte("other-installation-key-material!"))

	// Same key, same value: comparable across snapshots.
	first, again := k1.hash("c3VwZXItc2VjcmV0"), k1.hash("c3VwZXItc2VjcmV0")
	if first != again {
		t.Error("hash not deterministic under one key")
	}
	if k1.hash("one") == k1.hash("two") {
		t.Error("distinct values hashed to the same digest")
	}
	// Different key, same value: unrelated. This is the property that makes a
	// snapshot useless for testing guesses offline.
	if k1.hash("c3VwZXItc2VjcmV0") == k2.hash("c3VwZXItc2VjcmV0") {
		t.Error("the hash does not depend on the key")
	}
	// A key's ID reveals nothing of the key and differs between keys.
	if k1.ID == k2.ID || len(k1.ID) != 8 || strings.Contains(k1.ID, "first") {
		t.Errorf("key IDs: %q %q", k1.ID, k2.ID)
	}
	// It is not the old scheme: SHA-256 over any public salt and the value
	// would be reproducible by anyone. Check the digest is not a plain hash of
	// the value under the salt that used to be used.
	old := sha256.Sum256([]byte("chronos.ocp.run/v1:c3VwZXItc2VjcmV0"))
	if k1.hash("c3VwZXItc2VjcmV0") == hex.EncodeToString(old[:])[:12] {
		t.Error("hash is still the public-salt scheme")
	}
}

func TestRedact_EntriesCarryTheKeyID(t *testing.T) {
	key := NewKey([]byte("first-installation-key-material!"))
	_, entries, _ := RedactWith(secret("Opaque", map[string]interface{}{"password": "eA=="}), Policy{Key: key})
	if len(entries) != 1 || entries[0].KeyID != key.ID || entries[0].Hash != key.hash("eA==") {
		t.Fatalf("entry = %+v, want hash under key %s", entries, key.ID)
	}

	// No key configured: still hashed, under a key private to this process.
	_, entries, _ = Redact(secret("Opaque", map[string]interface{}{"password": "eA=="}))
	if entries[0].KeyID == "" || entries[0].KeyID == key.ID || entries[0].Hash == "" {
		t.Fatalf("fallback entry = %+v", entries[0])
	}
}

func TestLoadOrCreateKey(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	first, err := LoadOrCreateKey(ctx, c, "chronos-system")
	if err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "chronos-system", Name: KeySecretName}, sec); err != nil {
		t.Fatalf("the key was not stored: %v", err)
	}
	if len(sec.Data["key"]) != 32 {
		t.Errorf("stored key is %d bytes, want 32", len(sec.Data["key"]))
	}

	// A second start finds the same key, so hashes stay comparable.
	second, err := LoadOrCreateKey(ctx, c, "chronos-system")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.hash("x") != first.hash("x") {
		t.Error("a restart produced a different key")
	}

	// A rotated (deleted) Secret yields a new key with a new ID.
	if err := c.Delete(ctx, sec); err != nil {
		t.Fatal(err)
	}
	third, err := LoadOrCreateKey(ctx, c, "chronos-system")
	if err != nil {
		t.Fatal(err)
	}
	if third.ID == first.ID {
		t.Error("rotation did not change the key")
	}

	// A Secret someone emptied is refused rather than used as a weak key.
	sec = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: KeySecretName}, Data: map[string][]byte{"key": []byte("short")}}
	if err := c.Create(ctx, sec); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(ctx, c, "other"); err == nil {
		t.Error("a 5-byte key was accepted")
	}
}

func TestRedact_DockerConfigReason(t *testing.T) {
	orig := secret("kubernetes.io/dockerconfigjson", map[string]interface{}{
		".dockerconfigjson": "eyJhdXRocyI6e319",
	})
	_, entries, _ := Redact(orig)
	if len(entries) != 1 || entries[0].Reason != chronosv1alpha1.RedactionDockerConfig {
		t.Fatalf("expected dockerconfig reason, got %+v", entries)
	}
}

func TestRedact_LastAppliedAnnotationStripped(t *testing.T) {
	orig := secret("Opaque", map[string]interface{}{"password": "eA=="})
	meta, _, _ := unstructured.NestedMap(orig.Object, "metadata")
	meta["annotations"] = map[string]interface{}{
		"kubectl.kubernetes.io/last-applied-configuration": `{"data":{"password":"eA=="}}`,
	}
	_ = unstructured.SetNestedMap(orig.Object, meta, "metadata")

	red, _, _ := Redact(orig)
	ann := red.GetAnnotations()
	if _, present := ann["kubectl.kubernetes.io/last-applied-configuration"]; present {
		t.Error("last-applied-configuration annotation should be stripped from Secrets")
	}
}

// --- beyond Secrets -----------------------------------------------------------

func obj(kind string, spec map[string]interface{}) *unstructured.Unstructured {
	o := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       kind,
		"metadata":   map[string]interface{}{"name": "x", "namespace": "team-a"},
	}
	for k, v := range spec {
		o[k] = v
	}
	return &unstructured.Unstructured{Object: o}
}

func paths(entries []chronosv1alpha1.RedactionEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.FieldPath)
	}
	return out
}

func want(t *testing.T, got []string, exp ...string) {
	t.Helper()
	if len(got) != len(exp) {
		t.Fatalf("redacted paths = %v, want %v", got, exp)
	}
	for i := range exp {
		if got[i] != exp[i] {
			t.Fatalf("redacted paths = %v, want %v", got, exp)
		}
	}
}

// A ConfigMap key that names a credential is redacted; one that does not is
// kept, whatever it contains, unless the value itself looks like a credential.
func TestRedact_ConfigMapByKeyAndByValue(t *testing.T) {
	cm := obj("ConfigMap", map[string]interface{}{"data": map[string]interface{}{
		"application.properties": "server.port=8080",
		"DB_PASSWORD":            "hunter2",
		"api-key":                "abc",
		"ca.crt":                 "-----BEGIN CERTIFICATE-----\nMIIB...",         // public: kept
		"id_rsa":                 "-----BEGIN OPENSSH PRIVATE KEY-----\nb3Bl...", // shape: redacted
		"database.url":           "postgres://app:s3cret@db:5432/app",            // shape: redacted
	}})

	red, entries, _ := Redact(cm)

	want(t, paths(entries), "/data/DB_PASSWORD", "/data/api-key", "/data/database.url", "/data/id_rsa")
	data, _, _ := unstructured.NestedStringMap(red.Object, "data")
	if data["application.properties"] != "server.port=8080" || data["ca.crt"] == "" {
		t.Errorf("non-credential entries were altered: %v", data)
	}
	if data["DB_PASSWORD"] != "" || data["database.url"] != "" {
		t.Errorf("credential entries survived: %v", data)
	}
	for _, e := range entries {
		if e.Reason != chronosv1alpha1.RedactionSensitivePattern || e.Hash == "" {
			t.Errorf("entry %+v: want reason sensitive-pattern with a hash", e)
		}
	}
}

// Environment variables in a Pod template — however deep — are redacted by
// name or by value. A valueFrom reference carries no secret and is kept.
func TestRedact_DeploymentEnv(t *testing.T) {
	dep := obj("Deployment", map[string]interface{}{"spec": map[string]interface{}{
		"replicas": int64(3),
		"template": map[string]interface{}{"spec": map[string]interface{}{
			"initContainers": []interface{}{map[string]interface{}{
				"name": "migrate",
				"env": []interface{}{
					map[string]interface{}{"name": "DATABASE_PASSWORD", "value": "hunter2"},
				},
			}},
			"containers": []interface{}{map[string]interface{}{
				"name": "api",
				"env": []interface{}{
					map[string]interface{}{"name": "LOG_LEVEL", "value": "debug"},
					map[string]interface{}{"name": "SESSION_TOKEN", "value": "abc"},
					map[string]interface{}{"name": "UPSTREAM", "value": "https://svc:8443/"},
					map[string]interface{}{"name": "GITHUB", "value": "ghp_0123456789abcdefghijklmnopqrstuvwxyz"},
					map[string]interface{}{"name": "FROM_SECRET", "valueFrom": map[string]interface{}{
						"secretKeyRef": map[string]interface{}{"name": "creds", "key": "password"},
					}},
				},
			}},
		}},
	}})
	dep.Object["apiVersion"] = "apps/v1"

	red, entries, _ := Redact(dep)

	want(t, paths(entries),
		"/spec/template/spec/containers/0/env/1/value",
		"/spec/template/spec/containers/0/env/3/value",
		"/spec/template/spec/initContainers/0/env/0/value",
	)
	envs, _, _ := unstructured.NestedSlice(red.Object, "spec", "template", "spec", "containers", "0", "env")
	if envs == nil {
		// NestedSlice does not index lists; walk by hand.
		containers, _, _ := unstructured.NestedSlice(red.Object, "spec", "template", "spec", "containers")
		envs = containers[0].(map[string]interface{})["env"].([]interface{})
	}
	if envs[0].(map[string]interface{})["value"] != "debug" || envs[2].(map[string]interface{})["value"] != "https://svc:8443/" {
		t.Errorf("harmless env values were altered: %v", envs)
	}
	if envs[1].(map[string]interface{})["value"] != "" {
		t.Errorf("SESSION_TOKEN survived: %v", envs[1])
	}
	if _, ok := envs[4].(map[string]interface{})["valueFrom"]; !ok {
		t.Errorf("valueFrom reference was removed: %v", envs[4])
	}
	replicas, _, _ := unstructured.NestedInt64(red.Object, "spec", "replicas")
	if replicas != 3 {
		t.Errorf("unrelated spec field changed: replicas=%d", replicas)
	}
}

// Annotations can carry credentials on any kind, and the last-applied
// annotation carries the whole object. Both are handled everywhere, not only
// on Secrets.
func TestRedact_AnnotationsOnAnyKind(t *testing.T) {
	svc := obj("Service", map[string]interface{}{"spec": map[string]interface{}{"type": "ClusterIP"}})
	meta := svc.Object["metadata"].(map[string]interface{})
	meta["annotations"] = map[string]interface{}{
		"kubectl.kubernetes.io/last-applied-configuration": `{"metadata":{"annotations":{"x/token":"abc"}}}`,
		"x/token":       "abc",
		"description":   "the payments service",
		"x/webhook-url": "https://user:pw@hooks.example.com/",
	}

	red, entries, _ := Redact(svc)

	want(t, paths(entries),
		"/metadata/annotations/kubectl.kubernetes.io~1last-applied-configuration",
		"/metadata/annotations/x~1token",
		"/metadata/annotations/x~1webhook-url",
	)
	ann := red.GetAnnotations()
	if _, present := ann["kubectl.kubernetes.io/last-applied-configuration"]; present {
		t.Error("last-applied-configuration must be removed from every kind")
	}
	if ann["description"] != "the payments service" || ann["x/token"] != "" {
		t.Errorf("annotations wrong after redaction: %v", ann)
	}
	if entries[0].Reason != chronosv1alpha1.RedactionLastApplied || entries[0].Hash != "" {
		t.Errorf("last-applied entry = %+v; want reason last-applied and no hash", entries[0])
	}
}

// An ordinary object with nothing credential-like in it is stored as is.
func TestRedact_PlainObjectUntouched(t *testing.T) {
	cm := obj("ConfigMap", map[string]interface{}{"data": map[string]interface{}{
		"nginx.conf": "server { listen 8080; }",
		"key-format": "json", // "key" inside a word is not a credential key
	}})
	_, entries, wasRedacted := Redact(cm)
	if wasRedacted {
		t.Errorf("nothing here is a credential, yet redacted: %v", paths(entries))
	}
}

// A ChronosConfig can add key patterns and name whole fields.
func TestRedactWith_Policy(t *testing.T) {
	cm := obj("ConfigMap", map[string]interface{}{"data": map[string]interface{}{
		"license":     "ABCD-EFGH",
		"config.yaml": "db:\n  host: db\n",
		"readme":      "hello",
	}})
	policy := Policy{
		KeyPatterns: []*regexp.Regexp{regexp.MustCompile(`(?i)^licen[cs]e$`)},
		FieldPaths:  []string{"/data/config.yaml", "/data/does-not-exist"},
	}

	red, entries, _ := RedactWith(cm, policy)

	want(t, paths(entries), "/data/config.yaml", "/data/license")
	data, _, _ := unstructured.NestedStringMap(red.Object, "data")
	if data["readme"] != "hello" || data["license"] != "" || data["config.yaml"] != "" {
		t.Errorf("data after policy redaction: %v", data)
	}
	for _, e := range entries {
		if e.FieldPath == "/data/config.yaml" && e.Reason != chronosv1alpha1.RedactionPolicy {
			t.Errorf("policy-named field has reason %q", e.Reason)
		}
	}
}

// Paths must round-trip through keys with dots, slashes and tildes.
func TestPointerRoundTrip(t *testing.T) {
	for _, key := range []string{"application.properties", "kubectl.kubernetes.io/last-applied-configuration", "a~b/c"} {
		segs, err := Split("/metadata/annotations/" + escape(key))
		if err != nil || len(segs) != 3 || segs[2] != key {
			t.Errorf("%q did not round-trip: %v %v", key, segs, err)
		}
	}
	if _, err := Split("data.password"); err == nil {
		t.Error("a dotted path is not a pointer and must be rejected")
	}
}

// Remove is what the revert controller uses to leave a redacted field out of
// an apply. It must take exactly the field, prune a map it empties, and never
// renumber a list.
func TestRemove(t *testing.T) {
	o := map[string]interface{}{
		"data": map[string]interface{}{"password": "", "host": "db"},
		"spec": map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{
			"containers": []interface{}{map[string]interface{}{
				"env": []interface{}{
					map[string]interface{}{"name": "A", "value": "1"},
					map[string]interface{}{"name": "PASSWORD", "value": ""},
					map[string]interface{}{"name": "C", "value": "3"},
				},
			}},
		}}},
		"stringData": map[string]interface{}{"token": ""},
	}

	if !Remove(o, "/data/password") || !Remove(o, "/stringData/token") ||
		!Remove(o, "/spec/template/spec/containers/0/env/1/value") {
		t.Fatal("expected each removal to report success")
	}
	if Remove(o, "/data/nope") || Remove(o, "/nowhere/at/all") {
		t.Error("removing an absent field must report false")
	}

	data := o["data"].(map[string]interface{})
	if _, present := data["password"]; present || data["host"] != "db" {
		t.Errorf("data after removal: %v", data)
	}
	if _, present := o["stringData"]; present {
		t.Error("a map emptied by removal must be pruned, or apply would claim it")
	}
	env := o["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})["env"].([]interface{})
	if len(env) != 3 {
		t.Fatalf("list was renumbered: %v", env)
	}
	entry := env[1].(map[string]interface{})
	if _, present := entry["value"]; present || entry["name"] != "PASSWORD" {
		t.Errorf("env entry after removal: %v", entry)
	}
}
