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

package diagnostic

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayconsts "sigs.k8s.io/gateway-api/pkg/consts"
)

func testConfig() Config {
	return Config{
		Kubeconfig: "/private/example.kubeconfig", Context: "diagnostic", APIServer: "https://cluster.example.test:6443", ClusterUID: "cluster-uid",
		Namespace: testNamespace, ControllerName: "example.test/gateway", GatewayClass: "diagnostic", GatewayClassUID: "class-uid",
		ClassConfig: "diagnostic", ClassConfigUID: "parameters-uid", BackendImage: "example.test/echo-basic@sha256:" + strings.Repeat("a", 64),
		BackendNodeSelector: map[string]string{"example.test/diagnostic-worker": "true"}, ReportPath: "/private/diagnostic.json", AcceptResourceCreation: true,
	}
}

func preflightObjects() []client.Object {
	cfg := testConfig()
	objects := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(cfg.ClusterUID)}},
		&gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: cfg.GatewayClass, UID: types.UID(cfg.GatewayClassUID), Generation: 1},
			Spec:   gatewayv1.GatewayClassSpec{ControllerName: gatewayv1.GatewayController(cfg.ControllerName), ParametersRef: &gatewayv1.ParametersReference{Group: configv1alpha1.GroupName, Kind: configv1alpha1.GatewayClassConfigKind, Name: cfg.ClassConfig}},
			Status: gatewayv1.GatewayClassStatus{Conditions: []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue, ObservedGeneration: 1}, {Type: "SupportedVersion", Status: metav1.ConditionTrue, ObservedGeneration: 1}}}},
		&configv1alpha1.OpenStackGatewayClassConfig{ObjectMeta: metav1.ObjectMeta{Name: cfg.ClassConfig, UID: types.UID(cfg.ClassConfigUID)}, Spec: configv1alpha1.OpenStackGatewayClassConfigSpec{
			VIPSubnet: configv1alpha1.ResourceReference{ID: "11111111-1111-1111-1111-111111111111"}, MemberSubnet: configv1alpha1.ResourceReference{ID: "22222222-2222-2222-2222-222222222222"}, FloatingIP: configv1alpha1.FloatingIPConfig{Mode: configv1alpha1.FloatingIPNone}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: cfg.BackendNodeSelector}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}, Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.10"}}}},
	}
	for _, name := range []string{"gatewayclasses", "gateways", "httproutes"} {
		objects = append(objects, &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: name + "." + gatewayv1.GroupName, Annotations: map[string]string{gatewayconsts.BundleVersionAnnotation: upstreamVersion, gatewayconsts.ChannelAnnotation: "standard"}}, Spec: apiextensionsv1.CustomResourceDefinitionSpec{Group: gatewayv1.GroupName}})
	}
	objects = append(objects, &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "openstackgatewayclassconfigs." + configv1alpha1.GroupName},
		Spec:       apiextensionsv1.CustomResourceDefinitionSpec{Group: configv1alpha1.GroupName, Scope: apiextensionsv1.ClusterScoped, Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: configv1alpha1.GatewayClassConfigKind}, Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1alpha1", Served: true, Storage: true, Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object"}}}}},
		Status:     apiextensionsv1.CustomResourceDefinitionStatus{Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue}}},
	})
	return objects
}

