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
	"crypto/tls"
	"crypto/x509"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// ListenerTLS records an authorized certificate revision for HTTPS termination.
// It contains no certificate payload and does not authorize cloud publication.
// The containing Listener's hostname determines its logical SNI selection.
type ListenerTLS struct {
	Mode        gatewayv1.TLSModeType
	Certificate Source
}

// resolveListenerTLS validates an HTTPS listener against observed Secrets and
// grants. Collectors must also check CertificateReferenceAllowed before reading
// a Secret, and recheck authorization before publishing a graph.
func resolveListenerTLS(namespace string, listener gatewayv1.Listener, secrets []corev1.Secret, grants []gatewayv1.ReferenceGrant) (*ListenerTLS, *Diagnostic) {
	if listener.Protocol != gatewayv1.HTTPSProtocolType {
		return nil, &Diagnostic{
			Condition: string(gatewayv1.ListenerConditionAccepted), Reason: string(gatewayv1.ListenerReasonUnsupportedProtocol),
			Message: "certificate inputs require an HTTPS listener",
		}
	}
	configuration := listener.TLS
	if configuration == nil || ptr.Deref(configuration.Mode, gatewayv1.TLSModeTerminate) != gatewayv1.TLSModeTerminate {
		return nil, unsupportedTLS("HTTPS listeners require TLS termination")
	}
	if len(configuration.Options) != 0 {
		return nil, unsupportedTLS("TLS options are not implemented")
	}
	if len(configuration.CertificateRefs) != 1 {
		return nil, unsupportedTLS("HTTPS listeners require exactly one certificate reference")
	}
	reference := configuration.CertificateRefs[0]
	if !CertificateReferenceAllowed(namespace, reference, grants) {
		return nil, &Diagnostic{
			Condition: string(gatewayv1.ListenerConditionResolvedRefs), Reason: string(gatewayv1.ListenerReasonRefNotPermitted),
			Message: "certificate reference is not permitted by a ReferenceGrant",
		}
	}
	key, supported := CertificateSecretReference(namespace, reference)
	if !supported {
		return nil, invalidCertificate("certificate reference must select a core Secret")
	}
	for i := range secrets {
		secret := &secrets[i]
		if secret.Namespace != key.Namespace || secret.Name != key.Name || !secret.DeletionTimestamp.IsZero() {
			continue
		}
		if diagnostic := validateTLSSecret(secret); diagnostic != nil {
			return nil, diagnostic
		}
		return &ListenerTLS{Mode: gatewayv1.TLSModeTerminate, Certificate: source(secret)}, nil
	}
	return nil, invalidCertificate("certificate Secret was not found")
}

// CertificateSecretReference normalizes a core Secret reference for indexed
// reads. Its result establishes neither access permission nor cloud ownership.
func CertificateSecretReference(namespace string, reference gatewayv1.SecretObjectReference) (types.NamespacedName, bool) {
	if ptr.Deref(reference.Group, "") != "" || ptr.Deref(reference.Kind, "Secret") != "Secret" {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{
		Namespace: string(ptr.Deref(reference.Namespace, gatewayv1.Namespace(namespace))),
		Name:      string(reference.Name),
	}, true
}

// CertificateReferenceAllowed authorizes a Gateway's reference without reading
// the target. A grant for an HTTPRoute or Service cannot authorize a certificate.
func CertificateReferenceAllowed(namespace string, reference gatewayv1.SecretObjectReference, grants []gatewayv1.ReferenceGrant) bool {
	return referenceAllowed(namespace, "Gateway", gatewayv1.ObjectReference{
		Group: ptr.Deref(reference.Group, ""), Kind: ptr.Deref(reference.Kind, "Secret"),
		Namespace: reference.Namespace, Name: reference.Name,
	}, grants)
}

func validateTLSSecret(secret *corev1.Secret) *Diagnostic {
	if secret.Type != corev1.SecretTypeTLS {
		return invalidCertificate("certificate Secret must have type kubernetes.io/tls")
	}
	certificate, key := secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey]
	if len(certificate) == 0 || len(key) == 0 {
		return invalidCertificate("certificate Secret must contain tls.crt and tls.key")
	}
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil {
		return invalidCertificate("certificate Secret contains an invalid certificate or private key, or they do not match")
	}
	for _, der := range pair.Certificate {
		if _, err := x509.ParseCertificate(der); err != nil {
			return invalidCertificate("certificate Secret contains an invalid certificate chain")
		}
	}
	return nil
}

func invalidCertificate(message string) *Diagnostic {
	return &Diagnostic{
		Condition: string(gatewayv1.ListenerConditionResolvedRefs), Reason: string(gatewayv1.ListenerReasonInvalidCertificateRef),
		Message: message,
	}
}

func unsupportedTLS(message string) *Diagnostic {
	return &Diagnostic{
		Condition: string(gatewayv1.ListenerConditionAccepted), Reason: string(gatewayv1.ListenerReasonUnsupportedValue),
		Message: message,
	}
}
