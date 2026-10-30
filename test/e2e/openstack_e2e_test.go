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
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	configv1alpha1 "github.com/gateway-api-openstack/gateway-api-openstack/api/v1alpha1"
	"github.com/gateway-api-openstack/gateway-api-openstack/internal/cloud/openstack"
	"github.com/gateway-api-openstack/gateway-api-openstack/test/e2e/internal/cloudauth"
	"github.com/gateway-api-openstack/gateway-api-openstack/test/e2e/internal/controllercontract"
	"github.com/gateway-api-openstack/gateway-api-openstack/test/e2e/internal/runconfig"
)

const (
	backendName           = "backend"
	gatewayName           = "edge"
	routeName             = "basic"
	listenerName          = "http"
	backendPort           = 8080
	listenerPort          = 80
	emergencyCleanupLimit = 15 * time.Minute
)

type openStackSuite struct {
	config         e2eConfig
	client         client.Client
	coreRESTClient rest.Interface
	report         *e2eReport

	httpClient             *http.Client
	projectIDResolver      cloudauth.ProjectIDResolver
	gatewayClassName       string
	hostname               string
	createdNamespace       bool
	createdClass           bool
	createdGateway         bool
	createdRoute           bool
	controllerScaled       bool
	baselineAudit          *auditSummary
	activeAudit            *auditSummary
	activeAuditFingerprint [32]byte
	classConfig            *configv1alpha1.OpenStackGatewayClassConfig
	secondClassConfig      *configv1alpha1.OpenStackGatewayClassConfig
	secondClass            *gatewayv1.GatewayClass
	classParametersName    string
	frozenGateway          *gatewayv1.Gateway
	cloudClients           *openstack.ServiceClients

	controllerDeploymentUID types.UID
	backendDeploymentUID    types.UID
	backendServiceUID       types.UID
	namespaceUID            types.UID
	gatewayClassUID         types.UID
	gatewayUID              types.UID
	routeUID                types.UID
}

type leaseState struct {
	holder      string
	transitions int32
}

func TestOpenStackE2E(t *testing.T) {
	config, enabled, err := loadE2EConfig(os.Getenv)
	if err != nil {
		t.Fatalf("OpenStack E2E configuration was rejected: %v", err)
	}
	if !enabled {
		t.Skip("run the suite through make test-e2e with an explicit E2E_CONFIG")
	}

	startedAt := time.Now().UTC()
	report := newE2EReport(startedAt, config)

	ctx, cancel := context.WithTimeout(context.Background(), config.Timeout)
	defer cancel()

	suite := requireOpenStackSuite(t, config, report)
	runErr := runOpenStackWithCleanup(ctx, suite, report)
	runErr = verifyOpenStackPostCleanupAudit(suite, report, runErr)
	runErr = writeOpenStackArtifacts(config.ArtifactDirectory, report, runErr)
	if runErr != nil {
		t.Fatalf("OpenStack E2E did not complete: %v", runErr)
	}
}

func requireOpenStackSuite(t *testing.T, config e2eConfig, report *e2eReport) *openStackSuite {
	t.Helper()
	suite, err := newOpenStackSuite(config, report)
	if err == nil {
		return suite
	}
	failure := report.failCheck(checkPreflight, err)
	writeReportAfterSetupFailure(t, config.ArtifactDirectory, report)
	t.Fatal(failure)
	return nil
}

func runOpenStackWithCleanup(ctx context.Context, suite *openStackSuite, report *e2eReport) error {
	runErr := suite.run(ctx)
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), emergencyCleanupLimit)
	var cleanupErr error
	if runErr != nil {
		cleanupErr = suite.observeNetworkStage(networkEmergencyCleanup, func() error { return suite.ensureCleanup(cleanupCtx) })
	} else {
		cleanupErr = suite.ensureCleanup(cleanupCtx)
	}
	cleanupCancel()
	if cleanupErr == nil {
		return runErr
	}
	return errors.Join(runErr, report.failCheck(checkCleanup, cleanupErr))
}

func verifyOpenStackPostCleanupAudit(suite *openStackSuite, report *e2eReport, runErr error) error {
	if suite.baselineAudit == nil {
		return runErr
	}
	auditCtx, auditCancel := context.WithTimeout(context.Background(), emergencyCleanupLimit)
	err := suite.verifyPostCleanupAudit(auditCtx)
	auditCancel()
	if err == nil {
		return runErr
	}
	return errors.Join(runErr, report.failCheck(checkCleanupAudit, err))
}

