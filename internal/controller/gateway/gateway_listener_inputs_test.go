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

package gateway

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
)

func TestListenerInputsAuthorizeBeforeReadingSecrets(t *testing.T) {
	for _, test := range []struct {
		name       string
		alter      func(*gatewayv1.Gateway, *gatewayv1.GatewayClass, *gatewayv1.ReferenceGrant)
		wantReads  int
		wantChange bool
	}{
		{name: "permitted", wantReads: 1},
		{name: "wrong target name", alter: func(_ *gatewayv1.Gateway, _ *gatewayv1.GatewayClass, grant *gatewayv1.ReferenceGrant) {
			grant.Spec.To[0].Name = ptr.To(gatewayv1.ObjectName("another"))
		}},
		{name: "wrong source kind", alter: func(_ *gatewayv1.Gateway, _ *gatewayv1.GatewayClass, grant *gatewayv1.ReferenceGrant) {
			grant.Spec.From[0].Kind = "HTTPRoute"
		}},
		{name: "wrong source namespace", alter: func(_ *gatewayv1.Gateway, _ *gatewayv1.GatewayClass, grant *gatewayv1.ReferenceGrant) {
			grant.Spec.From[0].Namespace = "another"
		}},
		{name: "foreign class", alter: func(_ *gatewayv1.Gateway, class *gatewayv1.GatewayClass, _ *gatewayv1.ReferenceGrant) {
			class.Spec.ControllerName = "other.example/controller"
		}, wantChange: true},
		{name: "replaced Gateway", alter: func(gateway *gatewayv1.Gateway, _ *gatewayv1.GatewayClass, _ *gatewayv1.ReferenceGrant) {
			gateway.UID = "replacement"
		}, wantChange: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			gateway, class, grant, secret := listenerInputFixtures()
			expected := gateway.DeepCopy()
			if test.alter != nil {
				test.alter(gateway, class, grant)
			}
			kubeClient := rawIndexedFakeClientBuilder(testScheme(t), testConfig()).WithObjects(gateway, class, grant, secret).Build()
			reader := &certificateInputReader{Reader: kubeClient}
			reconciler := &Reconciler{APIReader: reader, Config: testConfig()}
			input, err := reconciler.collectListenerInputs(t.Context(), expected)
			if test.wantChange {
				if !errors.Is(err, controller.ErrGraphInputChanged) {
					t.Fatalf("error = %v, want changed ownership", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(reader.secretReads) != test.wantReads || len(input.Secrets) != test.wantReads {
				t.Fatalf("secret reads=%d observations=%d, want %d", len(reader.secretReads), len(input.Secrets), test.wantReads)
			}
		})
	}
}

func TestListenerInputsFollowRotationRevocationAndDeletion(t *testing.T) {
	gateway, class, grant, secret := listenerInputFixtures()
	second := *gateway.Spec.Listeners[0].DeepCopy()
	second.Name = "second"
	second.Port = 8443
	gateway.Spec.Listeners = append(gateway.Spec.Listeners, second)
	kubeClient := rawIndexedFakeClientBuilder(testScheme(t), testConfig()).WithObjects(gateway, class, grant, secret).Build()
	reader := &certificateInputReader{Reader: kubeClient}
	reconciler := &Reconciler{APIReader: reader, Config: testConfig()}
	input, err := reconciler.collectListenerInputs(t.Context(), gateway)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Secrets) != 1 || len(reader.secretReads) != 1 || reader.grantLists != 1 {
		t.Fatalf("shared certificate was not read once: secrets=%d reads=%d grants=%d", len(input.Secrets), len(reader.secretReads), reader.grantLists)
	}
	before := input.Secrets[0].DeepCopy()
	var current corev1.Secret
	if err := kubeClient.Get(t.Context(), client.ObjectKeyFromObject(secret), &current); err != nil {
		t.Fatal(err)
	}
	current.Data[corev1.TLSCertKey] = []byte("rotated-certificate")
	if err := kubeClient.Update(t.Context(), &current); err != nil {
		t.Fatal(err)
	}
	rotated, err := reconciler.collectListenerInputs(t.Context(), gateway)
	if err != nil || len(rotated.Secrets) != 1 {
		t.Fatalf("rotation observation: %v", err)
	}
	if rotated.Secrets[0].ResourceVersion == before.ResourceVersion || !reflect.DeepEqual(input.Secrets[0], *before) {
		t.Fatal("rotation reused the old revision or mutated an earlier observation")
	}
	if err := kubeClient.Delete(t.Context(), grant); err != nil {
		t.Fatal(err)
	}
	reader.secretReads = nil
	revoked, err := reconciler.collectListenerInputs(t.Context(), gateway)
	if err != nil || len(revoked.Secrets) != 0 || len(reader.secretReads) != 0 {
		t.Fatalf("revoked reference read a payload: err=%v reads=%d", err, len(reader.secretReads))
	}
	grant.ResourceVersion = ""
	if err := kubeClient.Create(t.Context(), grant); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Delete(t.Context(), &current); err != nil {
		t.Fatal(err)
	}
	absent, err := reconciler.collectListenerInputs(t.Context(), gateway)
	if err != nil || len(absent.Secrets) != 0 || len(reader.secretReads) != 1 {
		t.Fatalf("absent Secret observation: err=%v reads=%d", err, len(reader.secretReads))
	}
}

func TestListenerInputsDiscardPartialReadFailures(t *testing.T) {
	gateway, class, grant, secret := listenerInputFixtures()
	second := *gateway.Spec.Listeners[0].DeepCopy()
	second.Name = "second"
	second.TLS.CertificateRefs[0].Name = "unavailable"
	gateway.Spec.Listeners = append(gateway.Spec.Listeners, second)
	grant.Spec.To[0].Name = nil
	kubeClient := rawIndexedFakeClientBuilder(testScheme(t), testConfig()).WithObjects(gateway, class, grant, secret).Build()
	want := errors.New("Secret read unavailable")
	reader := &certificateInputReader{Reader: kubeClient, failSecret: "unavailable", readErr: want}
	reconciler := &Reconciler{APIReader: reader, Config: testConfig()}
	input, err := reconciler.collectListenerInputs(t.Context(), gateway)
	if !errors.Is(err, want) || len(input.Secrets) != 0 || input.Gateway != nil {
		t.Fatalf("partial input returned after failure: err=%v secrets=%d", err, len(input.Secrets))
	}
}

func listenerInputFixtures() (*gatewayv1.Gateway, *gatewayv1.GatewayClass, *gatewayv1.ReferenceGrant, *corev1.Secret) {
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "edge", UID: "gateway-uid", Generation: 1}, Spec: gatewayv1.GatewaySpec{
		GatewayClassName: "openstack",
		Listeners: []gatewayv1.Listener{{Name: "https", Protocol: gatewayv1.HTTPSProtocolType, Port: 443, TLS: &gatewayv1.ListenerTLSConfig{
			CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "certificate", Namespace: ptr.To(gatewayv1.Namespace("certificates"))}},
		}}},
	}}
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "openstack"}, Spec: gatewayv1.GatewayClassSpec{ControllerName: testConfig().ControllerName}}
	grant := &gatewayv1.ReferenceGrant{ObjectMeta: metav1.ObjectMeta{Namespace: "certificates", Name: "certificate"}, Spec: gatewayv1.ReferenceGrantSpec{
		From: []gatewayv1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "Gateway", Namespace: "apps"}},
		To:   []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Secret", Name: ptr.To(gatewayv1.ObjectName("certificate"))}},
	}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "certificates", Name: "certificate", UID: "secret-uid"}, Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{corev1.TLSCertKey: []byte("synthetic-certificate"), corev1.TLSPrivateKeyKey: []byte("synthetic-key")}}
	return gateway, class, grant, secret
}

type certificateInputReader struct {
	client.Reader
	secretReads []types.NamespacedName
	grantLists  int
	failSecret  string
	readErr     error
}

func (r *certificateInputReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, secret := object.(*corev1.Secret); secret {
		r.secretReads = append(r.secretReads, key)
		if key.Name == r.failSecret {
			return r.readErr
		}
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func (r *certificateInputReader) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if _, secret := list.(*corev1.SecretList); secret {
		return errors.New("certificate collection must not list Secret payloads")
	}
	if _, grant := list.(*gatewayv1.ReferenceGrantList); grant {
		r.grantLists++
	}
	return r.Reader.List(ctx, list, options...)
}
