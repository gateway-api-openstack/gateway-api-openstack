//go:build e2e

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

package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gateway-api-openstack/gateway-api-openstack/internal/audit"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/controller"
)

// verifyHTTPRouting reuses one Gateway and one forwarding rule. Extra logical
// listeners share its existing port, so this scenario adds no load balancer.
func (s *openStackSuite) verifyHTTPRouting(ctx context.Context) error {
	var gateway gatewayv1.Gateway
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: s.config.Namespace, Name: gatewayName}, &gateway); err != nil {
		return err
	}
	var route gatewayv1.HTTPRoute
	routeKey := client.ObjectKey{Namespace: s.config.Namespace, Name: routeName}
	if err := s.client.Get(ctx, routeKey, &route); err != nil {
		return err
	}
	originalGateway, originalRoute := gateway.Spec.DeepCopy(), route.Spec.DeepCopy()
	before, err := s.readOwnershipAudit(ctx, false)
	if err != nil {
		return err
	}
	alternate := gatewayv1.Hostname("alternate-" + s.hostname)
	listeners := []gatewayv1.Listener{
		{Name: listenerName, Protocol: gatewayv1.HTTPProtocolType, Port: listenerPort, Hostname: ptr.To(gatewayv1.Hostname(s.hostname))},
		{Name: "alternate", Protocol: gatewayv1.HTTPProtocolType, Port: listenerPort, Hostname: &alternate},
		{Name: "unsupported", Protocol: gatewayv1.UDPProtocolType, Port: 8081},
	}
	gateway.Spec.Listeners = listeners
	if err := s.patchRoutingGateway(ctx, gateway.Spec); err != nil {
		return err
	}
	route.Spec.Hostnames = append(route.Spec.Hostnames, alternate)
	route.Spec.Rules[0].Matches = []gatewayv1.HTTPRouteMatch{
		{Path: &gatewayv1.HTTPPathMatch{Type: ptr.To(gatewayv1.PathMatchExact), Value: ptr.To("/echo")}, Headers: []gatewayv1.HTTPHeaderMatch{{Name: "X-E2E-Route", Value: s.config.RunID}}},
		{Path: &gatewayv1.HTTPPathMatch{Type: ptr.To(gatewayv1.PathMatchExact), Value: ptr.To("/healthz")}},
	}
	route.Spec.ParentRefs = append(route.Spec.ParentRefs, gatewayv1.ParentReference{Name: gatewayName, SectionName: ptr.To(gatewayv1.SectionName("alternate"))})
	if err := s.patchRoutingRoute(ctx, routeKey, s.routeUID, route.Spec); err != nil {
		return err
	}
	if err := s.waitRoutingParents(ctx, routeKey, 2); err != nil {
		return err
	}
	if err := s.waitRoutingListeners(ctx, map[gatewayv1.SectionName]int32{listenerName: 1, "alternate": 1}, "unsupported"); err != nil {
		return err
	}
	if err := s.verifyRoutingHosts(ctx, []string{s.hostname, string(alternate)}); err != nil {
		return err
	}
	if err := s.verifyRoutingParentsUnchanged(ctx, before); err != nil {
		return err
	}
	if err := s.verifyRoutingPredicates(ctx); err != nil {
		return err
	}

	// Removing one section must preserve the other attachment on the same port.
	route.Spec.ParentRefs = route.Spec.ParentRefs[1:]
	if err := s.patchRoutingRoute(ctx, routeKey, s.routeUID, route.Spec); err != nil {
		return err
	}
	if err := s.waitRoutingParents(ctx, routeKey, 1); err != nil {
		return err
	}
	if err := s.waitRoutingListeners(ctx, map[gatewayv1.SectionName]int32{listenerName: 0, "alternate": 1}, "unsupported"); err != nil {
		return err
	}
	if err := s.verifyRoutingHosts(ctx, []string{string(alternate)}); err != nil {
		return err
	}
	if err := s.verifyRoutingRejected(ctx); err != nil {
		return fmt.Errorf("removed listener attachment: %w", err)
	}
	if err := s.patchRoutingRoute(ctx, routeKey, s.routeUID, *originalRoute); err != nil {
		return err
	}
	if err := s.patchRoutingGateway(ctx, *originalGateway); err != nil {
		return err
	}
	if err := s.waitForHTTPRoute(ctx); err != nil {
		return err
	}
	if err := s.waitForGateway(ctx, 1); err != nil {
		return err
	}
	if err := s.verifyCrossNamespaceRouting(ctx, *originalGateway, *originalRoute); err != nil {
		return err
	}
	if err := s.verifyRoutingParentsUnchanged(ctx, before); err != nil {
		return err
	}
	return s.verifyActiveOwnershipInventoryAudit(ctx)
}