func writeOpenStackArtifacts(directory string, report *e2eReport, runErr error) error {
	if err := writeE2EArtifacts(directory, report); err != nil {
		return errors.Join(runErr, &checkFailure{name: "write E2E artifacts", cause: err})
	}
	return runErr
}

func newOpenStackSuite(config e2eConfig, report *e2eReport) (*openStackSuite, error) {
	restConfig, err := runconfig.KubernetesRESTConfig(config)
	if err != nil {
		return nil, fmt.Errorf("load explicit kubeconfig and context: %w", err)
	}
	restConfig.Timeout = config.HTTPTimeout

	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		coordinationv1.AddToScheme,
		discoveryv1.AddToScheme,
		apiextensionsv1.AddToScheme,
		configv1alpha1.AddToScheme,
		gatewayv1.Install,
	} {
		if err := install(scheme); err != nil {
			return nil, fmt.Errorf("install Kubernetes scheme: %w", err)
		}
	}
	kubeClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes pod proxy client: %w", err)
	}

	return &openStackSuite{
		config:         config,
		client:         kubeClient,
		coreRESTClient: clientset.CoreV1().RESTClient(),
		report:         report,
		httpClient: &http.Client{
			Timeout: config.HTTPTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		gatewayClassName: config.GatewayClassName,
		hostname:         "e2e-" + config.RunID + ".example.test",
	}, nil
}

func (s *openStackSuite) run(ctx context.Context) error {
	steps := s.checkRunners()
	for _, name := range s.report.requiredChecks {
		// This audit must also run after a failed check and emergency cleanup.
		if name == checkCleanupAudit {
			continue
		}
		if err := steps[name](ctx); err != nil {
			return s.report.failCheck(name, err)
		}
		mustSetCheck(s.report, name, statusPassed, checkSummaryPassed)
	}
	return nil
}

func (s *openStackSuite) checkRunners() map[string]func(context.Context) error {
	return map[string]func(context.Context) error{
		checkPreflight:          s.preflight,
		checkBackend:            s.createBackend,
		checkGatewayClass:       s.createGatewayClass,
		checkGateway:            s.createGateway,
		checkHTTPRoute:          s.createHTTPRoute,
		checkActiveAudit:        s.verifyActiveOwnershipInventoryAudit,
		checkTraffic:            s.verifyExternalTraffic,
		checkReferencedSecurity: s.verifyReferencedSecurity,
		checkFloatingIPRecovery: s.verifyFloatingIPRecovery,
		checkFloatingIPDeletion: s.verifyFloatingIPDeletion,
		checkFrozenClass:        s.verifyFrozenClassConfiguration,
		checkLeaderRecovery:     s.deleteLeaderAndVerifyRecovery,
		checkColdRestart:        s.coldRestartAndVerifyRecovery,
		checkMetrics:            s.verifyConvergedNoOp,
		checkFrozenRestart:      s.verifyFrozenGatewayAfterRestart,
		checkMemberTransitions:  s.verifyWorkerMembershipTransitions,
		checkSequentialClasses:  s.verifySequentialClassConfiguration,
		checkCleanup:            s.orderlyCleanup,
		checkCleanupAudit:       s.verifyPostCleanupAudit,
	}
}

func (s *openStackSuite) preflight(ctx context.Context) error {
	if err := s.verifyPreflightFiles(); err != nil {
		return err
	}
	if err := s.verifyPreflightKubernetesState(ctx); err != nil {
		return err
	}
	if slices.Contains(s.report.requiredChecks, checkMemberTransitions) {
		if err := s.validateMembershipWorkers(ctx); err != nil {
			return err
		}
	}
	return s.prepareBaselineAudit(ctx)
}

