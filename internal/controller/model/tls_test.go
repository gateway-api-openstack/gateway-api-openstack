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

package model

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestResolveListenerTLSRetainsOnlyCertificateRevision(t *testing.T) {
	secret := tlsTestSecret(t)
	before := secret.DeepCopy()
	listener := tlsTestListener()
	resolved, diagnostic := resolveListenerTLS("apps", listener, []corev1.Secret{secret}, nil)
	if diagnostic != nil || resolved == nil || resolved.Mode != gatewayv1.TLSModeTerminate || resolved.Certificate != source(&secret) {
		t.Fatalf("TLS = %#v, diagnostic = %#v", resolved, diagnostic)
	}
	if !reflect.DeepEqual(secret, *before) {
		t.Fatal("TLS validation changed the observed Secret")
	}
	gateway := logicalGateway()
	gateway.Spec.Listeners = []gatewayv1.Listener{listener}
	compiled := compileLogical(t, Input{Gateway: gateway, Secrets: []corev1.Secret{secret}})
	encoded, err := json.Marshal(compiled)
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range secret.Data {
		if strings.Contains(string(encoded), string(payload)) || strings.Contains(string(encoded), base64.StdEncoding.EncodeToString(payload)) {
			t.Fatal("compiled Gateway retained a certificate payload")
		}
	}
	secret.ResourceVersion = "replacement"
	rotated, diagnostic := resolveListenerTLS("apps", listener, []corev1.Secret{secret}, nil)
	if diagnostic != nil || rotated.Certificate.ResourceVersion != "replacement" || resolved.Certificate == rotated.Certificate {
		t.Fatalf("replacement revision = %#v, diagnostic = %#v", rotated, diagnostic)
	}
}

func TestCompileTLSKeepsLogicalListenersAndIndependentErrors(t *testing.T) {
	secret := tlsTestSecret(t)
	gateway := logicalGateway()
	valid := tlsTestListener()
	invalid := *valid.DeepCopy()
	invalid.Name = "unauthorized"
	invalid.Hostname = ptr.To(gatewayv1.Hostname("another.example.com"))
	invalid.TLS.CertificateRefs[0].Namespace = ptr.To(gatewayv1.Namespace("certificates"))
	gateway.Spec.Listeners = append(gateway.Spec.Listeners, valid, invalid)
	input := Input{Gateway: gateway, Secrets: []corev1.Secret{secret}}
	before := gateway.DeepCopy()
	compiled := compileLogical(t, input)
	if !reflect.DeepEqual(gateway, before) {
		t.Fatal("compiler changed Gateway TLS inputs")
	}
	for _, listener := range compiled.Listeners {
		switch listener.Name {
		case "http":
			if listener.TLS != nil || len(listener.Diagnostics) != 0 {
				t.Fatalf("HTTP listener inherited TLS errors: %#v", listener)
			}
		case "https":
			if listener.TLS == nil || listener.TLS.Certificate != source(&secret) || listener.Hostname != "shop.example.com" || len(listener.Diagnostics) != 0 {
				t.Fatalf("valid HTTPS listener = %#v", listener)
			}
		case "unauthorized":
			if listener.TLS != nil || len(listener.Diagnostics) != 1 || listener.Diagnostics[0].Reason != string(gatewayv1.ListenerReasonRefNotPermitted) {
				t.Fatalf("unauthorized HTTPS listener = %#v", listener)
			}
		}
	}
	// Changing the observation cannot change an already compiled certificate.
	input.Secrets[0].UID = "changed"
	input.Gateway.Spec.Listeners[1].TLS.CertificateRefs[0].Name = "changed"
	if compiled.Listeners[1].TLS.Certificate.UID != "certificate-uid" || compiled.Listeners[1].TLS.Certificate.Name != "certificate" {
		t.Fatal("compiled TLS aliases its Kubernetes inputs")
	}
}

func TestCompileHTTPSListenerConflicts(t *testing.T) {
	for _, protocol := range []gatewayv1.ProtocolType{gatewayv1.HTTPProtocolType, gatewayv1.HTTPSProtocolType} {
		t.Run(string(protocol), func(t *testing.T) {
			gateway := logicalGateway()
			listener := tlsTestListener()
			other := *listener.DeepCopy()
			other.Name = "other"
			other.Protocol = protocol
			if protocol == gatewayv1.HTTPProtocolType {
				other.TLS = nil
			}
			gateway.Spec.Listeners = []gatewayv1.Listener{listener, other}
			compiled := compileLogical(t, Input{Gateway: gateway, Secrets: []corev1.Secret{tlsTestSecret(t)}})
			reason := gatewayv1.ListenerReasonHostnameConflict
			if protocol == gatewayv1.HTTPProtocolType {
				reason = gatewayv1.ListenerReasonProtocolConflict
			}
			for _, listener := range compiled.Listeners {
				if len(listener.Diagnostics) != 1 || listener.Diagnostics[0].Condition != string(gatewayv1.ListenerConditionConflicted) || listener.Diagnostics[0].Reason != string(reason) {
					t.Fatalf("listener conflict = %#v", listener)
				}
			}
		})
	}
}