func TestPreflightRejectsUnsafeInputsWithoutWrites(t *testing.T) {
	for _, test := range []struct {
		name      string
		mutate    func([]client.Object) []client.Object
		wantError bool
	}{
		{name: "valid"},
		{name: "existing namespace", wantError: true, mutate: func(objects []client.Object) []client.Object {
			return append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}})
		}},
		{name: "wrong cluster", wantError: true, mutate: func(objects []client.Object) []client.Object { objects[0].SetUID("other"); return objects }},
		{name: "wrong controller", wantError: true, mutate: func(objects []client.Object) []client.Object {
			objects[1].(*gatewayv1.GatewayClass).Spec.ControllerName = "other.test/controller"
			return objects
		}},
		{name: "replaced class", wantError: true, mutate: func(objects []client.Object) []client.Object { objects[1].SetUID("other"); return objects }},
		{name: "stale acceptance", wantError: true, mutate: func(objects []client.Object) []client.Object { objects[1].SetGeneration(2); return objects }},
		{name: "wrong parameters", wantError: true, mutate: func(objects []client.Object) []client.Object {
			objects[1].(*gatewayv1.GatewayClass).Spec.ParametersRef.Name = "other"
			return objects
		}},
		{name: "replaced parameters", wantError: true, mutate: func(objects []client.Object) []client.Object { objects[2].SetUID("other"); return objects }},
		{name: "floating IP allocation", wantError: true, mutate: func(objects []client.Object) []client.Object {
			objects[2].(*configv1alpha1.OpenStackGatewayClassConfig).Spec.FloatingIP = configv1alpha1.FloatingIPConfig{Mode: configv1alpha1.FloatingIPAllocate, ExternalNetwork: &configv1alpha1.ResourceReference{ID: "33333333-3333-3333-3333-333333333333"}}
			return objects
		}},
		{name: "excluded worker", wantError: true, mutate: func(objects []client.Object) []client.Object {
			objects[3].SetLabels(map[string]string{"example.test/diagnostic-worker": "true", corev1.LabelNodeExcludeBalancers: ""})
			return objects
		}},
		{name: "bundle mismatch", wantError: true, mutate: func(objects []client.Object) []client.Object {
			objects[4].SetAnnotations(map[string]string{gatewayconsts.BundleVersionAnnotation: "v1.5.0", gatewayconsts.ChannelAnnotation: "standard"})
			return objects
		}},
		{name: "experimental bundle", wantError: true, mutate: func(objects []client.Object) []client.Object {
			objects[4].SetAnnotations(map[string]string{gatewayconsts.BundleVersionAnnotation: upstreamVersion, gatewayconsts.ChannelAnnotation: "experimental"})
			return objects
		}},
		{name: "missing class CRD", wantError: true, mutate: func(objects []client.Object) []client.Object { return objects[:len(objects)-1] }},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := preflightObjects()
			if test.mutate != nil {
				objects = test.mutate(objects)
			}
			c := fake.NewClientBuilder().WithScheme(diagnosticScheme(t)).WithObjects(objects...).Build()
			// The function accepts only client.Reader, preventing writes through this boundary.
			err := preflight(context.Background(), c, testConfig())
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v, wantError=%v", err, test.wantError)
			}
		})
	}
}

func TestConfigRejectsImplicitScopeAndUnknownFields(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.AcceptResourceCreation = false }, func(c *Config) { c.Context = "" }, func(c *Config) { c.ClusterUID = "" }, func(c *Config) { c.GatewayClassUID = "" }, func(c *Config) { c.ClassConfigUID = "" }, func(c *Config) { c.APIServer = "http://cluster.example.test" }, func(c *Config) { c.Namespace = "default" }, func(c *Config) { c.BackendNodeSelector = nil }, func(c *Config) { c.BackendImage = "example.test/echo:latest" },
	} {
		cfg := testConfig()
		mutate(&cfg)
		if cfg.validate() == nil {
			t.Fatalf("accepted invalid configuration: %#v", cfg)
		}
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw, err := json.Marshal(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw[:len(raw)-1], []byte(`,"unsupported":true}`)...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("accepted an unknown configuration field")
	}
}

func TestFixtureBoundAndReportScope(t *testing.T) {
	objects, err := fixture(testConfig(), "run-token")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 5 {
		t.Fatalf("fixture has %d objects", len(objects))
	}
	gateways := 0
	for _, object := range objects {
		if object.GetAnnotations()[runAnnotation] != "run-token" {
			t.Fatal("unbound fixture object")
		}
		if _, ok := object.(*gatewayv1.Gateway); ok {
			gateways++
		}
	}
	if gateways != 1 {
		t.Fatal("fixture must create exactly one Gateway")
	}
	service := objects[1].(*corev1.Service)
	if service.Spec.Type != corev1.ServiceTypeNodePort || service.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		t.Fatal("fixture widens backend member scope")
	}
	if *objects[2].(*appsv1.Deployment).Spec.Replicas != 1 {
		t.Fatal("fixture must use one backend Pod")
	}
	route := objects[4].(*gatewayv1.HTTPRoute)
	if route.Name != routeName || len(route.Spec.Rules) != 1 {
		t.Fatal("pinned upstream route identity changed")
	}
	output, err := json.Marshal(newReport())
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{testConfig().APIServer, testConfig().ClusterUID, testConfig().BackendImage, testConfig().ClassConfigUID, "profileReports"} {
		if strings.Contains(string(output), private) {
			t.Fatal("diagnostic report contains private identity or conformance report fields")
		}
	}
}

