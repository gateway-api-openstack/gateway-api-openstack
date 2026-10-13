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

package controller

import (
	"strconv"

	corev1 "k8s.io/api/core/v1"
)

const (
	labelNodeRoleControlPlane = "node-role.kubernetes.io/control-plane"
	labelNodeRoleMaster       = "node-role.kubernetes.io/master"
)

// NodePortNodeExcluded reports whether Node labels exclude it from NodePort
// membership. Workers need no role label. Readiness and addresses are checked
// separately when constructing members.
func NodePortNodeExcluded(node *corev1.Node) bool {
	if _, controlPlane := node.Labels[labelNodeRoleControlPlane]; controlPlane {
		return true
	}
	if _, master := node.Labels[labelNodeRoleMaster]; master {
		return true
	}
	value, present := node.Labels[corev1.LabelNodeExcludeBalancers]
	if !present {
		return false
	}
	// Match the Kubernetes Service controller: only an explicit false value
	// includes a labelled Node. Empty and invalid values exclude it.
	excluded, err := strconv.ParseBool(value)
	return err != nil || excluded
}