func TestResolveListenerTLSRejectsInvalidSecrets(t *testing.T) {
	valid := tlsTestSecret(t)
	other := tlsTestSecret(t)
	tests := []struct {
		name   string
		change func(*corev1.Secret)
	}{
		{name: "wrong type", change: func(secret *corev1.Secret) { secret.Type = corev1.SecretTypeOpaque }},
		{name: "missing certificate", change: func(secret *corev1.Secret) { delete(secret.Data, corev1.TLSCertKey) }},
		{name: "missing key", change: func(secret *corev1.Secret) { delete(secret.Data, corev1.TLSPrivateKeyKey) }},
		{name: "invalid certificate", change: func(secret *corev1.Secret) { secret.Data[corev1.TLSCertKey] = []byte("private-certificate-input") }},
		{name: "invalid key", change: func(secret *corev1.Secret) { secret.Data[corev1.TLSPrivateKeyKey] = []byte("private-key-input") }},
		{name: "mismatched key", change: func(secret *corev1.Secret) {
			secret.Data[corev1.TLSPrivateKeyKey] = other.Data[corev1.TLSPrivateKeyKey]
		}},
		{name: "invalid chain", change: func(secret *corev1.Secret) {
			secret.Data[corev1.TLSCertKey] = append(secret.Data[corev1.TLSCertKey], pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("invalid chain")})...)
		}},
		{name: "deleting", change: func(secret *corev1.Secret) { secret.DeletionTimestamp = ptr.To(metav1.NewTime(time.Unix(2, 0))) }},
		{name: "absent", change: func(secret *corev1.Secret) { secret.Name = "another" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := valid.DeepCopy()
			tt.change(secret)
			resolved, diagnostic := resolveListenerTLS("apps", tlsTestListener(), []corev1.Secret{*secret}, nil)
			if resolved != nil || diagnostic == nil || diagnostic.Condition != string(gatewayv1.ListenerConditionResolvedRefs) || diagnostic.Reason != string(gatewayv1.ListenerReasonInvalidCertificateRef) {
				t.Fatalf("TLS = %#v, diagnostic = %#v", resolved, diagnostic)
			}
			if strings.Contains(diagnostic.Message, "private-") || strings.Contains(diagnostic.Message, "BEGIN ") {
				t.Fatalf("diagnostic exposed Secret content: %s", diagnostic.Message)
			}
		})
	}
}

func TestResolveListenerTLSRejectsUnsupportedConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		change func(*gatewayv1.Listener)
	}{
		{name: "missing TLS", change: func(listener *gatewayv1.Listener) { listener.TLS = nil }},
		{name: "passthrough", change: func(listener *gatewayv1.Listener) { listener.TLS.Mode = ptr.To(gatewayv1.TLSModePassthrough) }},
		{name: "missing reference", change: func(listener *gatewayv1.Listener) { listener.TLS.CertificateRefs = nil }},
		{name: "multiple references", change: func(listener *gatewayv1.Listener) {
			listener.TLS.CertificateRefs = append(listener.TLS.CertificateRefs, gatewayv1.SecretObjectReference{Name: "second"})
		}},
		{name: "options", change: func(listener *gatewayv1.Listener) {
			listener.TLS.Options = map[gatewayv1.AnnotationKey]gatewayv1.AnnotationValue{"example.com/tls": "value"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listener := tlsTestListener()
			tt.change(&listener)
			resolved, diagnostic := resolveListenerTLS("apps", listener, nil, nil)
			if resolved != nil || diagnostic == nil || diagnostic.Condition != string(gatewayv1.ListenerConditionAccepted) || diagnostic.Reason != string(gatewayv1.ListenerReasonUnsupportedValue) {
				t.Fatalf("TLS = %#v, diagnostic = %#v", resolved, diagnostic)
			}
		})
	}
}

