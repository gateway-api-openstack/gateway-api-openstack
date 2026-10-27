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
	"maps"
	"os"
	"path/filepath"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/test/e2e/internal/cloudauth"
	"github.com/gateway-api-openstack/gateway-api-openstack/test/e2e/internal/controllercontract"
)

func (s *openStackSuite) verifyRunScopedController(ctx context.Context, deployment *appsv1.Deployment) error {
	var namespace corev1.Namespace
	if err := s.client.Get(ctx, client.ObjectKey{Name: s.config.ControllerNamespace}, &namespace); err != nil {
		return fmt.Errorf("read run-scoped controller Namespace: %w", err)
	}
	if namespace.UID == "" || namespace.Annotations[runAnnotation] != s.config.RunID {
		return fmt.Errorf("controller Namespace lacks the exact run annotation and immutable identity")
	}

	if err := s.verifyRunScopedControllerClasses(ctx); err != nil {
		return err
	}
	container, found := findContainer(deployment.Spec.Template.Spec.Containers, s.config.ControllerContainer)
	if !found {
		return fmt.Errorf("configured controller container is absent")
	}
	configMap, err := s.runScopedControllerConfigMap(ctx, &container)
	if err != nil {
		return err
	}
	secret, err := s.runScopedControllerCloudsSecret(ctx, deployment, &container)
	if err != nil {
		return err
	}
	if err := validateControllerSourceVersions(deployment, configMap, secret); err != nil {
		return err
	}
	return cloudauth.VerifyProject(ctx, secret.Data["clouds.yaml"], s.config.Project.ExpectedProjectID, cloudauth.Options{
		CloudName:        s.config.Audit.Cloud,
		Region:           s.config.Audit.Region,
		Microversion:     s.config.Audit.Microversion,
		ResolveProjectID: s.projectIDResolver,
	})
}

func (s *openStackSuite) verifyRunScopedControllerClasses(ctx context.Context) error {
	if err := s.rejectControllerIdentityCollision(ctx); err != nil {
		return err
	}
	return s.verifyRunScopedClassConfig(ctx)
}

func (s *openStackSuite) verifyRunScopedClassConfig(ctx context.Context) error {
	var parameters configv1alpha1.OpenStackGatewayClassConfig
	if err := s.client.Get(ctx, client.ObjectKey{Name: s.config.GatewayClassName}, &parameters); err != nil {
		return fmt.Errorf("read run-scoped class configuration: %w", err)
	}
	wanted := s.classConfig
	if wanted == nil {
		wanted = controllercontract.TopologySettings{
			VIPSubnetID:              s.config.Project.ExpectedVIPSubnetID,
			MemberSubnetID:           s.config.Project.ExpectedMemberSubnetID,
			ExternalNetworkID:        s.config.Project.ExpectedExternalNetworkID,
			FrontendSecurityGroupIDs: s.config.Project.FrontendSecurityGroupIDs,
			BackendSecurityGroupIDs:  s.config.Project.BackendSecurityGroupIDs,
		}.ClassConfig(s.config.GatewayClassName, s.config.RunID)
	}
	if err := s.validateClassParameters(&parameters, wanted); err != nil {
		return err
	}
	if s.classConfig == nil {
		s.classConfig = parameters.DeepCopy()
	}
	if s.secondClassConfig != nil {
		var second configv1alpha1.OpenStackGatewayClassConfig
		if err := s.client.Get(ctx, client.ObjectKeyFromObject(s.secondClassConfig), &second); err != nil {
			return err
		}
		return s.validateClassParameters(&second, s.secondClassConfig)
	}
	return nil
}

func (s *openStackSuite) validateClassParameters(parameters, wanted *configv1alpha1.OpenStackGatewayClassConfig) error {
	if parameters.UID == "" || (wanted.UID != "" && parameters.UID != wanted.UID) || parameters.Annotations[runAnnotation] != s.config.RunID || !parameters.DeletionTimestamp.IsZero() || !equality.Semantic.DeepEqual(parameters.Spec, wanted.Spec) {
		return fmt.Errorf("class configuration does not match the approved run identity and topology")
	}
	return nil
}