type deletionRecorder struct {
	client.Client
	deleted []string
}

func (c *deletionRecorder) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	opts := &client.DeleteOptions{}
	opts.ApplyOptions(options)
	if opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID != object.GetUID() {
		return context.Canceled
	}
	c.deleted = append(c.deleted, reflect.TypeOf(object).String())
	return c.Client.Delete(ctx, object, options...)
}

func cleanupFixture(t *testing.T) (*createdObjects, *deletionRecorder) {
	t.Helper()
	objects, err := fixture(testConfig(), "run")
	if err != nil {
		t.Fatal(err)
	}
	ledger := &createdObjects{runID: "run"}
	for index, object := range objects {
		object.SetUID(types.UID("uid-" + string(rune('a'+index))))
		if err := ledger.record(object); err != nil {
			t.Fatal(err)
		}
	}
	c := fake.NewClientBuilder().WithScheme(diagnosticScheme(t)).WithObjects(objects...).Build()
	return ledger, &deletionRecorder{Client: c}
}

func TestCleanupWaitsForControllerAndNeverDeletesReplacement(t *testing.T) {
	t.Run("dependency order", func(t *testing.T) {
		ledger, c := cleanupFixture(t)
		if err := ledger.cleanup(context.Background(), c, time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(c.deleted, []string{"*v1.HTTPRoute", "*v1.Gateway", "*v1.Namespace"}) {
			t.Fatalf("delete order=%v", c.deleted)
		}
	})
	t.Run("finalizer retains Gateway and namespace", func(t *testing.T) {
		ledger, c := cleanupFixture(t)
		gateway := ledger.gateway.DeepCopy()
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(gateway), gateway); err != nil {
			t.Fatal(err)
		}
		gateway.Finalizers = []string{"example.test/controller"}
		if err := c.Update(context.Background(), gateway); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		if err := ledger.cleanup(ctx, c, time.Millisecond); err == nil {
			t.Fatal("cleanup ignored a controller finalizer")
		}
		if !reflect.DeepEqual(c.deleted, []string{"*v1.HTTPRoute", "*v1.Gateway"}) {
			t.Fatalf("namespace deleted before finalization: %v", c.deleted)
		}
	})
	t.Run("replacement UID", func(t *testing.T) {
		ledger, c := cleanupFixture(t)
		ledger.gateway.UID = "previous-uid"
		if err := ledger.cleanup(context.Background(), c, time.Millisecond); err == nil || len(c.deleted) != 0 {
			t.Fatalf("replacement mutated: error=%v, deletes=%v", err, c.deleted)
		}
	})
	t.Run("unrecorded Gateway", func(t *testing.T) {
		ledger, c := cleanupFixture(t)
		ledger.gateway = nil
		if err := ledger.cleanup(context.Background(), c, time.Millisecond); err == nil || len(c.deleted) != 0 {
			t.Fatalf("uncertain create adopted: error=%v, deletes=%v", err, c.deleted)
		}
	})
	t.Run("namespace identity changed", func(t *testing.T) {
		ledger, c := cleanupFixture(t)
		namespace := ledger.namespace.DeepCopy()
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(namespace), namespace); err != nil {
			t.Fatal(err)
		}
		namespace.Annotations[runAnnotation] = "other"
		if err := c.Update(context.Background(), namespace); err != nil {
			t.Fatal(err)
		}
		if err := ledger.cleanup(context.Background(), c, time.Millisecond); err == nil || len(c.deleted) != 0 {
			t.Fatalf("foreign namespace mutated: error=%v, deletes=%v", err, c.deleted)
		}
	})
}