func (s *openStackSuite) patchRoutingGateway(ctx context.Context, spec gatewayv1.GatewaySpec) error {
	var current gatewayv1.Gateway
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: s.config.Namespace, Name: gatewayName}, &current); err != nil {
		return err
	}
	if current.UID != s.gatewayUID || current.Annotations[runAnnotation] != s.config.RunID {
		return fmt.Errorf("Gateway identity changed during routing check")
	}
	base := current.DeepCopy()
	current.Spec = spec
	return s.client.Patch(ctx, &current, controller.OptimisticMergeFrom(base))
}

func (s *openStackSuite) patchRoutingRoute(ctx context.Context, key client.ObjectKey, uid types.UID, spec gatewayv1.HTTPRouteSpec) error {
	var current gatewayv1.HTTPRoute
	if err := s.client.Get(ctx, key, &current); err != nil {
		return err
	}
	if current.UID != uid || current.Annotations[runAnnotation] != s.config.RunID {
		return fmt.Errorf("HTTPRoute identity changed during routing check")
	}
	base := current.DeepCopy()
	current.Spec = spec
	return s.client.Patch(ctx, &current, controller.OptimisticMergeFrom(base))
}

func (s *openStackSuite) waitRoutingListeners(ctx context.Context, attached map[gatewayv1.SectionName]int32, rejected gatewayv1.SectionName) error {
	return wait.PollUntilContextCancel(ctx, s.config.PollInterval, true, func(ctx context.Context) (bool, error) {
		var gateway gatewayv1.Gateway
		if err := s.client.Get(ctx, client.ObjectKey{Namespace: s.config.Namespace, Name: gatewayName}, &gateway); err != nil {
			return false, err
		}
		if !gatewayHasReadyAddress(&gateway) || !exactCondition(gateway.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed), metav1.ConditionTrue, string(gatewayv1.GatewayReasonProgrammed), gateway.Generation) {
			return false, nil
		}
		if len(gateway.Status.Listeners) != len(attached)+1 {
			return false, nil
		}
		for _, listener := range gateway.Status.Listeners {
			if listener.Name == rejected {
				if !exactCondition(listener.Conditions, string(gatewayv1.ListenerConditionAccepted), metav1.ConditionFalse, string(gatewayv1.ListenerReasonUnsupportedProtocol), gateway.Generation) {
					return false, nil
				}
				continue
			}
			count, known := attached[listener.Name]
			if !known || listener.AttachedRoutes != count || !gatewayListenerConditionsReady(listener.Conditions, gateway.Generation) {
				return false, nil
			}
		}
		return true, nil
	})
}

func (s *openStackSuite) waitRoutingParents(ctx context.Context, key client.ObjectKey, count int) error {
	domain, _, _ := strings.Cut(s.config.ControllerName, "/")
	return wait.PollUntilContextCancel(ctx, s.config.PollInterval, true, func(ctx context.Context) (bool, error) {
		var route gatewayv1.HTTPRoute
		if err := s.client.Get(ctx, key, &route); err != nil {
			return false, err
		}
		own := 0
		for _, parent := range route.Status.Parents {
			if parent.ControllerName != gatewayv1.GatewayController(s.config.ControllerName) {
				continue
			}
			own++
			if !exactCondition(parent.Conditions, string(gatewayv1.RouteConditionAccepted), metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted), route.Generation) ||
				!exactCondition(parent.Conditions, string(gatewayv1.RouteConditionResolvedRefs), metav1.ConditionTrue, string(gatewayv1.RouteReasonResolvedRefs), route.Generation) ||
				!exactCondition(parent.Conditions, domain+"/Programmed", metav1.ConditionTrue, "Programmed", route.Generation) {
				return false, nil
			}
		}
		return own == count, nil
	})
}