func (s *openStackSuite) verifyPreflightFiles() error {
	if _, err := os.Lstat(s.config.ArtifactDirectory); !os.IsNotExist(err) {
		if err == nil {
			return fmt.Errorf("artifact directory already exists")
		}
		return fmt.Errorf("inspect artifact directory: %w", err)
	}
	info, err := os.Stat(s.config.Kubeconfig)
	if err != nil {
		return fmt.Errorf("inspect kubeconfig: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("kubeconfig is not a regular file")
	}
	return nil
}

func (s *openStackSuite) verifyPreflightKubernetesState(ctx context.Context) error {
	if err := s.verifyExplicitContext(); err != nil {
		return err
	}
	if err := runconfig.RequireClusterIdentity(ctx, s.client, s.config); err != nil {
		return err
	}
	if err := s.verifyGatewayAPIBundle(ctx); err != nil {
		return err
	}
	if err := controllercontract.RequireClassConfigCRD(ctx, s.client); err != nil {
		return err
	}
	if err := s.requireAbsent(ctx, client.ObjectKey{Name: s.gatewayClassName}, &gatewayv1.GatewayClass{}); err != nil {
		return fmt.Errorf("verify unique GatewayClass: %w", err)
	}
	if err := s.requireAbsent(ctx, client.ObjectKey{Name: s.config.Namespace}, &corev1.Namespace{}); err != nil {
		return fmt.Errorf("verify unique Namespace: %w", err)
	}
	if err := s.verifyControllerDeployment(ctx); err != nil {
		return err
	}
	return nil
}

func (s *openStackSuite) prepareBaselineAudit(ctx context.Context) error {
	info, err := os.Stat(s.config.Audit.Binary)
	if err != nil {
		return fmt.Errorf("inspect audit binary: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("audit binary is not an executable regular file")
	}
	summary, err := s.runOwnershipAudit(ctx, true, nil)
	if err != nil {
		return err
	}
	s.baselineAudit = &summary
	s.report.Audit = &auditEvidence{Baseline: &summary}
	return nil
}

func (s *openStackSuite) verifyExplicitContext() error {
	_, err := runconfig.KubernetesRESTConfig(s.config)
	return err
}

func (s *openStackSuite) verifyGatewayAPIBundle(ctx context.Context) error {
	return controllercontract.RequireGatewayAPICRDs(ctx, s.client)
}

func (s *openStackSuite) verifyControllerDeployment(ctx context.Context) error {
	deployment, pods, err := s.readyControllerPods(ctx)
	if err != nil {
		return err
	}
	if err := s.validateControllerDeploymentIdentity(deployment); err != nil {
		return err
	}
	if err := s.verifyRunScopedController(ctx, deployment); err != nil {
		return err
	}
	if err := s.validateControllerRollout(deployment, pods); err != nil {
		return err
	}
	if err := s.verifyControllerPods(ctx, deployment, pods); err != nil {
		return err
	}
	return s.verifyControllerLease(ctx, pods)
}

func (s *openStackSuite) validateControllerRollout(deployment *appsv1.Deployment, pods []corev1.Pod) error {
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != s.config.ControllerReplicas {
		return fmt.Errorf("controller Deployment replica count does not match the explicit configuration")
	}
	if deployment.Generation != deployment.Status.ObservedGeneration ||
		deployment.Status.UpdatedReplicas != s.config.ControllerReplicas ||
		deployment.Status.ReadyReplicas != s.config.ControllerReplicas ||
		deployment.Status.AvailableReplicas != s.config.ControllerReplicas ||
		deployment.Status.UnavailableReplicas != 0 {
		return fmt.Errorf("controller Deployment is not fully rolled out")
	}
	if len(pods) != int(s.config.ControllerReplicas) {
		return fmt.Errorf("controller Deployment has an unexpected number of selected pods")
	}
	return nil
}

func (s *openStackSuite) verifyControllerPods(ctx context.Context, deployment *appsv1.Deployment, pods []corev1.Pod) error {
	for index := range pods {
		if err := s.verifyControllerPod(ctx, deployment, &pods[index]); err != nil {
			return err
		}
	}
	return nil
}

func (s *openStackSuite) verifyControllerLease(ctx context.Context, pods []corev1.Pod) error {
	lease, err := s.currentLease(ctx)
	if err != nil {
		return err
	}
	if _, err := matchLeaseHolder(lease.holder, pods); err != nil {
		return err
	}
	return nil
}

func (s *openStackSuite) validateControllerDeploymentIdentity(deployment *appsv1.Deployment) error {
	if deployment == nil || deployment.UID == "" {
		return fmt.Errorf("controller Deployment has no immutable UID")
	}
	if s.controllerDeploymentUID == "" {
		s.controllerDeploymentUID = deployment.UID
	} else if deployment.UID != s.controllerDeploymentUID {
		return fmt.Errorf("controller Deployment UID changed during the run")
	}
	if deployment.Annotations[runAnnotation] != s.config.RunID {
		return fmt.Errorf("controller Deployment lacks the exact run annotation")
	}
	container, found := findContainer(deployment.Spec.Template.Spec.Containers, s.config.ControllerContainer)
	if !found {
		return fmt.Errorf("configured controller container is absent")
	}
	if !strings.HasSuffix(container.Image, "@"+s.config.ControllerImageDigest) {
		return fmt.Errorf("controller Deployment image is not pinned to the expected digest")
	}
	if err := controllercontract.ValidateMetricsPorts(container.Ports); err != nil {
		return err
	}
	if err := controllercontract.ValidateArguments(container.Args, s.config.Audit.Microversion); err != nil {
		return err
	}
	return nil
}

func (s *openStackSuite) readyControllerPods(ctx context.Context) (*appsv1.Deployment, []corev1.Pod, error) {
	var deployment appsv1.Deployment
	key := client.ObjectKey{Namespace: s.config.ControllerNamespace, Name: s.config.ControllerDeployment}
	if err := s.client.Get(ctx, key, &deployment); err != nil {
		return nil, nil, fmt.Errorf("read controller Deployment: %w", err)
	}
	pods, err := s.controllerDeploymentPods(ctx, &deployment)
	if err != nil {
		return nil, nil, err
	}
	return &deployment, pods.Items, nil
}

func (s *openStackSuite) verifyControllerPod(ctx context.Context, deployment *appsv1.Deployment, pod *corev1.Pod) error {
	if !podReady(pod) {
		return fmt.Errorf("controller pod is not Ready")
	}
	replicaSetOwner, err := controllerReplicaSetOwner(pod)
	if err != nil {
		return err
	}
	var replicaSet appsv1.ReplicaSet
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: replicaSetOwner.Name}, &replicaSet); err != nil {
		return fmt.Errorf("read controller ReplicaSet owner: %w", err)
	}
	if err := verifyReplicaSetOwnerUID(replicaSetOwner, &replicaSet); err != nil {
		return err
	}
	if err := verifyReplicaSetDeploymentOwner(&replicaSet, deployment); err != nil {
		return err
	}
	return s.verifyControllerPodImage(pod)
}