func TestCertificateReferencesRequireGatewayToSecretGrant(t *testing.T) {
	reference := gatewayv1.SecretObjectReference{Name: "certificate", Namespace: ptr.To(gatewayv1.Namespace("certificates"))}
	grant := gatewayv1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Namespace: "certificates", Name: "allow"},
		Spec: gatewayv1.ReferenceGrantSpec{
			From: []gatewayv1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "Gateway", Namespace: "apps"}},
			To:   []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Secret", Name: ptr.To(gatewayv1.ObjectName("certificate"))}},
		},
	}
	if key, supported := CertificateSecretReference("apps", reference); !supported || key != (types.NamespacedName{Namespace: "certificates", Name: "certificate"}) {
		t.Fatalf("normalized reference = %v, supported = %v", key, supported)
	}
	if !CertificateReferenceAllowed("apps", reference, []gatewayv1.ReferenceGrant{grant}) || CertificateReferenceAllowed("apps", reference, nil) {
		t.Fatal("cross-namespace reference did not follow its grant")
	}
	tests := []struct {
		name   string
		change func(*gatewayv1.ReferenceGrant)
	}{
		{name: "source kind", change: func(grant *gatewayv1.ReferenceGrant) { grant.Spec.From[0].Kind = "HTTPRoute" }},
		{name: "source namespace", change: func(grant *gatewayv1.ReferenceGrant) { grant.Spec.From[0].Namespace = "another" }},
		{name: "source group", change: func(grant *gatewayv1.ReferenceGrant) { grant.Spec.From[0].Group = "" }},
		{name: "target kind", change: func(grant *gatewayv1.ReferenceGrant) { grant.Spec.To[0].Kind = "Service" }},
		{name: "target group", change: func(grant *gatewayv1.ReferenceGrant) { grant.Spec.To[0].Group = "example.com" }},
		{name: "target name", change: func(grant *gatewayv1.ReferenceGrant) { grant.Spec.To[0].Name = ptr.To(gatewayv1.ObjectName("another")) }},
		{name: "target namespace", change: func(grant *gatewayv1.ReferenceGrant) { grant.Namespace = "apps" }},
		{name: "deleted grant", change: func(grant *gatewayv1.ReferenceGrant) {
			grant.DeletionTimestamp = ptr.To(metav1.NewTime(time.Unix(2, 0)))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := grant.DeepCopy()
			tt.change(changed)
			if CertificateReferenceAllowed("apps", reference, []gatewayv1.ReferenceGrant{*changed}) {
				t.Fatal("unrelated or deleted grant authorized the certificate")
			}
		})
	}
	grant.Spec.To[0].Name = nil
	if !CertificateReferenceAllowed("apps", reference, []gatewayv1.ReferenceGrant{grant}) {
		t.Fatal("grant for all Secrets in the namespace did not authorize the reference")
	}
}

func TestResolveListenerTLSChecksPermissionBeforeSecretValidation(t *testing.T) {
	listener := tlsTestListener()
	listener.TLS.CertificateRefs[0].Namespace = ptr.To(gatewayv1.Namespace("certificates"))
	secret := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "certificates", Name: "certificate"}, Type: corev1.SecretTypeOpaque}
	resolved, diagnostic := resolveListenerTLS("apps", listener, []corev1.Secret{secret}, nil)
	if resolved != nil || diagnostic == nil || diagnostic.Reason != string(gatewayv1.ListenerReasonRefNotPermitted) {
		t.Fatalf("unauthorized Secret was evaluated: TLS = %#v, diagnostic = %#v", resolved, diagnostic)
	}
	listener.TLS.CertificateRefs[0].Kind = ptr.To(gatewayv1.Kind("ConfigMap"))
	if _, supported := CertificateSecretReference("apps", listener.TLS.CertificateRefs[0]); supported {
		t.Fatal("unsupported kind normalized as a Secret")
	}
	_, diagnostic = resolveListenerTLS("apps", listener, nil, nil)
	if diagnostic == nil || diagnostic.Reason != string(gatewayv1.ListenerReasonRefNotPermitted) {
		t.Fatalf("unauthorized unsupported kind = %#v", diagnostic)
	}
	listener.TLS.CertificateRefs[0].Namespace = nil
	_, diagnostic = resolveListenerTLS("apps", listener, nil, nil)
	if diagnostic == nil || diagnostic.Reason != string(gatewayv1.ListenerReasonInvalidCertificateRef) {
		t.Fatalf("authorized unsupported kind = %#v", diagnostic)
	}
}

func tlsTestListener() gatewayv1.Listener {
	return gatewayv1.Listener{
		Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType, Hostname: ptr.To(gatewayv1.Hostname("shop.example.com")),
		TLS: &gatewayv1.ListenerTLSConfig{CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "certificate"}}},
	}
}

func tlsTestSecret(t *testing.T) corev1.Secret {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// Expiration and hostname matching are not additional admission rules here.
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1), DNSNames: []string{"another.example.com"},
		NotBefore: time.Unix(1, 0), NotAfter: time.Unix(2, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "certificate", UID: "certificate-uid", ResourceVersion: "original"},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKey}),
		},
	}
}