func TestActualBackendWorkerGatesGatewayCreation(t *testing.T) {
	cfg := testConfig()
	objects, err := fixture(cfg, "run")
	if err != nil {
		t.Fatal(err)
	}
	service := objects[1].(*corev1.Service)
	service.UID = "service-uid"
	service.Spec.Ports[0].NodePort = 30080
	deployment := objects[2].(*appsv1.Deployment)
	deployment.UID = "deployment-uid"
	deployment.Generation = 1
	deployment.Status = appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1}
	yes := true
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "backend-rs", Namespace: cfg.Namespace, UID: "rs-uid", OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", UID: deployment.UID, Name: deployment.Name, Controller: &yes}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "backend-pod", Namespace: cfg.Namespace, UID: "pod-uid", OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: &yes}}}, Spec: corev1.PodSpec{NodeName: "worker"}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	endpoint := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: cfg.Namespace, Labels: map[string]string{discoveryv1.LabelServiceName: backendName}}, Endpoints: []discoveryv1.Endpoint{{Conditions: discoveryv1.EndpointConditions{Ready: &yes}, NodeName: &pod.Spec.NodeName, TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: cfg.Namespace, Name: pod.Name, UID: pod.UID}}}}
	node := preflightObjects()[3].(*corev1.Node)
	for _, test := range []struct {
		name   string
		mutate func(*corev1.Node, *discoveryv1.EndpointSlice)
		want   bool
	}{
		{name: "eligible actual worker", want: true},
		{name: "control plane", mutate: func(n *corev1.Node, _ *discoveryv1.EndpointSlice) {
			n.Labels["node-role.kubernetes.io/control-plane"] = ""
		}},
		{name: "worker excluded", mutate: func(n *corev1.Node, _ *discoveryv1.EndpointSlice) {
			n.Labels[corev1.LabelNodeExcludeBalancers] = "true"
		}},
		{name: "worker outside selector", mutate: func(n *corev1.Node, _ *discoveryv1.EndpointSlice) { n.Labels = nil }},
		{name: "invalid selected address", mutate: func(n *corev1.Node, _ *discoveryv1.EndpointSlice) { n.Status.Addresses[0].Address = "invalid" }},
		{name: "wrong Pod UID", mutate: func(_ *corev1.Node, e *discoveryv1.EndpointSlice) { e.Endpoints[0].TargetRef.UID = "replacement" }},
		{name: "not ready endpoint", mutate: func(_ *corev1.Node, e *discoveryv1.EndpointSlice) { no := false; e.Endpoints[0].Conditions.Ready = &no }},
	} {
		t.Run(test.name, func(t *testing.T) {
			n, e := node.DeepCopy(), endpoint.DeepCopy()
			if test.mutate != nil {
				test.mutate(n, e)
			}
			c := fake.NewClientBuilder().WithScheme(diagnosticScheme(t)).WithObjects(service, deployment, rs, pod, n, e).Build()
			ready, err := backendReady(context.Background(), c, cfg, service, deployment, corev1.NodeInternalIP)
			if ready != test.want || test.want && err != nil {
				t.Fatalf("ready=%v error=%v want=%v", ready, err, test.want)
			}
		})
	}
}

func TestCleanupRetainsNamespaceWithLoadBalancerService(t *testing.T) {
	ledger, c := cleanupFixture(t)
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "foreign-lb", Namespace: testNamespace, UID: "foreign-service"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}}
	if err := c.Create(context.Background(), service); err != nil {
		t.Fatal(err)
	}
	if err := ledger.cleanup(context.Background(), c, time.Millisecond); err == nil {
		t.Fatal("namespace deletion would finalize a foreign OCCM graph")
	}
	if !reflect.DeepEqual(c.deleted, []string{"*v1.HTTPRoute", "*v1.Gateway"}) {
		t.Fatalf("unexpected deletes: %v", c.deleted)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(service), service); err != nil {
		t.Fatal(err)
	}
}

func TestUncertainCreateDoesNotAuthorizeCleanup(t *testing.T) {
	ledger, c := cleanupFixture(t)
	ledger.uncertainCreate = true
	if err := ledger.cleanup(context.Background(), c, time.Millisecond); err == nil || len(c.deleted) != 0 {
		t.Fatalf("error=%v deletes=%v", err, c.deleted)
	}
}

func TestRunBudgetReservesControllerCleanup(t *testing.T) {
	now := time.Now()
	if requireRunBudget(now, now.Add(10*time.Minute), true) == nil {
		t.Fatal("default Go timeout accepted before mutation")
	}
	if err := requireRunBudget(now, now.Add(75*time.Minute), true); err != nil {
		t.Fatal(err)
	}
	if err := requireRunBudget(now, time.Time{}, false); err != nil {
		t.Fatal(err)
	}
}
