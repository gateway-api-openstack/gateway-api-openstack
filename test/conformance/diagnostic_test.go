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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/conformance/tests"
	upstreamconfig "sigs.k8s.io/gateway-api/conformance/utils/config"
	"sigs.k8s.io/gateway-api/conformance/utils/suite"
	"sigs.k8s.io/gateway-api/pkg/features"
)

const (
	cloudObservationTimeout = 10 * time.Minute
	setupTimeout            = 10 * time.Minute
	cleanupTimeout          = 15 * time.Minute
	// Reserve four sequential upstream observation windows, plus the smaller
	// listener/HTTP checks and time for cleanup before Go's hard deadline.
	minimumRemainingBudget = setupTimeout + 4*cloudObservationTimeout + cleanupTimeout + 5*time.Minute
)

func requireRunBudget(now, deadline time.Time, hasDeadline bool) error {
	if hasDeadline && deadline.Sub(now) < minimumRemainingBudget {
		return fmt.Errorf("diagnostic needs at least %s remaining before Go's hard timeout; use -timeout=75m", minimumRemainingBudget)
	}
	return nil
}

// TestOpenStackDiagnostic is deliberately excluded from ordinary execution by an
// explicit opt-in. It may cause the installed controller to create one cloud graph.
func TestOpenStackDiagnostic(t *testing.T) {
	if os.Getenv(optInEnvironment) != "true" {
		t.Skip("live diagnostic requires explicit opt-in")
	}
	deadline, hasDeadline := t.Deadline()
	if err := requireRunBudget(time.Now(), deadline, hasDeadline); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(os.Getenv(configEnvironment))
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(cfg.ReportPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("reserve a new private diagnostic report: %v", err)
	}
	result := newReport()
	defer func() {
		if err := writeReport(file, result); err != nil {
			t.Error(err)
		}
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := writeReport(file, result); err != nil {
		t.Fatal(err)
	}
	result.Status, result.Reason = "setupblocked", "Diagnostic prerequisites were not satisfied"
	rest, err := restConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	scheme := diagnosticScheme(t)
	kubeClient, err := client.New(rest, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create diagnostic Kubernetes client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	if err := preflight(ctx, kubeClient, cfg); err != nil {
		t.Fatal(err)
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		t.Fatalf("create diagnostic run identity: %v", err)
	}
	created := &createdObjects{runID: hex.EncodeToString(token)}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if err := created.cleanup(cleanupCtx, kubeClient, 2*time.Second); err != nil {
			result.Cleanup, result.Status, result.Reason = "blocked", "fail", "Cleanup is blocked; retain objects and restore controller finalization"
			t.Errorf("diagnostic cleanup: %v", err)
		} else if created.namespace != nil {
			result.Cleanup = "complete"
		}
	}()
	objects, err := fixture(cfg, created.runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range objects {
		if _, gateway := object.(*gatewayv1.Gateway); gateway {
			// Class parameters are shared inputs. Re-read them immediately
			// before the only operation that authorizes cloud provisioning.
			addressType, err := requireClass(ctx, kubeClient, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := waitForBackend(ctx, kubeClient, cfg, objects, addressType); err != nil {
				t.Fatal(err)
			}
			currentAddressType, err := requireClass(ctx, kubeClient, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if currentAddressType != addressType {
				t.Fatal("class address selection changed while preparing the backend")
			}
		}
		if err := kubeClient.Create(ctx, object); err != nil {
			created.uncertainCreate = true
			t.Fatalf("create diagnostic fixture: %v", err)
		}
		if err := created.record(object); err != nil {
			created.uncertainCreate = true
			t.Fatal(err)
		}
	}
	timeouts := upstreamconfig.DefaultTimeoutConfig()
	timeouts.GatewayMustHaveAddress = cloudObservationTimeout
	timeouts.GatewayMustHaveCondition = cloudObservationTimeout
	timeouts.RouteMustHaveParents = cloudObservationTimeout
	timeouts.HTTPRouteMustHaveCondition = cloudObservationTimeout
	conformanceSuite, err := suite.NewConformanceTestSuite(suite.ConformanceOptions{
		ConfigurableOptions: suite.ConfigurableOptions{
			GatewayClassName:  cfg.GatewayClass,
			SupportedFeatures: []features.FeatureName{features.SupportGateway, features.SupportHTTPRoute},
			TimeoutConfig:     timeouts,
		},
		Client: kubeClient,
	})
	if err != nil {
		t.Fatalf("initialize pinned diagnostic assertions: %v", err)
	}
	conformanceSuite.ControllerName = cfg.ControllerName
	result.Status, result.Reason = "fail", "The selected upstream assertions did not pass"
	if t.Run(selectedTest, func(t *testing.T) { tests.HTTPRouteSimpleSameNamespace.Test(t, conformanceSuite) }) {
		result.Status, result.Reason = "pass", "The selected assertions passed with the recorded fixture adaptations"
	}
}

func diagnosticScheme(t testing.TB) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, discoveryv1.AddToScheme, appsv1.AddToScheme, gatewayv1.Install, configv1alpha1.AddToScheme, apiextensionsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}
