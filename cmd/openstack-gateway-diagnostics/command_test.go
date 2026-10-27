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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack/clients"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/diagnostics"
)

func TestCommandRedactsInputAndAuthenticationFailures(t *testing.T) {
	for _, test := range []struct {
		name      string
		failInput bool
	}{
		{name: "input", failInput: true},
		{name: "authentication"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			dependencies := commandDependencies{
				readInputs: func(context.Context, commandOptions) (diagnostics.Request, error) {
					if test.failInput {
						return diagnostics.Request{}, errors.New("private-kubeconfig-and-token")
					}
					return diagnostics.Request{}, nil
				},
				newInspector: func(context.Context, clients.ClientConfig, time.Duration) (diagnostics.Inspector, error) {
					return nil, errors.New("private-cloud-and-credential")
				},
			}
			code := runCommand(context.Background(), []string{"--class-config=test"}, emptyEnv, &stdout, &stderr, dependencies)
			if code != 2 || strings.Contains(stdout.String()+stderr.String(), "private-") {
				t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			var report diagnostics.Report
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || !report.HasFailures() {
				t.Fatalf("invalid failed report: %s (%v)", stdout.String(), err)
			}
			if report.ToolVersion != version || report.GeneratedAt.IsZero() {
				t.Fatalf("report lacks build version or generation time: %+v", report)
			}
		})
	}
}

func TestCommandKeepsUnknownChecksExplicit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	dependencies := commandDependencies{
		readInputs: func(ctx context.Context, options commandOptions) (diagnostics.Request, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("input reads have no deadline")
			}
			return diagnostics.Request{CheckBarbican: options.barbican}, nil
		},
		newInspector: func(context.Context, clients.ClientConfig, time.Duration) (diagnostics.Inspector, error) {
			return fakeInspector{}, nil
		},
	}
	code := runCommand(context.Background(), []string{"--class-config=test", "--check-barbican"}, emptyEnv, &stdout, &stderr, dependencies)
	if code != 0 || !strings.Contains(stdout.String(), `"status": "unknown"`) || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestOptionsRejectUnsafeValuesWithoutEcho(t *testing.T) {
	for _, args := range [][]string{
		{}, {"--class-config=test", "--timeout=0"}, {"--class-config=test", "--openstack-api-qps=0"},
		{"--class-config=test", "--openstack-api-burst=0"}, {"--class-config=test", "--octavia-microversion=2.4"},
		{"--class-config=test", "--node-selector=private-secret===x"}, {"--unknown=private-secret"},
	} {
		_, err := parseOptions(args, emptyEnv, io.Discard)
		if err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
	options, err := parseOptions([]string{"--class-config=test", "--openstack-region=flag-region"}, func(key string) string {
		if key == "OS_REGION_NAME" {
			return "env-region"
		}
		return ""
	}, io.Discard)
	if err != nil || options.openStack.Region != "flag-region" {
		t.Fatalf("flag did not override environment: %+v (%v)", options, err)
	}
}

func TestCollectInputsUsesClassDefaultsAndWorkerSelector(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := configv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	parameters := &configv1alpha1.OpenStackGatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: configv1alpha1.OpenStackGatewayClassConfigSpec{
			VIPSubnet: configv1alpha1.ResourceReference{Name: "private-subnet"}, MemberSubnet: configv1alpha1.ResourceReference{Name: "private-subnet"},
		},
	}
	worker := func(name, address string) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"diagnostic": "selected"}},
			Spec:       corev1.NodeSpec{ProviderID: "openstack:///" + name},
			Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: address}}},
		}
	}
	first, second := worker("a", "192.0.2.1"), worker("b", "192.0.2.2")
	controlPlane := worker("control-plane", "192.0.2.3")
	controlPlane.Labels["node-role.kubernetes.io/control-plane"] = ""
	unselected := worker("unselected", "192.0.2.4")
	unselected.Labels = nil
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(parameters, second, first, controlPlane, unselected).Build()
	request, err := collectInputs(context.Background(), reader, commandOptions{
		classConfig: "test", nodeSelector: labels.SelectorFromSet(labels.Set{"diagnostic": "selected"}), barbican: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Nodes) != 2 || request.Nodes[0].Address != "192.0.2.1" || request.Nodes[1].Address != "192.0.2.2" || !request.CheckBarbican {
		t.Fatalf("unexpected selected nodes: %+v", request)
	}
	if request.Topology.VIPSubnet.Name != "private-subnet" {
		t.Fatalf("class topology was lost: %+v", request.Topology)
	}
	first.Status.Addresses = append(first.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "192.0.2.5"})
	if selectedAddress(first, corev1.NodeInternalIP) != "" {
		t.Fatal("ambiguous selected addresses were accepted")
	}
}

type fakeInspector struct{}

func (fakeInspector) Inspect(_ context.Context, request diagnostics.Request) diagnostics.Report {
	report := diagnostics.NewReport()
	if request.CheckBarbican {
		report.Checks = append(report.Checks, diagnostics.Check{Name: "update", Status: diagnostics.Unknown, Message: "Not proved by a read."})
	}
	return report
}

func emptyEnv(string) string { return "" }
