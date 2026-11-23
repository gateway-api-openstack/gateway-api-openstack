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

package runconfig

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
)

func resolvedBackendTrafficPolicy(backend Backend) corev1.ServiceExternalTrafficPolicy {
	if backend.MemberMode == configv1alpha1.MemberModeEndpoint {
		return backend.ExternalTrafficPolicy
	}
	return backendTrafficPolicy(backend.ExternalTrafficPolicy)
}

func validateBackendMode(memberMode configv1alpha1.MemberMode, policy corev1.ServiceExternalTrafficPolicy, selector map[string]string, projectMode ProjectMode, sharedNetwork bool) error {
	switch memberMode {
	case "", configv1alpha1.MemberModeNodePort:
		return validateBackendProfile(policy, selector, projectMode, sharedNetwork)
	case configv1alpha1.MemberModeEndpoint:
		if policy != "" || sharedNetwork {
			return fmt.Errorf("endpoint mode does not use externalTrafficPolicy or shared NodePort network tests")
		}
		// Endpoint mode selects Pod addresses. The selector only controls placement.
		return validateBackendProfile(corev1.ServiceExternalTrafficPolicyLocal, selector, projectMode, false)
	default:
		return fmt.Errorf("backend.memberMode must be NodePort or Endpoint")
	}
}

func (c Runtime) validateHTTPScenarios() error {
	if c.HTTPRouting || c.EndpointBackends || c.BackendMemberMode == configv1alpha1.MemberModeEndpoint {
		if c.ExpectedAPIServer == "" || c.ExpectedClusterUID == "" {
			return fmt.Errorf("HTTP routing and Endpoint checks require exact Kubernetes cluster identity")
		}
	}
	if c.EndpointBackends && c.BackendMemberMode != configv1alpha1.MemberModeEndpoint {
		return fmt.Errorf("scenarios.endpointBackends requires backend.memberMode Endpoint")
	}
	if c.BackendMemberMode == configv1alpha1.MemberModeEndpoint {
		if c.LocalLifecycle || c.MemberTransitions || len(c.Project.BackendSecurityGroupIDs) != 0 {
			return fmt.Errorf("endpoint mode cannot run NodePort lifecycle scenarios or validate worker security groups")
		}
	}
	return nil
}
