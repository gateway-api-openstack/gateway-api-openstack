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

package v1alpha1

import "k8s.io/apimachinery/pkg/runtime"

// DeepCopyInto copies the object and every mutable field into out.
func (in *OpenStackGatewayClassConfig) DeepCopyInto(out *OpenStackGatewayClassConfig) {
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
}

// DeepCopy returns an independent copy of the object.
func (in *OpenStackGatewayClassConfig) DeepCopy() *OpenStackGatewayClassConfig {
	if in == nil {
		return nil
	}
	out := new(OpenStackGatewayClassConfig)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *OpenStackGatewayClassConfig) DeepCopyObject() runtime.Object {
	if copy := in.DeepCopy(); copy != nil {
		return copy
	}
	return nil
}

// DeepCopyInto copies the specification and all selectors and optional values.
func (in *OpenStackGatewayClassConfigSpec) DeepCopyInto(out *OpenStackGatewayClassConfigSpec) {
	*out = *in
	out.VIPSubnet.Tags = append([]string(nil), in.VIPSubnet.Tags...)
	out.MemberSubnet.Tags = append([]string(nil), in.MemberSubnet.Tags...)
	if in.FloatingIP.ExternalNetwork != nil {
		out.FloatingIP.ExternalNetwork = new(ResourceReference)
		*out.FloatingIP.ExternalNetwork = *in.FloatingIP.ExternalNetwork
		out.FloatingIP.ExternalNetwork.Tags = append([]string(nil), in.FloatingIP.ExternalNetwork.Tags...)
	}
	copyInt := func(p *int32) *int32 {
		if p == nil {
			return nil
		}
		v := *p
		return &v
	}
	out.HealthMonitor.Delay = copyInt(in.HealthMonitor.Delay)
	out.HealthMonitor.Timeout = copyInt(in.HealthMonitor.Timeout)
	out.HealthMonitor.MaxRetries = copyInt(in.HealthMonitor.MaxRetries)
	out.HealthMonitor.MaxRetriesDown = copyInt(in.HealthMonitor.MaxRetriesDown)
}

// DeepCopyInto copies a list and all of its objects.
func (in *OpenStackGatewayClassConfigList) DeepCopyInto(out *OpenStackGatewayClassConfigList) {
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]OpenStackGatewayClassConfig, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy returns an independent copy of a list.
func (in *OpenStackGatewayClassConfigList) DeepCopy() *OpenStackGatewayClassConfigList {
	if in == nil {
		return nil
	}
	out := new(OpenStackGatewayClassConfigList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *OpenStackGatewayClassConfigList) DeepCopyObject() runtime.Object {
	if copy := in.DeepCopy(); copy != nil {
		return copy
	}
	return nil
}
