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

package model

import (
	"cmp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func targetsGateway(parent gatewayv1.ParentReference, namespace string, gateway *gatewayv1.Gateway) bool {
	if parent.Group != nil && *parent.Group != gatewayv1.GroupName || parent.Kind != nil && *parent.Kind != "Gateway" {
		return false
	}
	if parent.Namespace != nil {
		namespace = string(*parent.Namespace)
	}
	return namespace == gateway.Namespace && string(parent.Name) == gateway.Name
}

func attachParent(input Input, route *gatewayv1.HTTPRoute, parentIndex int, listeners []Listener) ParentAttachment {
	parent := route.Spec.ParentRefs[parentIndex]
	result := ParentAttachment{Index: parentIndex, Reference: *parent.DeepCopy(), State: AttachmentRejected}
	for i := range input.Gateway.Spec.Listeners {
		listener := &input.Gateway.Spec.Listeners[i]
		if parent.SectionName != nil && *parent.SectionName != listener.Name || parent.Port != nil && *parent.Port != listener.Port {
			continue
		}
		attachment := ListenerAttachment{Name: listener.Name, State: AttachmentRejected}
		if !acceptsHTTP(listeners[i]) {
			attachment.Diagnostics = append(attachment.Diagnostics, routeDiagnostic(gatewayv1.RouteReasonNotAllowedByListeners, "listener does not accept HTTPRoute configuration"))
		} else if !allowsRoute(listener, input.Gateway.Namespace, route.Namespace, input.Namespaces) {
			attachment.Diagnostics = append(attachment.Diagnostics, routeDiagnostic(gatewayv1.RouteReasonNotAllowedByListeners, "listener does not allow this HTTPRoute kind or namespace"))
		} else {
			attachment.Hostnames = intersectHostnames(listener.Hostname, route.Spec.Hostnames)
			if len(attachment.Hostnames) == 0 {
				attachment.Diagnostics = append(attachment.Diagnostics, routeDiagnostic(gatewayv1.RouteReasonNoMatchingListenerHostname, "route and listener hostnames do not intersect"))
			} else {
				attachment.State = AttachmentAccepted
				result.State = AttachmentAccepted
			}
		}
		result.Listeners = append(result.Listeners, attachment)
	}
	slices.SortFunc(result.Listeners, func(a, b ListenerAttachment) int { return cmp.Compare(a.Name, b.Name) })
	if len(result.Listeners) == 0 {
		result.Diagnostics = append(result.Diagnostics, routeDiagnostic(gatewayv1.RouteReasonNoMatchingParent, "parent reference does not select a listener"))
	} else if result.State == AttachmentRejected {
		// A listener that allowed the kind and namespace but had no intersecting
		// hostname determines this outcome, regardless of listener order.
		reason := gatewayv1.RouteReasonNotAllowedByListeners
		message := "no selected listener allows this HTTPRoute"
		for _, listener := range result.Listeners {
			for _, diagnostic := range listener.Diagnostics {
				if diagnostic.Reason == string(gatewayv1.RouteReasonNoMatchingListenerHostname) {
					reason = gatewayv1.RouteReasonNoMatchingListenerHostname
					message = "route and allowed listener hostnames do not intersect"
				}
			}
		}
		result.Diagnostics = append(result.Diagnostics, routeDiagnostic(reason, message))
	}
	return result
}

func acceptsHTTP(listener Listener) bool {
	for _, diagnostic := range listener.Diagnostics {
		if diagnostic.Condition == string(gatewayv1.ListenerConditionAccepted) || diagnostic.Condition == string(gatewayv1.ListenerConditionConflicted) {
			return false
		}
	}
	return true
}

func allowsRoute(listener *gatewayv1.Listener, gatewayNamespace, routeNamespace string, namespaces []corev1.Namespace) bool {
	allowed := listener.AllowedRoutes
	if allowed == nil {
		return gatewayNamespace == routeNamespace
	}
	if len(allowed.Kinds) != 0 {
		found := false
		for _, kind := range allowed.Kinds {
			if kind.Kind == "HTTPRoute" && (kind.Group == nil || *kind.Group == gatewayv1.GroupName) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if allowed.Namespaces == nil || allowed.Namespaces.From == nil {
		return gatewayNamespace == routeNamespace
	}
	switch *allowed.Namespaces.From {
	case gatewayv1.NamespacesFromSame:
		return gatewayNamespace == routeNamespace
	case gatewayv1.NamespacesFromAll:
		return true
	case gatewayv1.NamespacesFromSelector:
		if allowed.Namespaces.Selector == nil {
			return false
		}
		selector, err := metav1.LabelSelectorAsSelector(allowed.Namespaces.Selector)
		if err != nil {
			return false
		}
		for i := range namespaces {
			if namespaces[i].Name == routeNamespace {
				return selector.Matches(labels.Set(namespaces[i].Labels))
			}
		}
	}
	return false
}

// intersectHostnames preserves the effective hostname constraints for the
// pinned HTTP profile, including nested wildcard suffixes and apex exclusion.
func intersectHostnames(listener *gatewayv1.Hostname, routes []gatewayv1.Hostname) []string {
	listenerName := ""
	if listener != nil {
		listenerName = string(*listener)
	}
	if len(routes) == 0 {
		return []string{listenerName}
	}
	result := make([]string, 0, len(routes))
	for _, route := range routes {
		hostname := string(route)
		switch {
		case listenerName == "", listenerName == hostname:
			result = append(result, hostname)
		case wildcardMatches(listenerName, hostname):
			result = append(result, hostname)
		case wildcardMatches(hostname, listenerName):
			result = append(result, listenerName)
		}
	}
	slices.Sort(result)
	return slices.Compact(result)
}

func wildcardMatches(pattern, hostname string) bool {
	if !strings.HasPrefix(pattern, "*.") {
		return false
	}
	suffix := pattern[1:]
	return len(hostname) > len(suffix) && strings.HasSuffix(hostname, suffix)
}

func routeDiagnostic(reason gatewayv1.RouteConditionReason, message string) Diagnostic {
	return Diagnostic{Condition: string(gatewayv1.RouteConditionAccepted), Reason: string(reason), Message: message}
}
