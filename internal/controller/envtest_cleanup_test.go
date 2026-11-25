//go:build envtest

/*
Copyright 2026 The gateway-api-openstack Authors.

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

package controller_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func testCleanupUIDPrecondition(t *testing.T, apiClient client.Client, namespace string) {
	t.Helper()
	original := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "cleanup-uid-precondition"}}
	if err := apiClient.Create(t.Context(), original); err != nil {
		t.Fatalf("create original ConfigMap: %v", err)
	}
	stale := original.DeepCopy()
	if err := apiClient.Delete(t.Context(), original, client.Preconditions{UID: &original.UID}); err != nil {
		t.Fatalf("delete original ConfigMap: %v", err)
	}
	key := client.ObjectKeyFromObject(original)
	if err := apiClient.Get(t.Context(), key, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("original ConfigMap Get() error = %v, want NotFound", err)
	}

	// Recreate after cleanup observes the original UID but before its DELETE.
	// Unlike the fake client, the API server enforces the UID precondition.
	replacement := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: original.Name}}
	if err := apiClient.Create(t.Context(), replacement); err != nil {
		t.Fatalf("create replacement ConfigMap: %v", err)
	}
	if replacement.UID == "" || replacement.UID == stale.UID {
		t.Fatalf("replacement UID = %q, want a new UID distinct from %q", replacement.UID, stale.UID)
	}
	if err := apiClient.Delete(t.Context(), stale, client.Preconditions{UID: &stale.UID}); !apierrors.IsConflict(err) {
		t.Fatalf("stale ConfigMap Delete() error = %v, want Conflict", err)
	}
	var observed corev1.ConfigMap
	if err := apiClient.Get(t.Context(), key, &observed); err != nil {
		t.Fatalf("read preserved replacement ConfigMap: %v", err)
	}
	if observed.UID != replacement.UID || !observed.DeletionTimestamp.IsZero() {
		t.Fatalf("stale cleanup changed replacement: UID = %q, deletionTimestamp = %v", observed.UID, observed.DeletionTimestamp)
	}
	if err := apiClient.Delete(t.Context(), &observed, client.Preconditions{UID: &replacement.UID}); err != nil {
		t.Fatalf("delete replacement with its own UID: %v", err)
	}
	if err := apiClient.Get(t.Context(), key, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("replacement ConfigMap Get() error = %v, want NotFound", err)
	}
}