func (s *openStackSuite) waitRoutingDenied(ctx context.Context, key client.ObjectKey, condition gatewayv1.RouteConditionType, reason gatewayv1.RouteConditionReason) error {
	config := controller.Config{ControllerName: gatewayv1.GatewayController(s.config.ControllerName)}
	return wait.PollUntilContextCancel(ctx, s.config.PollInterval, true, func(ctx context.Context) (bool, error) {
		var route gatewayv1.HTTPRoute
		if err := s.client.Get(ctx, key, &route); err != nil {
			return false, err
		}
		if controller.RouteHasControllerBinding(config, &route) {
			return false, nil
		}
		for _, parent := range route.Status.Parents {
			if parent.ControllerName == config.ControllerName && exactCondition(parent.Conditions, string(condition), metav1.ConditionFalse, string(reason), route.Generation) {
				return true, nil
			}
		}
		return false, nil
	})
}

func (s *openStackSuite) verifyRoutingHosts(ctx context.Context, hosts []string) error {
	return wait.PollUntilContextCancel(ctx, s.config.PollInterval, true, func(ctx context.Context) (bool, error) {
		address, err := s.gatewayAddress(ctx)
		if err != nil {
			return false, err
		}
		for _, host := range hosts {
			if !s.probeRoutingTraffic(ctx, address, host) {
				return false, nil
			}
		}
		return true, nil
	})
}

func (s *openStackSuite) verifyRoutingParentsUnchanged(ctx context.Context, before audit.Report) error {
	after, err := s.readOwnershipAudit(ctx, false)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(routingParentIDs(before), routingParentIDs(after)) {
		return fmt.Errorf("routing change replaced or added Gateway infrastructure")
	}
	return nil
}

func routingParentIDs(report audit.Report) map[string]string {
	ids := map[string]string{}
	for _, resource := range report.Resources {
		if resource.Type == "LoadBalancer" || resource.Type == "Listener" || resource.Type == "FloatingIP" {
			ids[resource.Service+"/"+resource.Type+"/"+resource.ID] = resource.ParentID
		}
	}
	return ids
}

func (s *openStackSuite) routingNamespaceName() string { return "gao-e2e-routes-" + s.config.RunID }

func (s *openStackSuite) verifyCrossNamespaceRouting(ctx context.Context, gatewaySpec gatewayv1.GatewaySpec, routeSpec gatewayv1.HTTPRouteSpec) error {
	// The original Route is fully removed before its cross-namespace replacement.
	if err := s.deleteHTTPRoute(ctx); err != nil {
		return err
	}
	if err := s.waitForGateway(ctx, 0); err != nil {
		return err
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.routingNamespaceName(), Annotations: map[string]string{runAnnotation: s.config.RunID}, Labels: map[string]string{"e2e.gateway-api-openstack.github.io/run": s.config.RunID}}}
	s.routingNamespaceCreated = true
	if err := s.client.Create(ctx, namespace); err != nil {
		return err
	}
	s.routingNamespaceUID = namespace.UID
	original := gatewaySpec.DeepCopy()
	gatewaySpec.Listeners[0].AllowedRoutes = &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: ptr.To(gatewayv1.NamespacesFromSelector), Selector: &metav1.LabelSelector{MatchLabels: namespace.Labels}}}
	if err := s.patchRoutingGateway(ctx, gatewaySpec); err != nil {
		return err
	}
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: namespace.Name, Name: routeName, Annotations: map[string]string{runAnnotation: s.config.RunID}}, Spec: *routeSpec.DeepCopy()}
	route.Spec.ParentRefs[0].Namespace = ptr.To(gatewayv1.Namespace(s.config.Namespace))
	route.Spec.Rules[0].BackendRefs[0].Namespace = ptr.To(gatewayv1.Namespace(s.config.Namespace))
	if err := s.client.Create(ctx, route); err != nil {
		return err
	}
	s.crossRouteUID = route.UID
	key := client.ObjectKeyFromObject(route)
	if err := s.waitRoutingDenied(ctx, key, gatewayv1.RouteConditionResolvedRefs, gatewayv1.RouteReasonRefNotPermitted); err != nil {
		return err
	}
	grant := &gatewayv1.ReferenceGrant{ObjectMeta: metav1.ObjectMeta{Namespace: s.config.Namespace, Name: "routing-backend", Annotations: map[string]string{runAnnotation: s.config.RunID}}, Spec: gatewayv1.ReferenceGrantSpec{
		From: []gatewayv1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: gatewayv1.Namespace(namespace.Name)}},
		To:   []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Service", Name: ptr.To(gatewayv1.ObjectName(backendName))}},
	}}
	for attempt := 0; attempt < 2; attempt++ {
		grant.ResourceVersion = ""
		grant.UID = ""
		if err := s.client.Create(ctx, grant); err != nil {
			return err
		}
		if err := s.waitRoutingParents(ctx, key, 1); err != nil {
			return err
		}
		if err := s.verifyExternalTraffic(ctx); err != nil {
			return err
		}
		if attempt == 0 {
			uid := grant.UID
			if err := s.deleteMarkedAndWait(ctx, grant, &uid); err != nil {
				return err
			}
			if err := s.waitRoutingDenied(ctx, key, gatewayv1.RouteConditionResolvedRefs, gatewayv1.RouteReasonRefNotPermitted); err != nil {
				return err
			}
			if err := s.verifyRoutingRejected(ctx); err != nil {
				return fmt.Errorf("revoked Service reference: %w", err)
			}
		}
	}
	// Namespace selector removal must detach the Route independently of its grant.
	base := namespace.DeepCopy()
	namespace.Labels = nil
	if err := s.client.Patch(ctx, namespace, controller.OptimisticMergeFrom(base)); err != nil {
		return err
	}
	if err := s.waitRoutingDenied(ctx, key, gatewayv1.RouteConditionAccepted, gatewayv1.RouteReasonNotAllowedByListeners); err != nil {
		return err
	}
	if err := s.waitForGateway(ctx, 0); err != nil {
		return err
	}
	if err := s.deleteCrossNamespaceRoute(ctx); err != nil {
		return err
	}
	if err := s.patchRoutingGateway(ctx, *original); err != nil {
		return err
	}
	if err := s.createHTTPRoute(ctx); err != nil {
		return err
	}
	if err := s.verifyExternalTraffic(ctx); err != nil {
		return err
	}
	return s.deleteRoutingNamespace(ctx)
}

