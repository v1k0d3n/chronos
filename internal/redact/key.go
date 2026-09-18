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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A redaction hash exists so a diff can say "this secret changed" without
// holding the secret. It used to be SHA-256 over a fixed, public salt and the
// value. That is an oracle: anyone who can read a snapshot can hash candidate
// passwords offline and compare — and secrets are often low-entropy. The hash
// is now an HMAC under a key that exists only inside the cluster, so a
// snapshot on its own proves nothing about the value.
//
// The key is per installation, generated on first start into a Secret in the
// operator's namespace. Hashes are only comparable under the same key, so each
// carries the key's ID; a rotation (delete the Secret, restart) reads as "not
// comparable" rather than as a false "changed".

const (
	// KeySecretName is the Secret holding the key.
	KeySecretName = "chronos-redaction-key"
	keyField      = "key"
	keyBytes      = 32
)

// Key is a redaction key with its identifier.
type Key struct {
	// ID identifies the key without revealing it: a short digest of it.
	ID     string
	secret []byte
}

// NewKey derives a Key from raw material.
func NewKey(secret []byte) *Key {
	sum := sha256.Sum256(secret)
	return &Key{ID: hex.EncodeToString(sum[:])[:8], secret: append([]byte(nil), secret...)}
}

// EphemeralKey is a random key for one process lifetime. It is the fallback
// when no installation key is configured: hashes are then not comparable
// across restarts, which is a loss of function — but never a loss of secrecy,
// which a public salt would be.
func EphemeralKey() *Key {
	b := make([]byte, keyBytes)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("reading random bytes: %v", err))
	}
	return NewKey(b)
}

// hash returns the short HMAC of a value under this key.
func (k *Key) hash(v interface{}) string {
	mac := hmac.New(sha256.New, k.secret)
	_, _ = fmt.Fprintf(mac, "%v", v)
	return hex.EncodeToString(mac.Sum(nil))[:12]
}

// The key Secret is in the operator's own namespace; the watcher needs to
// create it there once and read it after. (It can already read Secrets
// everywhere, to snapshot them -- but creating one is a narrower grant.)
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create,namespace=system

// LoadOrCreateKey returns the installation's key, creating it on first use.
// A concurrent first start is resolved by whoever creates the Secret first.
func LoadOrCreateKey(ctx context.Context, c client.Client, namespace string) (*Key, error) {
	sec := &corev1.Secret{}
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: KeySecretName}, sec)
	if err == nil {
		return keyFromSecret(sec)
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading the redaction key: %w", err)
	}

	b := make([]byte, keyBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generating a redaction key: %w", err)
	}
	sec = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      KeySecretName,
			Namespace: namespace,
			Labels:    map[string]string{"app.kubernetes.io/name": "chronos"},
			Annotations: map[string]string{
				"chronos.ocp.run/purpose": "HMAC key for redaction hashes. Deleting it rotates the key; " +
					"hashes recorded under the old key stop being comparable with new ones.",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{keyField: b},
	}
	if err := c.Create(ctx, sec); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("creating the redaction key: %w", err)
		}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: KeySecretName}, sec); err != nil {
			return nil, fmt.Errorf("reading the redaction key another replica created: %w", err)
		}
	}
	return keyFromSecret(sec)
}

func keyFromSecret(sec *corev1.Secret) (*Key, error) {
	b := sec.Data[keyField]
	if len(b) < 16 {
		return nil, fmt.Errorf("secret %s/%s has no usable %q (need at least 16 bytes)", sec.Namespace, sec.Name, keyField)
	}
	return NewKey(b), nil
}
