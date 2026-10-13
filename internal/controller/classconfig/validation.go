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

package classconfig

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Validate applies the same structural rules as the CRD and supplies defaults
// for callers that read unstructured fixtures without API-server defaulting.
func Validate(spec configv1alpha1.OpenStackGatewayClassConfigSpec) (cloud.TopologyRequest, corev1.NodeAddressType, cloud.HealthMonitorConfig, error) {
	request := cloud.TopologyRequest{FlavorID: spec.FlavorID, AvailabilityZone: spec.AvailabilityZone}
	var err error
	request.VIPSubnet, err = reference(spec.VIPSubnet)
	if err != nil {
		return request, "", cloud.HealthMonitorConfig{}, invalid("vipSubnet must select one resource by UUID, exact name, or tags")
	}
	request.MemberSubnet, err = reference(spec.MemberSubnet)
	if err != nil {
		return request, "", cloud.HealthMonitorConfig{}, invalid("memberSubnet must select one resource by UUID, exact name, or tags")
	}
	mode := spec.FloatingIP.Mode
	if mode == "" {
		mode = configv1alpha1.FloatingIPNone
	}
	switch mode {
	case configv1alpha1.FloatingIPNone:
		if spec.FloatingIP.ExternalNetwork != nil {
			return request, "", cloud.HealthMonitorConfig{}, invalid("floatingIP.externalNetwork is only allowed with Allocate mode")
		}
	case configv1alpha1.FloatingIPAllocate:
		if spec.FloatingIP.ExternalNetwork == nil {
			return request, "", cloud.HealthMonitorConfig{}, invalid("floatingIP.externalNetwork is required with Allocate mode")
		}
		network, err := reference(*spec.FloatingIP.ExternalNetwork)
		if err != nil {
			return request, "", cloud.HealthMonitorConfig{}, invalid("floatingIP.externalNetwork must select one resource by UUID, exact name, or tags")
		}
		request.ExternalNetwork = &network
	default:
		return request, "", cloud.HealthMonitorConfig{}, invalid("floatingIP.mode must be None or Allocate")
	}
	if spec.FlavorID != "" && !uuidPattern.MatchString(spec.FlavorID) {
		return request, "", cloud.HealthMonitorConfig{}, invalid("flavorID must be a UUID")
	}
	if spec.AvailabilityZone != "" && !validName(spec.AvailabilityZone) {
		return request, "", cloud.HealthMonitorConfig{}, invalid("availabilityZone must be a nonempty exact name of at most 255 characters")
	}
	addressType := corev1.NodeAddressType(spec.NodeAddressType)
	if addressType == "" {
		addressType = corev1.NodeInternalIP
	}
	if addressType != corev1.NodeInternalIP && addressType != corev1.NodeExternalIP {
		return request, "", cloud.HealthMonitorConfig{}, invalid("nodeAddressType must be InternalIP or ExternalIP")
	}
	monitor := cloud.HealthMonitorConfig{
		Path: spec.HealthMonitor.Path, Delay: intValue(spec.HealthMonitor.Delay, 10),
		Timeout: intValue(spec.HealthMonitor.Timeout, 5), MaxRetries: intValue(spec.HealthMonitor.MaxRetries, 3),
		MaxRetriesDown: intValue(spec.HealthMonitor.MaxRetriesDown, 3),
	}
	if monitor.Path == "" {
		monitor.Path = "/"
	}
	if !strings.HasPrefix(monitor.Path, "/") || utf8.RuneCountInString(monitor.Path) > 255 || strings.ContainsFunc(monitor.Path, unicode.IsControl) {
		return request, "", monitor, invalid("healthMonitor.path must start with /, contain no control characters, and be at most 255 characters")
	}
	if monitor.Delay < 1 || monitor.Delay > 3600 || monitor.Timeout < 1 || monitor.Timeout > 3600 || monitor.Timeout >= monitor.Delay {
		return request, "", monitor, invalid("healthMonitor timeout must be less than delay, with both between 1 and 3600 seconds")
	}
	if monitor.MaxRetries < 1 || monitor.MaxRetries > 10 || monitor.MaxRetriesDown < 1 || monitor.MaxRetriesDown > 10 {
		return request, "", monitor, invalid("healthMonitor retry counts must be between 1 and 10")
	}
	return request, addressType, monitor, nil
}

func reference(ref configv1alpha1.ResourceReference) (cloud.ResourceReference, error) {
	count := 0
	if ref.ID != "" {
		count++
	}
	if ref.Name != "" {
		count++
	}
	if len(ref.Tags) != 0 {
		count++
	}
	if count != 1 || (ref.ID != "" && !uuidPattern.MatchString(ref.ID)) || (ref.Name != "" && !validName(ref.Name)) || len(ref.Tags) > 16 {
		return cloud.ResourceReference{}, invalid("invalid resource reference")
	}
	seen := make(map[string]struct{}, len(ref.Tags))
	for _, tag := range ref.Tags {
		if len(tag) == 0 || utf8.RuneCountInString(tag) > 60 || strings.TrimSpace(tag) != tag || strings.ContainsAny(tag, ",") || strings.ContainsFunc(tag, unicode.IsControl) {
			return cloud.ResourceReference{}, invalid("invalid resource tag")
		}
		if _, exists := seen[tag]; exists {
			return cloud.ResourceReference{}, invalid("duplicate resource tag")
		}
		seen[tag] = struct{}{}
	}
	return cloud.ResourceReference{ID: ref.ID, Name: ref.Name, Tags: append([]string(nil), ref.Tags...)}, nil
}

func validName(value string) bool {
	return len(value) > 0 && utf8.RuneCountInString(value) <= 255 && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

func intValue(value *int32, fallback int) int {
	if value == nil {
		return fallback
	}
	return int(*value)
}
