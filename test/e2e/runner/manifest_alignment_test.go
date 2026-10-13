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

package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/test/e2e/internal/controllercontract"
)

func TestBaseManifestsAndExampleUseTheRegisteredClassAPI(t *testing.T) {
	root := testRepositoryRoot(t)
	base, err := loadBaseObjects(root)
	if err != nil {
		t.Fatal(err)
	}
	example, err := os.ReadFile(filepath.Join(root, "examples/basic.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(example)), 4096)
	var class gatewayv1.GatewayClass
	var parameters configv1alpha1.OpenStackGatewayClassConfig
	for {
		var object unstructured.Unstructured
		if err := decoder.Decode(&object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		switch object.GetKind() {
		case "GatewayClass":
			if class.Name != "" {
				t.Fatal("basic example has more than one GatewayClass")
			}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &class); err != nil {
				t.Fatal(err)
			}
		case configv1alpha1.GatewayClassConfigKind:
			if parameters.Name != "" {
				t.Fatal("basic example has more than one class configuration")
			}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &parameters); err != nil {
				t.Fatal(err)
			}
		}
	}
	if class.Name == "" || parameters.Name == "" {
		t.Fatal("basic example is missing its GatewayClass or parameters")
	}
	if parameters.APIVersion != configv1alpha1.GroupVersion.String() || parameters.Namespace != "" {
		t.Fatalf("example parameter API identity = %#v", parameters.TypeMeta)
	}
	reference := class.Spec.ParametersRef
	if reference == nil || string(reference.Group) != configv1alpha1.GroupName || reference.Kind != configv1alpha1.GatewayClassConfigKind || reference.Name != parameters.Name || reference.Namespace != nil {
		t.Fatalf("example class references a different configuration API: %#v", reference)
	}
	controllerName := base.configMap.Data[controllercontract.EnvControllerName]
	domain, _, qualified := strings.Cut(controllerName, "/")
	if !qualified || domain != configv1alpha1.GroupName || string(class.Spec.ControllerName) != controllerName {
		t.Fatalf("base controller identity %q and example class %q do not agree with the registered API group", controllerName, class.Spec.ControllerName)
	}
	definition := installedClassConfigCRD(t)
	for _, rule := range base.clusterRole.Rules {
		if slices.Contains(rule.APIGroups, configv1alpha1.GroupName) && slices.Contains(rule.Resources, definition.Spec.Names.Plural) && slices.Contains(rule.Verbs, "get") && slices.Contains(rule.Verbs, "list") && slices.Contains(rule.Verbs, "watch") {
			return
		}
	}
	t.Fatal("base controller RBAC cannot read the registered class configuration API")
}
