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

package controllercontract

import (
	"context"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayconsts "sigs.k8s.io/gateway-api/pkg/consts"
)

func TestRequireGatewayAPICRDsChecksEveryInstalledDefinition(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		change  func([]client.Object) []client.Object
		wantErr bool
	}{
		{name: "pinned Standard bundle"},
		{name: "missing required CRD", change: func(objects []client.Object) []client.Object { return objects[1:] }, wantErr: true},
		{name: "extra CRD from different version", change: func(objects []client.Object) []client.Object {
			return append(objects, &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: "referencegrants.gateway.networking.k8s.io", Annotations: map[string]string{gatewayconsts.BundleVersionAnnotation: "v0.0.0"}}})
		}, wantErr: true},
		{name: "experimental channel", change: func(objects []client.Object) []client.Object {
			objects[0].GetAnnotations()[gatewayconsts.ChannelAnnotation] = "experimental"
			return objects
		}, wantErr: true},
		{name: "deleting CRD", change: func(objects []client.Object) []client.Object {
			now := metav1.Now()
			objects[0].SetDeletionTimestamp(&now)
			objects[0].SetFinalizers([]string{"test"})
			return objects
		}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var objects []client.Object
			for _, name := range []string{"gatewayclasses", "gateways", "httproutes"} {
				objects = append(objects, &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{
					Name:        name + ".gateway.networking.k8s.io",
					Annotations: map[string]string{gatewayconsts.BundleVersionAnnotation: gatewayconsts.BundleVersion, gatewayconsts.ChannelAnnotation: "standard"},
				}})
			}
			objects = append(objects, &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: "foreign.example.test"}})
			if test.change != nil {
				objects = test.change(objects)
			}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			if err := RequireGatewayAPICRDs(context.Background(), reader); (err != nil) != test.wantErr {
				t.Fatalf("RequireGatewayAPICRDs() = %v, want error %t", err, test.wantErr)
			}
		})
	}
}