func (s *openStackSuite) rejectControllerIdentityCollision(ctx context.Context) error {
	var classes gatewayv1.GatewayClassList
	if err := s.client.List(ctx, &classes); err != nil {
		return fmt.Errorf("list GatewayClasses before E2E execution: %w", err)
	}
	foundPrimary, foundSecond := !s.createdClass, s.secondClass == nil
	for index := range classes.Items {
		class := &classes.Items[index]
		known := s.createdClass && class.Name == s.gatewayClassName || s.secondClass != nil && class.Name == s.secondClass.Name
		if !known && string(class.Spec.ControllerName) != s.config.ControllerName {
			continue
		}
		if class.Spec.ControllerName != gatewayv1.GatewayController(s.config.ControllerName) || !class.DeletionTimestamp.IsZero() {
			return fmt.Errorf("test GatewayClass changed controller or is being deleted")
		}
		if s.createdClass && class.Name == s.gatewayClassName && class.UID == s.gatewayClassUID &&
			class.Annotations[runAnnotation] == s.config.RunID {
			ref := class.Spec.ParametersRef
			parametersName := s.classParametersName
			if parametersName == "" {
				parametersName = s.config.GatewayClassName
			}
			if ref == nil || string(ref.Group) != configv1alpha1.GroupName || string(ref.Kind) != configv1alpha1.GatewayClassConfigKind || ref.Name != parametersName || ref.Namespace != nil {
				return fmt.Errorf("the run-scoped GatewayClass no longer references its approved class configuration")
			}
			foundPrimary = true
			continue
		}
		if s.secondClass != nil && s.secondClass.UID != "" && class.UID == s.secondClass.UID && class.Name == s.secondClass.Name && class.Annotations[runAnnotation] == s.config.RunID && equality.Semantic.DeepEqual(class.Spec, s.secondClass.Spec) {
			foundSecond = true
			continue
		}
		return fmt.Errorf("the run-scoped controller identity is already selected by a GatewayClass")
	}
	if !foundPrimary || !foundSecond {
		return fmt.Errorf("a run-scoped GatewayClass disappeared")
	}
	return nil
}

func (s *openStackSuite) runScopedControllerConfigMap(ctx context.Context, container *corev1.Container) (*corev1.ConfigMap, error) {
	name, err := controllerConfigMapName(container, s.config.Audit.Microversion)
	if err != nil {
		return nil, err
	}

	var configMap corev1.ConfigMap
	key := client.ObjectKey{Namespace: s.config.ControllerNamespace, Name: name}
	if err := s.client.Get(ctx, key, &configMap); err != nil {
		return nil, fmt.Errorf("read run-scoped controller ConfigMap: %w", err)
	}
	if err := validateRunScopedControllerConfigMap(&configMap, s.config); err != nil {
		return nil, err
	}
	return &configMap, nil
}

func controllerConfigMapName(container *corev1.Container, microversion string) (string, error) {
	if len(container.Command) != 0 {
		return "", fmt.Errorf("run-scoped controller must use the image entrypoint directly")
	}
	name, err := controllerConfigMapEnvironmentName(container.EnvFrom)
	if err != nil {
		return "", err
	}
	if err := validateControllerOverrides(container, microversion); err != nil {
		return "", err
	}
	return name, nil
}

func controllerConfigMapEnvironmentName(sources []corev1.EnvFromSource) (string, error) {
	if len(sources) != 1 || sources[0].Prefix != "" || sources[0].ConfigMapRef == nil ||
		sources[0].SecretRef != nil || sources[0].ConfigMapRef.Optional != nil && *sources[0].ConfigMapRef.Optional {
		return "", fmt.Errorf("run-scoped controller must use one unprefixed ConfigMap environment source")
	}
	return sources[0].ConfigMapRef.Name, nil
}

func validateControllerOverrides(container *corev1.Container, microversion string) error {
	for _, variable := range container.Env {
		if controllercontract.IsProtectedEnvironment(variable.Name) {
			return fmt.Errorf("run-scoped controller has a direct environment override for a protected setting")
		}
	}
	if err := controllercontract.ValidateArguments(container.Args, microversion); err != nil {
		return err
	}
	return nil
}

func validateRunScopedControllerConfigMap(configMap *corev1.ConfigMap, config e2eConfig) error {
	if configMap.UID == "" || configMap.Annotations[runAnnotation] != config.RunID || configMap.Immutable == nil || !*configMap.Immutable {
		return fmt.Errorf("controller ConfigMap must be immutable and carry the exact run annotation")
	}
	want := controllercontract.Settings{
		ControllerName: config.ControllerName,
		ClusterID:      config.Audit.ClusterID,
		Cloud:          config.Audit.Cloud,
		Region:         config.Audit.Region,
	}.Environment()
	if !maps.Equal(configMap.Data, want) {
		return fmt.Errorf("controller ConfigMap does not match the approved E2E settings")
	}
	return nil
}

