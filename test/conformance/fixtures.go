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
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	upstream "sigs.k8s.io/gateway-api/conformance"
	"sigs.k8s.io/yaml"
)

func fixture(cfg Config, runID string) ([]client.Object, error) {
	routeData, err := upstream.Manifests.ReadFile("tests/httproute-simple-same-namespace.yaml")
	if err != nil {
		return nil, fmt.Errorf("read pinned upstream route fixture: %w", err)
	}
	route := &gatewayv1.HTTPRoute{}
	if err := yaml.UnmarshalStrict(routeData, route); err != nil {
		return nil, fmt.Errorf("decode pinned upstream route fixture: %w", err)
	}
	if route.Name != routeName || route.Namespace != cfg.Namespace || len(route.Spec.ParentRefs) != 1 || route.Spec.ParentRefs[0].Name != gatewayName || len(route.Spec.Rules) != 1 || len(route.Spec.Rules[0].BackendRefs) != 1 || route.Spec.Rules[0].BackendRefs[0].Name != backendName {
		return nil, fmt.Errorf("pinned upstream route is outside the diagnostic fixture")
	}
	labels := map[string]string{"app": backendName}
	replicas := int32(1)
	objects := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.Namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: backendName, Namespace: cfg.Namespace}, Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort, ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal, Selector: labels,
			Ports: []corev1.ServicePort{{Name: "http", Protocol: corev1.ProtocolTCP, Port: 8080, TargetPort: intstr.FromInt32(3000)}},
		}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: backendName, Namespace: cfg.Namespace}, Spec: appsv1.DeploymentSpec{
			Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
				NodeSelector: cfg.BackendNodeSelector,
				Containers: []corev1.Container{{Name: backendName, Image: cfg.BackendImage, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")}}, Ports: []corev1.ContainerPort{{ContainerPort: 3000}},
					Env: []corev1.EnvVar{
						{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
						{Name: "NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
					},
					ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt32(3000)}}, InitialDelaySeconds: 1, PeriodSeconds: 2},
				}},
			}},
		}},
		&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: gatewayName, Namespace: cfg.Namespace}, Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(cfg.GatewayClass), Listeners: []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
		}},
		route,
	}
	for _, object := range objects {
		object.SetAnnotations(map[string]string{runAnnotation: runID})
	}
	return objects, nil
}
