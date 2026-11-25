//go:build envtest

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

package controller_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller/gatewayclass"
)

func testGatewayDependencyWatches(t *testing.T, restConfig *rest.Config) {
	t.Helper()
	scheme := testScheme(t)
	manager, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("create watch test manager: %v", err)
	}
	config := testConfig()
	// Keep the running controllers separate from the manually reconciled fixtures.
	config.ControllerName = "watch.example.com/gateway-api-openstack"
	if err := controller.SetupIndexes(t.Context(), manager.GetFieldIndexer(), config); err != nil {
		t.Fatalf("set up watch test indexes: %v", err)
	}
	provider := &recordingProvider{}
	gatewayReconciler := newEnvtestGatewayReconciler(manager.GetClient(), manager.GetAPIReader(), provider, config)
	if err := gatewayReconciler.SetupWithManager(manager); err != nil {
		t.Fatalf("set up Gateway watches: %v", err)
	}
	classReconciler := &gatewayclass.Reconciler{
		Client: manager.GetClient(), APIReader: manager.GetAPIReader(), TopologyResolver: provider, Config: config,
	}
	if err := classReconciler.SetupWithManager(manager); err != nil {
		t.Fatalf("set up GatewayClass watches: %v", err)
	}
	managerContext, stopManager := context.WithCancel(context.Background())
	managerResult := make(chan error, 1)
	go func() { managerResult <- manager.Start(managerContext) }()
	t.Cleanup(func() {
		stopManager()
		select {
		case err := <-managerResult:
			if err != nil {
				t.Errorf("stop watch test manager: %v", err)
			}
			// Read recorded requests only after the controller worker has stopped.
			if len(provider.Calls) != 0 {
				t.Errorf("certificate validation issued %d cloud requests", len(provider.Calls))
			}
		case <-time.After(envtestWaitTimeout):
			t.Error("watch test manager did not stop")
		}
	})
	syncContext, cancelSync := context.WithTimeout(t.Context(), envtestWaitTimeout)
	defer cancelSync()
	if !manager.GetCache().WaitForCacheSync(syncContext) {
		t.Fatal("watch test manager cache did not sync")
	}
	apiClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create watch test API client: %v", err)
	}
	const gatewayNamespace = "watch-apps-envtest"
	const certificateNamespace = "watch-certificates-envtest"
	for _, name := range []string{gatewayNamespace, certificateNamespace} {
		if err := apiClient.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	parameters := envtestClassParameters("watch-envtest")
	class := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "watch-envtest"},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: config.ControllerName,
			ParametersRef: &gatewayv1.ParametersReference{
				Group: configv1alpha1.GroupName, Kind: configv1alpha1.GatewayClassConfigKind, Name: parameters.Name,
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: certificateNamespace, Name: "certificate"},
		Type:       corev1.SecretTypeTLS,
		Data:       envtestCertificateData(t),
	}
	grant := &gatewayv1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Namespace: certificateNamespace, Name: "certificate"},
		Spec: gatewayv1.ReferenceGrantSpec{
			From: []gatewayv1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "Gateway", Namespace: gatewayNamespace}},
			To:   []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Secret", Name: ptr.To(gatewayv1.ObjectName(secret.Name))}},
		},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: gatewayNamespace, Name: "certificates"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(class.Name),
			Listeners: []gatewayv1.Listener{{
				Name: "https", Protocol: gatewayv1.HTTPSProtocolType, Port: 443,
				TLS: &gatewayv1.ListenerTLSConfig{CertificateRefs: []gatewayv1.SecretObjectReference{{
					Name: gatewayv1.ObjectName(secret.Name), Namespace: ptr.To(gatewayv1.Namespace(secret.Namespace)),
				}}},
			}},
		},
	}
	for _, object := range []client.Object{parameters, class, secret, grant, gateway} {
		if err := apiClient.Create(t.Context(), object); err != nil {
			t.Fatalf("create %T: %v", object, err)
		}
	}
	key := client.ObjectKeyFromObject(gateway)
	check := func(status metav1.ConditionStatus, reason gatewayv1.ListenerConditionReason) {
		t.Helper()
		waitForEnvtest(t, func(ctx context.Context) (bool, error) {
			var current gatewayv1.Gateway
			if err := apiClient.Get(ctx, key, &current); err != nil {
				return false, err
			}
			if current.Generation != gateway.Generation {
				t.Fatalf("Gateway generation = %d, want unchanged %d", current.Generation, gateway.Generation)
			}
			if len(current.Status.Listeners) != 1 {
				return false, nil
			}
			resolved := meta.FindStatusCondition(current.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionResolvedRefs))
			return resolved != nil && resolved.Status == status && resolved.Reason == string(reason) && resolved.ObservedGeneration == gateway.Generation, nil
		})
	}
	check(metav1.ConditionTrue, gatewayv1.ListenerReasonResolvedRefs)

	// No Gateway update or manual Reconcile accompanies these dependency changes.
	// A Secret data update changes resourceVersion without advancing generation.
	secretGeneration := secret.Generation
	secret.Data[corev1.TLSPrivateKeyKey] = []byte("invalid-rotated-key")
	if err := apiClient.Update(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	if secret.Generation != secretGeneration {
		t.Fatalf("Secret generation = %d, want %d", secret.Generation, secretGeneration)
	}
	check(metav1.ConditionFalse, gatewayv1.ListenerReasonInvalidCertificateRef)
	secret.Data = envtestCertificateData(t)
	if err := apiClient.Update(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	check(metav1.ConditionTrue, gatewayv1.ListenerReasonResolvedRefs)
	if err := apiClient.Delete(t.Context(), grant); err != nil {
		t.Fatal(err)
	}
	check(metav1.ConditionFalse, gatewayv1.ListenerReasonRefNotPermitted)
}
