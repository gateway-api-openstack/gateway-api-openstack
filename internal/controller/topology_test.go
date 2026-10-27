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

package controller

import (
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/classconfig"
)

func testResolvedTopology() classconfig.Resolved {
	return classconfig.Resolved{
		Topology:        cloud.Topology{VIPSubnetID: "11111111-1111-1111-1111-111111111111", MemberSubnetID: "22222222-2222-2222-2222-222222222222"},
		NodeAddressType: corev1.NodeInternalIP, HealthMonitor: cloud.HealthMonitorConfig{Path: "/", Delay: 10, Timeout: 5, MaxRetries: 3, MaxRetriesDown: 3},
		ParametersName: "parameters", ParametersUID: "parameters-uid", ParametersGeneration: 1,
	}
}

func TestGatewayTopologyBindingRetainsValidatedConfiguration(t *testing.T) {
	config := Config{ControllerName: "gateway.example/controller"}
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"example.org/foreign": "keep"}, Finalizers: []string{"example.org/finalizer"}}}
	resolved := testResolvedTopology()
	if err := BindGatewayTopology(config, gateway, resolved); err != nil {
		t.Fatal(err)
	}
	before := gateway.DeepCopy()
	if err := BindGatewayTopology(config, gateway, resolved); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gateway, before) {
		t.Fatal("repeated binding changed metadata")
	}
	stored, err := StoredGatewayTopology(config, gateway)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, resolved) {
		t.Fatalf("stored=%#v", stored)
	}
	resolved.Topology.VIPSubnetID = "33333333-3333-3333-3333-333333333333"
	if err := BindGatewayTopology(config, gateway, resolved); !errors.Is(err, cloud.ErrOwnershipConflict) {
		t.Fatalf("replacement error=%v", err)
	}
	if !GatewayTopologyMetadataEqual(config, before, gateway) {
		t.Fatal("rejected replacement changed binding")
	}
	ClearGatewayTopology(config, gateway)
	if HasGatewayTopologyBinding(config, gateway) || gateway.Annotations["example.org/foreign"] != "keep" || !reflect.DeepEqual(gateway.Finalizers, []string{"example.org/finalizer"}) {
		t.Fatalf("clear changed foreign metadata: %#v", gateway.ObjectMeta)
	}
}

func TestGatewayTopologyBindingRejectsIncompleteOrChangedMetadata(t *testing.T) {
	config := Config{ControllerName: "gateway.example/controller"}
	tests := []struct {
		name   string
		mutate func(*gatewayv1.Gateway)
	}{
		{"annotation missing", func(g *gatewayv1.Gateway) { delete(g.Annotations, config.GatewayTopologyAnnotation()) }},
		{"finalizer missing", func(g *gatewayv1.Gateway) { g.Finalizers = nil }},
		{"annotation changed", func(g *gatewayv1.Gateway) { g.Annotations[config.GatewayTopologyAnnotation()] += " " }},
		{"duplicate binding", func(g *gatewayv1.Gateway) {
			g.Finalizers = append(g.Finalizers, config.GatewayTopologyFinalizer("other"))
		}},
		{"invalid JSON with matching digest", func(g *gatewayv1.Gateway) {
			g.Annotations[config.GatewayTopologyAnnotation()] = "{}"
			g.Finalizers = []string{config.GatewayTopologyFinalizer("{}")}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gateway := &gatewayv1.Gateway{}
			if err := BindGatewayTopology(config, gateway, testResolvedTopology()); err != nil {
				t.Fatal(err)
			}
			test.mutate(gateway)
			if _, err := StoredGatewayTopology(config, gateway); !errors.Is(err, cloud.ErrOwnershipConflict) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestGatewaySecurityBinding(t *testing.T) {
	config := Config{ControllerName: "gateway.example/controller"}
	resolved := testResolvedTopology()
	resolved.Topology.VIPSecurityGroupIDs = []string{"33333333-3333-3333-3333-333333333333"}
	resolved.Topology.BackendSecurityGroupIDs = []string{"44444444-4444-4444-4444-444444444444"}
	gateway := &gatewayv1.Gateway{}
	if err := BindGatewayTopology(config, gateway, resolved); err != nil {
		t.Fatal(err)
	}
	stored, err := StoredGatewayTopology(config, gateway)
	if err != nil || !reflect.DeepEqual(stored, resolved) {
		t.Fatalf("stored security binding=%#v, error=%v", stored.Topology, err)
	}
	resolved.Topology.BackendSecurityGroupIDs[0] = "55555555-5555-5555-5555-555555555555"
	if err := BindGatewayTopology(config, gateway, resolved); !errors.Is(err, cloud.ErrOwnershipConflict) {
		t.Fatalf("changed security binding error=%v", err)
	}
	for _, ids := range [][]string{{"invalid"}, {"33333333-3333-3333-3333-333333333333", "33333333-3333-3333-3333-333333333333"}, {"44444444-4444-4444-4444-444444444444", "33333333-3333-3333-3333-333333333333"}} {
		resolved.Topology.VIPSecurityGroupIDs = ids
		if err := BindGatewayTopology(config, &gatewayv1.Gateway{}, resolved); !errors.Is(err, cloud.ErrOwnershipConflict) {
			t.Fatalf("invalid security IDs %v error=%v", ids, err)
		}
	}
}
