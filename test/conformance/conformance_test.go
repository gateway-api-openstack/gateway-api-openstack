//go:build conformance

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

package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientset "k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/apis/v1alpha2"
	"sigs.k8s.io/gateway-api/apis/v1alpha3"
	xv1alpha1 "sigs.k8s.io/gateway-api/apisx/v1alpha1"
	upstream "sigs.k8s.io/gateway-api/conformance"
	confv1 "sigs.k8s.io/gateway-api/conformance/apis/v1"
	"sigs.k8s.io/gateway-api/conformance/tests"
	"sigs.k8s.io/gateway-api/conformance/utils/suite"
	"sigs.k8s.io/yaml"
)

// TestConformance is intentionally excluded from ordinary repository tests.
// The unmodified upstream setup needs several load balancers even for one test.
func TestConformance(t *testing.T) {
	if os.Getenv(enableEnvironment) != "true" {
		t.Skip("set GATEWAY_OPENSTACK_CONFORMANCE=true through the conformance Make target to opt in")
	}
	config, err := loadConfig(os.Getenv(configEnvironment))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(config.ArtifactsDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(config.ArtifactsDirectory, 0o700); err != nil {
		t.Fatalf("create a fresh artifact directory: %v", err)
	}
	record := runRecord{Config: config, BundleVersion: bundleVersion, StartedAt: time.Now().UTC(), Stage: "preflight", Cleanup: "not started"}
	t.Cleanup(func() {
		record.FinishedAt = time.Now().UTC()
		record.TestsPassed = !t.Failed()
		if err := writePrivateYAML(filepath.Join(config.ArtifactsDirectory, "run.yaml"), record); err != nil {
			t.Errorf("write run metadata: %v", err)
		}
	})
	log.SetLogger(zap.New(zap.WriteTo(os.Stdout)))
	restConfig, err := kubernetesConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{corev1.AddToScheme, gatewayv1.Install, v1alpha2.Install, v1alpha3.Install, xv1alpha1.Install, apiextensionsv1.AddToScheme} {
		if err := install(scheme); err != nil {
			t.Fatal(err)
		}
	}
	clientOptions := client.Options{Scheme: scheme}
	kubeClient, err := client.New(restConfig, clientOptions)
	if err != nil {
		t.Fatal(err)
	}
	fixtures, err := isolationFixtures(upstream.Manifests)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	gatewayClass, err := preflight(ctx, kubeClient, config, fixtures)
	if err != nil {
		t.Fatal(err)
	}
	record.AdvertisedFeatures = slices.Clone(gatewayClass.Status.SupportedFeatures)
	if err := validateDiagnosticTest(config, tests.ConformanceTests); err != nil {
		t.Fatal(err)
	}
	// Atomic creation prevents two runners from both passing absence checks.
	// A failed cleanup keeps this namespace as an operator-visible lock.
	runID := uuid.NewString()
	lock := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: lockNamespace, Labels: map[string]string{runLabel: runID}}}
	if err := kubeClient.Create(ctx, lock); err != nil {
		t.Fatalf("reserve dedicated conformance run: %v", err)
	}
	record.Cleanup = "pending"
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cleanupCancel()
		if err := waitForFixtureCleanup(cleanupCtx, kubeClient, fixtures); err != nil {
			record.Cleanup = "incomplete"
			t.Errorf("fixture cleanup incomplete; retain the controller, credentials, and run lock for recovery: %v", err)
			return
		}
		if err := kubeClient.Delete(cleanupCtx, lock, client.Preconditions{UID: &lock.UID}); err != nil && !apierrors.IsNotFound(err) {
			record.Cleanup = "incomplete"
			t.Errorf("release conformance run lock: %v", err)
			return
		}
		record.Cleanup = "Kubernetes fixtures absent"
	})
	clients, err := clientset.NewForConfig(restConfig)
	if err != nil {
		t.Fatal(err)
	}
	options := config.options()
	options.NamespaceLabels = map[string]string{runLabel: runID}
	conformanceSuite, err := suite.NewConformanceTestSuite(suite.ConformanceOptions{
		ConfigurableOptions: options,
		Client:              kubeClient, ClientOptions: clientOptions, Clientset: clients, RestConfig: restConfig,
		ManifestFS: []fs.FS{upstream.Manifests},
	})
	if err != nil {
		t.Fatal(err)
	}
	record.Stage = "setup"
	record.SuitePassed = t.Run("Upstream", func(t *testing.T) {
		conformanceSuite.Setup(t, tests.ConformanceTests)
		record.Stage = "tests"
		if err := conformanceSuite.Run(t, tests.ConformanceTests); err != nil {
			t.Fatal(err)
		}
		record.Stage = "completed"
	})
	// Wait for parallel subtests and upstream cleanup before saving the report.
	// Keep it verbatim even when its status differs from the Go test outcome.
	if record.Stage == "completed" {
		report, err := conformanceSuite.Report()
		if err != nil {
			t.Fatal(err)
		}
		if err := writePrivateYAML(filepath.Join(config.ArtifactsDirectory, "upstream-report.yaml"), report); err != nil {
			t.Fatal(err)
		}
		record.ReportWritten = true
		reportFailed := slices.ContainsFunc(report.ProfileReports, func(profile confv1.ProfileReport) bool {
			return profile.Core.Failed > 0 || profile.Extended != nil && profile.Extended.Failed > 0
		})
		record.ReportConsistent = reportFailed != record.SuitePassed
		if !record.ReportConsistent {
			t.Error("upstream report differs from the completed Go test outcome; retain both artifacts and do not publish a conformance claim")
		}
	}
}

