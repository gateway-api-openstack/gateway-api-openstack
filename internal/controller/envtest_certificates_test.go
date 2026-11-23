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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
)

func testGatewayCertificateInputs(t *testing.T, apiClient, cacheClient client.Client, config controller.Config, className string) {
	t.Helper()
	const gatewayNamespace = "certificate-apps-envtest"
	const certificateNamespace = "certificate-store-envtest"
	for _, name := range []string{gatewayNamespace, certificateNamespace} {
		if err := apiClient.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: certificateNamespace, Name: "certificate"}, Type: corev1.SecretTypeTLS, Data: envtestCertificateData(t)}
	if err := apiClient.Create(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: gatewayNamespace, Name: "certificates"}, Spec: gatewayv1.GatewaySpec{
		GatewayClassName: gatewayv1.ObjectName(className),
		Listeners: []gatewayv1.Listener{{Name: "https", Protocol: gatewayv1.HTTPSProtocolType, Port: 443, TLS: &gatewayv1.ListenerTLSConfig{
			CertificateRefs: []gatewayv1.SecretObjectReference{{Name: gatewayv1.ObjectName(secret.Name), Namespace: ptr.To(gatewayv1.Namespace(secret.Namespace))}},
		}}},
	}}
	if err := apiClient.Create(t.Context(), gateway); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(gateway)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), envtestWaitTimeout)
		defer cancel()
		if err := apiClient.Delete(ctx, gateway); client.IgnoreNotFound(err) != nil {
			t.Errorf("delete certificate test Gateway: %v", err)
		}
		if err := wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(ctx context.Context) (bool, error) {
			var current gatewayv1.Gateway
			err := cacheClient.Get(ctx, key, &current)
			return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
		}); err != nil {
			t.Errorf("wait for certificate test Gateway deletion: %v", err)
		}
	})
	waitForGatewayResourceVersion(t, cacheClient, key, gateway.ResourceVersion, false)
	provider := &recordingProvider{}
	reader := &envtestCertificateReader{Reader: apiClient}
	reconciler := newEnvtestGatewayReconciler(cacheClient, reader, provider, config)
	check := func(status metav1.ConditionStatus, reason gatewayv1.ListenerConditionReason) {
		t.Helper()
		if _, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
		if err := apiClient.Get(t.Context(), key, gateway); err != nil {
			t.Fatal(err)
		}
		if len(gateway.Status.Listeners) != 1 {
			t.Fatalf("listener status count=%d", len(gateway.Status.Listeners))
		}
		conditions := gateway.Status.Listeners[0].Conditions
		resolved := meta.FindStatusCondition(conditions, string(gatewayv1.ListenerConditionResolvedRefs))
		if resolved == nil || resolved.Status != status || resolved.Reason != string(reason) || resolved.ObservedGeneration != gateway.Generation {
			t.Fatalf("ResolvedRefs=%#v, want %s/%s", resolved, status, reason)
		}
		programmed := meta.FindStatusCondition(conditions, string(gatewayv1.ListenerConditionProgrammed))
		if programmed == nil || programmed.Status != metav1.ConditionFalse || len(provider.Calls) != 0 {
			t.Fatal("certificate input collection enabled HTTPS programming")
		}
		waitForGatewayResourceVersion(t, cacheClient, key, gateway.ResourceVersion, false)
	}
	check(metav1.ConditionFalse, gatewayv1.ListenerReasonRefNotPermitted)
	if reader.secretReads != 0 {
		t.Fatal("denied certificate reference read the Secret")
	}
	grant := &gatewayv1.ReferenceGrant{ObjectMeta: metav1.ObjectMeta{Namespace: certificateNamespace, Name: "certificate"}, Spec: gatewayv1.ReferenceGrantSpec{
		From: []gatewayv1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "Gateway", Namespace: gatewayNamespace}},
		To:   []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Secret", Name: ptr.To(gatewayv1.ObjectName(secret.Name))}},
	}}
	if err := apiClient.Create(t.Context(), grant); err != nil {
		t.Fatal(err)
	}
	check(metav1.ConditionTrue, gatewayv1.ListenerReasonResolvedRefs)
	if reader.secretReads != 1 {
		t.Fatalf("authorized certificate reads=%d, want 1", reader.secretReads)
	}

	// Metadata watches must see data-only rotations without caching payloads.
	metadata := &metav1.PartialObjectMetadata{}
	metadata.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Secret"})
	waitForEnvtest(t, func(ctx context.Context) (bool, error) {
		if err := cacheClient.Get(ctx, client.ObjectKeyFromObject(secret), metadata); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return metadata.ResourceVersion == secret.ResourceVersion, nil
	})
	secret.Data[corev1.TLSPrivateKeyKey] = []byte("invalid-rotated-key")
	if err := apiClient.Update(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	waitForEnvtest(t, func(ctx context.Context) (bool, error) {
		if err := cacheClient.Get(ctx, client.ObjectKeyFromObject(secret), metadata); err != nil {
			return false, err
		}
		return metadata.ResourceVersion == secret.ResourceVersion, nil
	})
	check(metav1.ConditionFalse, gatewayv1.ListenerReasonInvalidCertificateRef)
	secret.Data = envtestCertificateData(t)
	if err := apiClient.Update(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	check(metav1.ConditionTrue, gatewayv1.ListenerReasonResolvedRefs)
	if err := apiClient.Delete(t.Context(), grant); err != nil {
		t.Fatal(err)
	}
	before := reader.secretReads
	check(metav1.ConditionFalse, gatewayv1.ListenerReasonRefNotPermitted)
	if reader.secretReads != before {
		t.Fatal("revoked grant still allowed a Secret read")
	}
	grant.ResourceVersion = ""
	if err := apiClient.Create(t.Context(), grant); err != nil {
		t.Fatal(err)
	}
	if err := apiClient.Delete(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	check(metav1.ConditionFalse, gatewayv1.ListenerReasonInvalidCertificateRef)

	assertIndex := func(field, value string, present bool) {
		t.Helper()
		waitForEnvtest(t, func(ctx context.Context) (bool, error) {
			var gateways gatewayv1.GatewayList
			if err := cacheClient.List(ctx, &gateways, client.MatchingFields{field: value}); err != nil {
				return false, err
			}
			if !present {
				return len(gateways.Items) == 0, nil
			}
			return len(gateways.Items) == 1 && gateways.Items[0].UID == gateway.UID, nil
		})
	}
	oldKey := controller.ObjectKeyString(client.ObjectKeyFromObject(secret))
	assertIndex(controller.IndexGatewayByCertificateSecret, oldKey, true)
	assertIndex(controller.IndexGatewayByCertificateNamespace, certificateNamespace, true)
	gateway.Spec.Listeners[0].TLS.CertificateRefs[0] = gatewayv1.SecretObjectReference{Name: "local"}
	if err := apiClient.Update(t.Context(), gateway); err != nil {
		t.Fatal(err)
	}
	assertIndex(controller.IndexGatewayByCertificateSecret, oldKey, false)
	assertIndex(controller.IndexGatewayByCertificateNamespace, certificateNamespace, false)
	assertIndex(controller.IndexGatewayByCertificateSecret, gatewayNamespace+"/local", true)
}

type envtestCertificateReader struct {
	client.Reader
	secretReads int
}

func (r *envtestCertificateReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, secret := object.(*corev1.Secret); secret {
		r.secretReads++
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func envtestCertificateData(t *testing.T) map[string][]byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"example.test"}, NotBefore: time.Unix(0, 0), NotAfter: time.Unix(4102444800, 0), KeyUsage: x509.KeyUsageDigitalSignature}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{
		corev1.TLSCertKey:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}),
		corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privateKey}),
	}
}