func (s *openStackSuite) deleteCrossNamespaceRoute(ctx context.Context) error {
	if !s.routingNamespaceCreated {
		return nil
	}
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: s.routingNamespaceName(), Name: routeName}}
	if err := s.deleteMarkedAndWait(ctx, route, &s.crossRouteUID); err != nil {
		return err
	}
	return nil
}

func (s *openStackSuite) deleteRoutingNamespace(ctx context.Context) error {
	if !s.routingNamespaceCreated {
		return nil
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.routingNamespaceName()}}
	if err := s.deleteMarkedAndWait(ctx, namespace, &s.routingNamespaceUID); err != nil {
		return err
	}
	s.routingNamespaceCreated = false
	s.routingNamespaceUID = ""
	s.crossRouteUID = ""
	return nil
}

// These requests check match alternatives and exact headers without assuming
// the not-yet-supported Core status code for an unmatched request.
func (s *openStackSuite) verifyRoutingPredicates(ctx context.Context) error {
	for _, check := range []struct {
		path, header string
		allowed      bool
	}{
		{path: "/healthz", allowed: true},
		{path: "/echo?msg=unexpected", header: "different-run", allowed: false},
	} {
		status, err := s.routingStatus(ctx, check.path, check.header)
		if err != nil {
			return err
		}
		if (status == http.StatusOK) != check.allowed {
			return fmt.Errorf("HTTP match alternative or exact header check failed")
		}
	}
	return nil
}

func (s *openStackSuite) verifyRoutingRejected(ctx context.Context) error {
	status, err := s.routingStatus(ctx, "/echo?msg=unexpected", s.config.RunID)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		return fmt.Errorf("request still forwards to the backend")
	}
	return nil
}

func (s *openStackSuite) routingStatus(ctx context.Context, path, header string) (int, error) {
	address, err := s.gatewayAddress(ctx)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(address, "80")+path, nil)
	if err != nil {
		return 0, err
	}
	request.Host = s.hostname
	request.Close = true
	if header != "" {
		request.Header.Set("X-E2E-Route", header)
	}
	response, err := s.httpClient.Do(request)
	if err != nil {
		return 0, err
	}
	return response.StatusCode, response.Body.Close()
}