func controllerReplicaSetOwner(pod *corev1.Pod) (*metav1.OwnerReference, error) {
	replicaSetOwner := metav1.GetControllerOf(pod)
	if replicaSetOwner == nil || replicaSetOwner.Kind != "ReplicaSet" || replicaSetOwner.APIVersion != appsv1.SchemeGroupVersion.String() {
		return nil, fmt.Errorf("controller pod does not have the expected ReplicaSet owner")
	}
	return replicaSetOwner, nil
}

func verifyReplicaSetOwnerUID(owner *metav1.OwnerReference, replicaSet *appsv1.ReplicaSet) error {
	if owner.UID == "" || replicaSet.UID != owner.UID {
		return fmt.Errorf("controller pod ReplicaSet owner UID does not match")
	}
	return nil
}

func verifyReplicaSetDeploymentOwner(replicaSet *appsv1.ReplicaSet, deployment *appsv1.Deployment) error {
	deploymentOwner := metav1.GetControllerOf(replicaSet)
	if deploymentOwner == nil || deploymentOwner.APIVersion != appsv1.SchemeGroupVersion.String() ||
		deploymentOwner.Kind != "Deployment" || deploymentOwner.Name != deployment.Name || deploymentOwner.UID != deployment.UID {
		return fmt.Errorf("controller pod is outside the configured Deployment")
	}
	return nil
}

func (s *openStackSuite) verifyControllerPodImage(pod *corev1.Pod) error {
	status, found := findContainerStatus(pod.Status.ContainerStatuses, s.config.ControllerContainer)
	if !found || !status.Ready ||
		(status.ImageID != s.config.ControllerImageDigest && !strings.HasSuffix(status.ImageID, "@"+s.config.ControllerImageDigest)) {
		return fmt.Errorf("controller pod is not running the expected immutable image")
	}
	return nil
}

func (s *openStackSuite) requireAbsent(ctx context.Context, key client.ObjectKey, object client.Object) error {
	err := s.client.Get(ctx, key, object)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("object already exists")
}

func mustSetCheck(report *e2eReport, name string, status checkStatus, summary string) {
	if err := report.setCheck(name, status, summary); err != nil {
		panic(err)
	}
}

func writeReportAfterSetupFailure(t *testing.T, directory string, report *e2eReport) {
	t.Helper()
	if err := writeE2EArtifacts(directory, report); err != nil {
		t.Error("OpenStack E2E failure artifacts could not be written")
	}
}

func TestEveryConfiguredCheckHasRunner(t *testing.T) {
	config := reportTestConfig(projectModeDedicated)
	config.LocalLifecycle, config.MemberTransitions = true, true
	config.FloatingIPRecovery, config.FloatingIPDeletion = true, true
	config.Project.FrontendSecurityGroupIDs = []string{"test-group"}
	suite := &openStackSuite{config: config}
	runners := suite.checkRunners()
	checks := configuredChecks(config)
	if len(runners) != len(checks) {
		t.Fatalf("%d configured checks have %d runners", len(checks), len(runners))
	}
	for _, name := range checks {
		if runners[name] == nil {
			t.Fatalf("configured check %q has no runner", name)
		}
	}
}
