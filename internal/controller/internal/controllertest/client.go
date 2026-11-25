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

// Package controllertest supplies independent fixtures for controller unit tests.
package controllertest

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayconsts "sigs.k8s.io/gateway-api/pkg/consts"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
)

// Scheme returns a new scheme for Gateway and HTTPRoute controller tests.
func Scheme(tb testing.TB) *runtime.Scheme {
	tb.Helper()
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{
		configv1alpha1.AddToScheme, corev1.AddToScheme, discoveryv1.AddToScheme,
		apiextensionsv1.AddToScheme, gatewayv1.Install,
	} {
		if err := install(scheme); err != nil {
			tb.Fatal(err)
		}
	}
	return scheme
}

// ClientBuilder supplies the class parameters and API bundle for ordinary tests.
// Use RawClientBuilder for intentionally incomplete configurations.
type ClientBuilder struct {
	builder              *fake.ClientBuilder
	config               controller.Config
	hasGatewayAPICRDs    bool
	hasDefaultParameters bool
}

// NewClientBuilder creates an independent builder with the controller's indexes.
func NewClientBuilder(scheme *runtime.Scheme, config controller.Config) *ClientBuilder {
	return &ClientBuilder{builder: RawClientBuilder(scheme, config), config: config}
}

// WithObjects completes the supplied fixtures in place before storing them.
// Callers must create their own objects rather than share them between tests.
// Explicit version conditions, CRDs, parameters, and topology bindings are kept.
func (b *ClientBuilder) WithObjects(objects ...client.Object) *ClientBuilder {
	for _, object := range objects {
		switch value := object.(type) {
		case *configv1alpha1.OpenStackGatewayClassConfig:
			if value.Name == "test-parameters" {
				b.hasDefaultParameters = true
			}
		case *gatewayv1.GatewayClass:
			if value.Spec.ParametersRef == nil {
				value.Spec.ParametersRef = &gatewayv1.ParametersReference{Group: configv1alpha1.GroupName, Kind: configv1alpha1.GatewayClassConfigKind, Name: "test-parameters"}
			}
		case *gatewayv1.Gateway:
			CompleteGatewayTopology(b.config, value)
		}
		if definition, ok := object.(*apiextensionsv1.CustomResourceDefinition); ok &&
			(definition.Spec.Group == gatewayv1.GroupName || definition.Spec.Group == "gateway.networking.x-k8s.io") {
			b.hasGatewayAPICRDs = true
		}
		gatewayClass, ok := object.(*gatewayv1.GatewayClass)
		if !ok || meta.FindStatusCondition(gatewayClass.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion)) != nil {
			continue
		}
		controller.SetCondition(&gatewayClass.Status.Conditions, controller.Condition(
			string(gatewayv1.GatewayClassConditionStatusSupportedVersion), metav1.ConditionTrue,
			string(gatewayv1.GatewayClassReasonSupportedVersion),
			"Installed Gateway API CRDs use supported bundle version "+gatewayconsts.BundleVersion,
			gatewayClass.Generation,
		))
	}
	b.builder = b.builder.WithObjects(objects...)
	return b
}

// WithStatusSubresource enables fake status handling for the supplied kinds.
func (b *ClientBuilder) WithStatusSubresource(objects ...client.Object) *ClientBuilder {
	b.builder = b.builder.WithStatusSubresource(objects...)
	return b
}

// Build adds only default fixtures that the test has not supplied explicitly.
func (b *ClientBuilder) Build() client.WithWatch {
	if !b.hasDefaultParameters {
		b.builder.WithObjects(ClassParameters())
	}
	if !b.hasGatewayAPICRDs {
		b.builder.WithObjects(SupportedGatewayAPICRDs()...)
	}
	return b.builder.Build()
}

// RawClientBuilder registers indexes without completing or adding fixtures.
func RawClientBuilder(scheme *runtime.Scheme, config controller.Config) *fake.ClientBuilder {
	builder := fake.NewClientBuilder().WithScheme(scheme)
	if err := controller.SetupIndexes(context.Background(), &fieldIndexer{builder: builder}, config); err != nil {
		panic(err)
	}
	return builder
}

// WithStatusPatchError injects the caller's current error into status patches.
// Each synchronous test owns its error value and can clear it to resume writes.
func WithStatusPatchError(base client.WithWatch, injectedErr *error) client.WithWatch {
	return interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
			if subResource == "status" && *injectedErr != nil {
				return *injectedErr
			}
			return c.SubResource(subResource).Patch(ctx, object, patch, options...)
		},
	})
}

type fieldIndexer struct {
	builder *fake.ClientBuilder
}

func (i *fieldIndexer) IndexField(_ context.Context, object client.Object, field string, extract client.IndexerFunc) error {
	i.builder.WithIndex(object, field, extract)
	return nil
}

// SupportedGatewayAPICRDs returns a fresh copy of the required bundle fixtures.
func SupportedGatewayAPICRDs() []client.Object {
	names := []string{
		"gatewayclasses.gateway.networking.k8s.io",
		"gateways.gateway.networking.k8s.io",
		"httproutes.gateway.networking.k8s.io",
		"referencegrants.gateway.networking.k8s.io",
	}
	definitions := make([]client.Object, 0, len(names))
	for _, name := range names {
		definitions = append(definitions, gatewayAPICRD(name, gatewayconsts.BundleVersion))
	}
	return definitions
}

// gatewayAPICRD creates a bundle fixture, including an absent version if requested.
func gatewayAPICRD(name, bundleVersion string) *apiextensionsv1.CustomResourceDefinition {
	annotations := map[string]string{}
	if bundleVersion != "" {
		annotations[gatewayconsts.BundleVersionAnnotation] = bundleVersion
	}
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations},
		Spec:       apiextensionsv1.CustomResourceDefinitionSpec{Group: gatewayv1.GroupName},
	}
}
