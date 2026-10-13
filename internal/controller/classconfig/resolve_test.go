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

package classconfig

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

func validParameters() *configv1alpha1.OpenStackGatewayClassConfig {
	return &configv1alpha1.OpenStackGatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "parameters", UID: "parameters-uid", Generation: 2},
		Spec: configv1alpha1.OpenStackGatewayClassConfigSpec{
			VIPSubnet:    configv1alpha1.ResourceReference{ID: "11111111-1111-1111-1111-111111111111"},
			MemberSubnet: configv1alpha1.ResourceReference{Name: "members"},
		},
	}
}

func TestValidateDefaultsAndExplicitTopology(t *testing.T) {
	params := validParameters()
	before := params.DeepCopy()
	req, address, monitor, err := Validate(params.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if req.VIPSubnet.ID != params.Spec.VIPSubnet.ID || req.MemberSubnet.Name != "members" || req.ExternalNetwork != nil {
		t.Fatalf("request = %#v", req)
	}
	if address != corev1.NodeInternalIP || monitor != (cloud.HealthMonitorConfig{Path: "/", Delay: 10, Timeout: 5, MaxRetries: 3, MaxRetriesDown: 3}) {
		t.Fatalf("defaults = %s %#v", address, monitor)
	}
	if !reflect.DeepEqual(params, before) {
		t.Fatal("validation changed the API object")
	}
	params.Spec.FloatingIP = configv1alpha1.FloatingIPConfig{Mode: configv1alpha1.FloatingIPAllocate, ExternalNetwork: &configv1alpha1.ResourceReference{Tags: []string{"external", "test"}}}
	req, _, _, err = Validate(params.Spec)
	if err != nil {
		t.Fatal(err)
	}
	req.ExternalNetwork.Tags[0] = "changed"
	if params.Spec.FloatingIP.ExternalNetwork.Tags[0] != "external" {
		t.Fatal("request aliases API selector")
	}
}

func TestValidateRejectsInvalidConfiguration(t *testing.T) {
	ptr := func(v int32) *int32 { return &v }
	tests := []struct {
		name   string
		mutate func(*configv1alpha1.OpenStackGatewayClassConfigSpec)
	}{
		{"missing reference", func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) {
			s.VIPSubnet = configv1alpha1.ResourceReference{}
		}},
		{"two selectors", func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) { s.VIPSubnet.Name = "vip" }},
		{"invalid UUID", func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) { s.VIPSubnet.ID = "not-an-id" }},
		{"duplicate tag", func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) {
			s.MemberSubnet = configv1alpha1.ResourceReference{Tags: []string{"same", "same"}}
		}},
		{"comma tag", func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) {
			s.MemberSubnet = configv1alpha1.ResourceReference{Tags: []string{","}}
		}},
		{"whitespace name", func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) { s.MemberSubnet.Name = " name" }},
		{"allocate missing network", func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) {
			s.FloatingIP.Mode = configv1alpha1.FloatingIPAllocate
		}},
		{"none with network", func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) {
			s.FloatingIP.ExternalNetwork = &configv1alpha1.ResourceReference{Name: "external"}
		}},
		{"unknown mode", func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) { s.FloatingIP.Mode = "Adopt" }},
		{"unknown address", func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) { s.NodeAddressType = "Hostname" }},
		{"flavor name", func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) { s.FlavorID = "small" }},
		{"zone whitespace", func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) { s.AvailabilityZone = " zone" }},
		{"relative health path", func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) { s.HealthMonitor.Path = "health" }},
		{"health control character", func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) { s.HealthMonitor.Path = "/health\n" }},
		{"timeout equals delay", func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) { s.HealthMonitor.Timeout = ptr(10) }},
		{"zero retries", func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) { s.HealthMonitor.MaxRetries = ptr(0) }},
		{"too many retries", func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) { s.HealthMonitor.MaxRetriesDown = ptr(11) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			params := validParameters()
			test.mutate(&params.Spec)
			_, _, _, err := Validate(params.Spec)
			var invalid *InvalidParametersError
			if !errors.As(err, &invalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestResolveRejectsParametersBeforeCloudRead(t *testing.T) {
	ns := gatewayv1.Namespace("default")
	tests := []struct {
		name       string
		ref        *gatewayv1.ParametersReference
		parameters *configv1alpha1.OpenStackGatewayClassConfig
	}{
		{name: "missing reference"},
		{name: "wrong group", ref: &gatewayv1.ParametersReference{Group: "other.example", Kind: configv1alpha1.GatewayClassConfigKind, Name: "parameters"}},
		{name: "namespace", ref: &gatewayv1.ParametersReference{Group: configv1alpha1.GroupName, Kind: configv1alpha1.GatewayClassConfigKind, Name: "parameters", Namespace: &ns}},
		{name: "missing object", ref: validReference()},
		{name: "invalid spec", ref: validReference(), parameters: &configv1alpha1.OpenStackGatewayClassConfig{ObjectMeta: metav1.ObjectMeta{Name: "parameters"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := configv1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if test.parameters != nil {
				builder = builder.WithObjects(test.parameters)
			}
			resolver := &fakeResolver{}
			_, err := Resolve(context.Background(), builder.Build(), resolver, &gatewayv1.GatewayClass{Spec: gatewayv1.GatewayClassSpec{ParametersRef: test.ref}})
			var invalid *InvalidParametersError
			if !errors.As(err, &invalid) || resolver.calls != 0 {
				t.Fatalf("error=%v calls=%d", err, resolver.calls)
			}
		})
	}
}

func TestResolveUsesCurrentParametersAndClassifiesCloudErrors(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := configv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	params := validParameters()
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(params).Build()
	resolver := &fakeResolver{}
	class := &gatewayv1.GatewayClass{Spec: gatewayv1.GatewayClassSpec{ParametersRef: validReference()}}
	resolved, err := Resolve(context.Background(), reader, resolver, class)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ParametersUID != params.UID || resolved.ParametersGeneration != 2 || resolved.Topology.MemberSubnetID != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("resolved=%#v", resolved)
	}
	if err := reader.Get(context.Background(), types.NamespacedName{Name: params.Name}, params); err != nil {
		t.Fatal(err)
	}
	params.Spec.MemberSubnet = configv1alpha1.ResourceReference{Name: "replacement"}
	if err := reader.Update(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(context.Background(), reader, resolver, class); err != nil {
		t.Fatal(err)
	}
	if resolver.request.MemberSubnet.Name != "replacement" {
		t.Fatal("reused obsolete topology request")
	}
	resolver.err = cloud.NewProviderError(cloud.ErrorCategoryTerminalValidation, errors.New("private-network-id"))
	_, err = Resolve(context.Background(), reader, resolver, class)
	var invalid *InvalidParametersError
	if !errors.As(err, &invalid) || invalid.Message != "GatewayClass topology or requested Amphora capability could not be validated" {
		t.Fatalf("error=%v", err)
	}
	resolver.err = cloud.ErrAuthorization
	if _, err = Resolve(context.Background(), reader, resolver, class); !errors.Is(err, cloud.ErrAuthorization) {
		t.Fatalf("error=%v", err)
	}
}

func validReference() *gatewayv1.ParametersReference {
	return &gatewayv1.ParametersReference{Group: configv1alpha1.GroupName, Kind: configv1alpha1.GatewayClassConfigKind, Name: "parameters"}
}

type fakeResolver struct {
	calls   int
	request cloud.TopologyRequest
	err     error
}

func (r *fakeResolver) ResolveTopology(_ context.Context, request cloud.TopologyRequest) (cloud.Topology, error) {
	r.calls++
	r.request = request
	return cloud.Topology{VIPSubnetID: request.VIPSubnet.ID, MemberSubnetID: "22222222-2222-2222-2222-222222222222"}, r.err
}

func TestResolveRedactsProviderErrorsAndPreservesTheirCauses(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := configv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(validParameters()).Build()
	class := &gatewayv1.GatewayClass{Spec: gatewayv1.GatewayClassSpec{ParametersRef: validReference()}}
	cause := errors.New("token=private-token response=private-body")
	for _, providerErr := range []error{cause, cloud.NewProviderError(cloud.ErrorCategoryAuthorization, cause), cloud.NewProviderError(cloud.ErrorCategoryTimeout, cause)} {
		_, err := Resolve(t.Context(), reader, &fakeResolver{err: providerErr}, class)
		if !errors.Is(err, providerErr) || !errors.Is(err, cause) || err.Error() != "could not resolve GatewayClass topology" {
			t.Fatalf("Resolve() error = %v, want redacted error with original cause", err)
		}
	}
}

func TestValidateUnicodeFieldLimits(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*configv1alpha1.OpenStackGatewayClassConfigSpec)
		invalid bool
	}{
		{name: "name at limit", mutate: func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) {
			s.MemberSubnet.Name = strings.Repeat("가", 255)
		}},
		{name: "name over limit", invalid: true, mutate: func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) {
			s.MemberSubnet.Name = strings.Repeat("가", 256)
		}},
		{name: "name edge whitespace", invalid: true, mutate: func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) { s.MemberSubnet.Name = "\u00a0members" }},
		{name: "name control", invalid: true, mutate: func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) { s.MemberSubnet.Name = "member\u0085name" }},
		{name: "tag at limit", mutate: func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) {
			s.MemberSubnet = configv1alpha1.ResourceReference{Tags: []string{strings.Repeat("가", 60)}}
		}},
		{name: "tag over limit", invalid: true, mutate: func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) {
			s.MemberSubnet = configv1alpha1.ResourceReference{Tags: []string{strings.Repeat("가", 61)}}
		}},
		{name: "health path at limit", mutate: func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) {
			s.HealthMonitor.Path = "/" + strings.Repeat("가", 254)
		}},
		{name: "health path over limit", invalid: true, mutate: func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) {
			s.HealthMonitor.Path = "/" + strings.Repeat("가", 255)
		}},
		{name: "health path control", invalid: true, mutate: func(s *configv1alpha1.OpenStackGatewayClassConfigSpec) { s.HealthMonitor.Path = "/health\u0085" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			parameters := validParameters()
			test.mutate(&parameters.Spec)
			_, _, _, err := Validate(parameters.Spec)
			if (err != nil) != test.invalid {
				t.Fatalf("Validate() error = %v, want invalid %t", err, test.invalid)
			}
		})
	}
}
