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

package v1alpha1

import (
	"context"
	"os"
	"strings"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	structuralcel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

func TestClassConfigurationSchema(t *testing.T) {
	content, err := os.ReadFile("../../config/crd/gateway-api-openstack.github.io_openstackgatewayclassconfigs.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var definition apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(content, &definition); err != nil {
		t.Fatal(err)
	}
	if definition.Spec.Group != GroupName || definition.Spec.Scope != apiextensionsv1.ClusterScoped || len(definition.Spec.Versions) != 1 || !definition.Spec.Versions[0].Storage {
		t.Fatal("CRD registration does not match Go API")
	}
	var schema apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(definition.Spec.Versions[0].Schema.OpenAPIV3Schema, &schema, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(&schema)
	if err != nil {
		t.Fatal(err)
	}
	if errs := structuralschema.ValidateStructural(field.NewPath("schema"), structural); len(errs) != 0 {
		t.Fatalf("non-structural CRD: %v", errs)
	}
	validator, _, err := validation.NewSchemaValidator(&schema)
	if err != nil {
		t.Fatal(err)
	}
	cel := structuralcel.NewValidator(structural, true, 1000000)
	tests := []struct {
		name    string
		change  func(map[string]any)
		invalid bool
	}{
		{name: "defaults"},
		{name: "allocate", change: func(s map[string]any) {
			s["floatingIP"] = map[string]any{"mode": "Allocate", "externalNetwork": map[string]any{"tags": []any{"public"}}}
		}},
		{name: "Unicode name at limit", change: func(s map[string]any) { s["memberSubnet"] = map[string]any{"name": strings.Repeat("가", 255)} }},
		{name: "Unicode name over limit", invalid: true, change: func(s map[string]any) { s["memberSubnet"] = map[string]any{"name": strings.Repeat("가", 256)} }},
		{name: "Unicode tag at limit", change: func(s map[string]any) { s["memberSubnet"] = map[string]any{"tags": []any{strings.Repeat("가", 60)}} }},
		{name: "Unicode tag over limit", invalid: true, change: func(s map[string]any) { s["memberSubnet"] = map[string]any{"tags": []any{strings.Repeat("가", 61)}} }},
		{name: "Unicode edge whitespace", invalid: true, change: func(s map[string]any) { s["memberSubnet"] = map[string]any{"name": "\u00a0members"} }},
		{name: "Unicode name control", invalid: true, change: func(s map[string]any) { s["memberSubnet"] = map[string]any{"name": "member\u0085name"} }},
		{name: "Unicode health path at limit", change: func(s map[string]any) { s["healthMonitor"] = map[string]any{"path": "/" + strings.Repeat("가", 254)} }},
		{name: "Unicode health path over limit", invalid: true, change: func(s map[string]any) { s["healthMonitor"] = map[string]any{"path": "/" + strings.Repeat("가", 255)} }},
		{name: "Unicode health path control", invalid: true, change: func(s map[string]any) { s["healthMonitor"] = map[string]any{"path": "/health\u0085"} }},
		{name: "two selectors", invalid: true, change: func(s map[string]any) { s["vipSubnet"].(map[string]any)["name"] = "other" }},
		{name: "no selector", invalid: true, change: func(s map[string]any) { s["vipSubnet"] = map[string]any{} }},
		{name: "malformed UUID", invalid: true, change: func(s map[string]any) { s["vipSubnet"] = map[string]any{"id": "id"} }},
		{name: "allocate without network", invalid: true, change: func(s map[string]any) { s["floatingIP"] = map[string]any{"mode": "Allocate"} }},
		{name: "none with network", invalid: true, change: func(s map[string]any) {
			s["floatingIP"] = map[string]any{"mode": "None", "externalNetwork": map[string]any{"name": "public"}}
		}},
		{name: "invalid tag", invalid: true, change: func(s map[string]any) { s["vipSubnet"] = map[string]any{"tags": []any{","}} }},
		{name: "timeout reaches delay", invalid: true, change: func(s map[string]any) { s["healthMonitor"] = map[string]any{"timeout": int64(10)} }},
		{name: "zero retries", invalid: true, change: func(s map[string]any) { s["healthMonitor"] = map[string]any{"maxRetries": int64(0)} }},
		{name: "invalid address", invalid: true, change: func(s map[string]any) { s["nodeAddressType"] = "Hostname" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object := map[string]any{"apiVersion": GroupVersion.String(), "kind": GatewayClassConfigKind, "metadata": map[string]any{"name": "test"}, "spec": map[string]any{"vipSubnet": map[string]any{"id": "11111111-1111-1111-1111-111111111111"}, "memberSubnet": map[string]any{"name": "members"}}}
			spec := object["spec"].(map[string]any)
			if test.change != nil {
				test.change(spec)
			}
			defaulting.Default(object, structural)
			errs := validation.ValidateCustomResource(field.NewPath("object"), object, validator)
			celErrs, _ := cel.Validate(context.Background(), field.NewPath("object"), structural, object, nil, 10000000)
			errs = append(errs, celErrs...)
			if (len(errs) > 0) != test.invalid {
				t.Fatalf("validation errors=%v", errs)
			}
			if test.name == "defaults" {
				var typed OpenStackGatewayClassConfig
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object, &typed); err != nil {
					t.Fatal(err)
				}
				if typed.Spec.NodeAddressType != "InternalIP" || typed.Spec.FloatingIP.Mode != FloatingIPNone || typed.Spec.HealthMonitor.Delay == nil || *typed.Spec.HealthMonitor.Delay != 10 || typed.Spec.HealthMonitor.Timeout == nil || *typed.Spec.HealthMonitor.Timeout != 5 {
					t.Fatalf("defaults=%#v", typed.Spec)
				}
			}
		})
	}
}

func TestClassConfigurationDeepCopy(t *testing.T) {
	delay := int32(10)
	original := &OpenStackGatewayClassConfig{Spec: OpenStackGatewayClassConfigSpec{
		VIPSubnet: ResourceReference{Tags: []string{"vip"}}, MemberSubnet: ResourceReference{Tags: []string{"member"}},
		FloatingIP: FloatingIPConfig{ExternalNetwork: &ResourceReference{Tags: []string{"external"}}}, HealthMonitor: HealthMonitorConfig{Delay: &delay},
	}}
	copied := original.DeepCopy()
	copied.Spec.VIPSubnet.Tags[0] = "changed"
	copied.Spec.MemberSubnet.Tags[0] = "changed"
	copied.Spec.FloatingIP.ExternalNetwork.Tags[0] = "changed"
	*copied.Spec.HealthMonitor.Delay = 20
	if original.Spec.VIPSubnet.Tags[0] != "vip" || original.Spec.MemberSubnet.Tags[0] != "member" || original.Spec.FloatingIP.ExternalNetwork.Tags[0] != "external" || *original.Spec.HealthMonitor.Delay != 10 {
		t.Fatal("deep copy aliases mutable specification fields")
	}
}