type runRecord struct {
	Config             Config                       `json:"config"`
	BundleVersion      string                       `json:"bundleVersion"`
	StartedAt          time.Time                    `json:"startedAt"`
	FinishedAt         time.Time                    `json:"finishedAt"`
	Stage              string                       `json:"stage"`
	Cleanup            string                       `json:"cleanup"`
	AdvertisedFeatures []gatewayv1.SupportedFeature `json:"advertisedFeatures"`
	ReportWritten      bool                         `json:"reportWritten"`
	SuitePassed        bool                         `json:"suitePassed"`
	ReportConsistent   bool                         `json:"reportConsistent"`
	TestsPassed        bool                         `json:"testsPassed"`
}

func writePrivateYAML(path string, value any) error {
	data, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// The suite also creates namespaces and a GatewayClass outside its base file.
// Discover those isolation boundaries from the pinned fixtures instead of
// maintaining a second list of resource names.
func isolationFixtures(manifests fs.FS) ([]client.Object, error) {
	objects := make(map[string]client.Object)
	err := fs.WalkDir(manifests, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		data, err := fs.ReadFile(manifests, path)
		if err != nil {
			return err
		}
		decoder := kubeyaml.NewYAMLOrJSONDecoder(strings.NewReader(string(data)), 4096)
		for {
			var raw json.RawMessage
			if err := decoder.Decode(&raw); err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
			if len(raw) == 0 {
				continue
			}
			var object unstructured.Unstructured
			if err := json.Unmarshal(raw, &object); err != nil {
				return err
			}
			if object.GetKind() != "Namespace" && object.GetKind() != "GatewayClass" {
				continue
			}
			objects[object.GetKind()+"/"+object.GetName()] = &object
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read upstream fixture boundaries: %w", err)
	}
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	result := make([]client.Object, 0, len(keys))
	for _, key := range keys {
		result = append(result, objects[key])
	}
	return result, nil
}

func waitForFixtureCleanup(ctx context.Context, reader client.Reader, fixtures []client.Object) error {
	return wait.PollUntilContextCancel(ctx, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		for _, fixture := range fixtures {
			current := fixture.DeepCopyObject().(client.Object)
			if err := reader.Get(ctx, client.ObjectKeyFromObject(fixture), current); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return false, err
			}
			return false, nil
		}
		return true, nil
	})
}