func validateControllerSourceVersions(deployment *appsv1.Deployment, configMap *corev1.ConfigMap, secret *corev1.Secret) error {
	if deployment == nil || configMap == nil || secret == nil {
		return fmt.Errorf("controller configuration sources lack immutable API identity")
	}
	wantConfig, err := controllercontract.SourceVersion(configMap.UID, configMap.ResourceVersion)
	if err != nil {
		return err
	}
	wantSecret, err := controllercontract.SourceVersion(secret.UID, secret.ResourceVersion)
	if err != nil {
		return err
	}
	if deployment.Spec.Template.Annotations[controllercontract.ConfigSourceAnnotation] != wantConfig ||
		deployment.Spec.Template.Annotations[controllercontract.CloudsSourceAnnotation] != wantSecret {
		return fmt.Errorf("controller Pod template is not bound to the validated ConfigMap and Secret versions")
	}
	return nil
}

func (s *openStackSuite) runScopedControllerCloudsSecret(
	ctx context.Context,
	deployment *appsv1.Deployment,
	container *corev1.Container,
) (*corev1.Secret, error) {
	name, err := controllerCloudsSecretName(deployment, container, s.config.Project.ControllerCloudsSecret)
	if err != nil {
		return nil, err
	}

	var secret corev1.Secret
	key := client.ObjectKey{Namespace: s.config.ControllerNamespace, Name: name}
	if err := s.client.Get(ctx, key, &secret); err != nil {
		return nil, fmt.Errorf("read run-scoped controller Secret: %w", err)
	}
	if err := validateRunScopedControllerSecret(&secret, s.config.RunID); err != nil {
		return nil, err
	}
	return &secret, nil
}

func controllerCloudsSecretName(
	deployment *appsv1.Deployment,
	container *corev1.Container,
	expectedName string,
) (string, error) {
	volumeName, err := controllerCloudsVolumeName(container)
	if err != nil {
		return "", err
	}
	source := controllerCloudsVolumeSource(deployment, volumeName)
	if source == nil || source.SecretName != expectedName ||
		source.Optional != nil && *source.Optional || !mountsOnlyCloudsYAML(source) {
		return "", fmt.Errorf("controller Deployment does not mount the approved clouds.yaml Secret")
	}
	return source.SecretName, nil
}

func controllerCloudsVolumeName(container *corev1.Container) (string, error) {
	volumeName := ""
	mounts := 0
	for _, mount := range container.VolumeMounts {
		if mount.MountPath != controllercontract.CloudsMountPath {
			continue
		}
		mounts++
		if !mount.ReadOnly || mount.SubPath != "" || mount.SubPathExpr != "" {
			return "", fmt.Errorf("controller clouds.yaml mount is not an exact read-only directory mount")
		}
		volumeName = mount.Name
	}
	if mounts != 1 {
		return "", fmt.Errorf("controller Deployment must mount one clouds.yaml directory")
	}
	return volumeName, nil
}

func controllerCloudsVolumeSource(deployment *appsv1.Deployment, volumeName string) *corev1.SecretVolumeSource {
	for index := range deployment.Spec.Template.Spec.Volumes {
		volume := &deployment.Spec.Template.Spec.Volumes[index]
		if volume.Name == volumeName {
			return volume.Secret
		}
	}
	return nil
}

func validateRunScopedControllerSecret(secret *corev1.Secret, runID string) error {
	if secret.UID == "" || secret.Annotations[runAnnotation] != runID || secret.Immutable == nil || !*secret.Immutable {
		return fmt.Errorf("controller Secret must be immutable and carry the exact run annotation")
	}
	if len(secret.Data) != 1 || len(secret.Data["clouds.yaml"]) == 0 {
		return fmt.Errorf("controller Secret must contain only clouds.yaml")
	}
	return nil
}

func mountsOnlyCloudsYAML(source *corev1.SecretVolumeSource) bool {
	return source != nil && len(source.Items) == 1 && source.Items[0].Key == "clouds.yaml" &&
		filepath.Clean(source.Items[0].Path) == "clouds.yaml"
}

func (s *openStackSuite) verifyAuthenticatedAuditProject(ctx context.Context) error {
	return s.withAuthenticatedAuditCloudsYAML(ctx, func(string) error { return nil })
}

func (s *openStackSuite) withAuthenticatedAuditCloudsYAML(ctx context.Context, use func(path string) error) error {
	contents, err := os.ReadFile(s.config.Audit.CloudsYAML)
	if err != nil {
		return fmt.Errorf("read audit clouds.yaml: %w", err)
	}
	return cloudauth.VerifyProjectWithExactCopy(
		ctx,
		contents,
		s.config.Project.ExpectedProjectID,
		cloudauth.Options{
			CloudName:        s.config.Audit.Cloud,
			Region:           s.config.Audit.Region,
			Microversion:     s.config.Audit.Microversion,
			ResolveProjectID: s.projectIDResolver,
		},
		use,
	)
}
