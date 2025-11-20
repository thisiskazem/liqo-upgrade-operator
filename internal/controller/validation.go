/*
Copyright 2025.

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
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	upgradev1alpha1 "github.com/thisiskazem/liqo-upgrade-controller/api/v1alpha1"
)

// CompatibilityMatrix represents the version compatibility data
type CompatibilityMatrix map[string][]string

// Stage 0: Start & Validation
func (r *LiqoUpgradeReconciler) startValidation(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Stage 0: Starting validation phase")

	// Initialize status
	upgrade.Status.TotalStages = 9 // Stage 0-8 per design (now implementing through Stage 3)
	upgrade.Status.CurrentStage = 0

	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseValidating, "Validating compatibility and prerequisites", nil)
}

func (r *LiqoUpgradeReconciler) performValidation(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Performing validation checks")

	namespace := upgrade.Spec.Namespace
	if namespace == "" {
		namespace = "liqo"
	}

	// Step 1: Verify cluster identity
	logger.Info("Step 1: Verifying cluster identity")
	if err := r.verifyClusterIdentity(ctx, namespace); err != nil {
		return r.fail(ctx, upgrade, fmt.Sprintf("Cluster identity verification failed: %v", err))
	}

	// Step 2: Detect local cluster version from liqo-controller-manager image
	logger.Info("Step 2: Detecting local cluster Liqo version")
	localVersion, err := r.detectDeployedVersion(ctx, namespace)
	if err != nil {
		return r.fail(ctx, upgrade, fmt.Sprintf("Failed to detect local cluster version: %v", err))
	}
	logger.Info("Local cluster version detected", "version", localVersion)

	// Step 3: Get remote cluster versions from ForeignCluster CRs
	logger.Info("Step 3: Getting remote cluster versions from ForeignCluster CRs")
	remoteVersions, err := r.getRemoteClusterVersions(ctx, localVersion)
	if err != nil {
		return r.fail(ctx, upgrade, fmt.Sprintf("Failed to get remote cluster versions: %v", err))
	}

	// Step 4: Find minimum version among all versions (local + remotes)
	logger.Info("Step 4: Finding minimum version among all clusters")
	allVersions := append([]string{localVersion}, remoteVersions...)
	minimumVersion := r.findMinimumVersion(allVersions)
	logger.Info("Minimum version across all clusters", "version", minimumVersion)

	// Step 5: Load compatibility matrix and check if minimum version can upgrade to target
	logger.Info("Step 5: Checking compatibility matrix")
	matrix, err := r.loadCompatibilityMatrix(ctx, namespace)
	if err != nil {
		return r.fail(ctx, upgrade, fmt.Sprintf("Failed to load compatibility matrix: %v", err))
	}

	if !r.isCompatible(matrix, minimumVersion, upgrade.Spec.TargetVersion) {
		return r.fail(ctx, upgrade, fmt.Sprintf("Incompatible upgrade: minimum version %s cannot upgrade to %s", minimumVersion, upgrade.Spec.TargetVersion))
	}
	logger.Info("Compatibility check passed", "from", minimumVersion, "to", upgrade.Spec.TargetVersion)

	// Step 6: Load target version descriptor
	logger.Info("Step 6: Loading target version descriptor")
	targetDescriptor, err := r.loadTargetDescriptor(ctx, upgrade.Spec.TargetVersion, namespace)
	if err != nil {
		return r.fail(ctx, upgrade, fmt.Sprintf("Failed to load target descriptor: %v", err))
	}
	logger.Info("Target descriptor loaded successfully", "version", targetDescriptor.Version, "components", len(targetDescriptor.Components))

	// Step 7: Check component health
	logger.Info("Step 7: Checking component health")
	if err := r.verifyComponentHealth(ctx, namespace); err != nil {
		return r.fail(ctx, upgrade, fmt.Sprintf("Component health check failed: %v", err))
	}

	// Step 8: Create live inventory and snapshot
	logger.Info("Step 8: Creating live inventory snapshot")
	snapshot, err := r.createLiveInventory(ctx, namespace, localVersion)
	if err != nil {
		return r.fail(ctx, upgrade, fmt.Sprintf("Failed to create live inventory: %v", err))
	}

	if err := r.createSnapshotConfigMap(ctx, upgrade, snapshot, namespace); err != nil {
		return r.fail(ctx, upgrade, fmt.Sprintf("Failed to create snapshot ConfigMap: %v", err))
	}

	// Step 9: Build upgrade plan (compare snapshot vs target descriptor)
	logger.Info("Step 9: Building upgrade plan")
	plan, err := r.buildUpgradePlan(ctx, snapshot, targetDescriptor)
	if err != nil {
		return r.fail(ctx, upgrade, fmt.Sprintf("Failed to build upgrade plan: %v", err))
	}

	if err := r.createUpgradePlanConfigMap(ctx, upgrade, plan, namespace); err != nil {
		return r.fail(ctx, upgrade, fmt.Sprintf("Failed to create upgrade plan ConfigMap: %v", err))
	}
	logger.Info("Upgrade plan created", "toCreate", len(plan.ToCreate), "toUpdate", len(plan.ToUpdate), "toDelete", len(plan.ToDelete))

	// Step 10: Save previous version and add Compatible condition
	// Note: We update status fields here but don't transition phase yet
	// The phase transition to PhaseCRDs happens in startCRDUpgrade()
	upgrade.Status.PreviousVersion = localVersion

	condition := metav1.Condition{
		Type:               string(upgradev1alpha1.ConditionCompatible),
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "ValidationPassed",
		Message:            fmt.Sprintf("Minimum version %s → %s is compatible", minimumVersion, upgrade.Spec.TargetVersion),
	}
	upgrade.Status.Conditions = []metav1.Condition{condition}

	// Move to next phase: CRD Upgrade
	// This will update the status to PhaseCRDs
	return r.startCRDUpgrade(ctx, upgrade)
}

func (r *LiqoUpgradeReconciler) verifyClusterIdentity(ctx context.Context, namespace string) error {
	// Verify ForeignCluster CRD exists
	deployment := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      "liqo-controller-manager",
		Namespace: namespace,
	}, deployment)
	return err
}

func (r *LiqoUpgradeReconciler) detectDeployedVersion(ctx context.Context, namespace string) (string, error) {
	deployment := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      "liqo-controller-manager",
		Namespace: namespace,
	}, deployment)
	if err != nil {
		return "", err
	}

	if len(deployment.Spec.Template.Spec.Containers) == 0 {
		return "", fmt.Errorf("no containers found in deployment")
	}

	image := deployment.Spec.Template.Spec.Containers[0].Image
	parts := strings.Split(image, ":")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid image format: %s", image)
	}

	version := parts[len(parts)-1]
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}

	return version, nil
}

func (r *LiqoUpgradeReconciler) verifyComponentHealth(ctx context.Context, namespace string) error {
	deployment := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      "liqo-controller-manager",
		Namespace: namespace,
	}, deployment)
	if err != nil {
		return err
	}

	if deployment.Status.ReadyReplicas < 1 {
		return fmt.Errorf("liqo-controller-manager not ready: %d/%d", deployment.Status.ReadyReplicas, *deployment.Spec.Replicas)
	}

	return nil
}

func (r *LiqoUpgradeReconciler) loadCompatibilityMatrix(ctx context.Context, namespace string) (CompatibilityMatrix, error) {
	configMap := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      compatibilityConfigMap,
		Namespace: namespace,
	}, configMap)
	if err != nil {
		return nil, err
	}

	yamlData, ok := configMap.Data["compatibility.yaml"]
	if !ok {
		return nil, fmt.Errorf("compatibility.yaml not found in ConfigMap")
	}

	var matrix CompatibilityMatrix
	if err := yaml.Unmarshal([]byte(yamlData), &matrix); err != nil {
		return nil, err
	}

	return matrix, nil
}

func (r *LiqoUpgradeReconciler) isCompatible(matrix CompatibilityMatrix, sourceVersion, targetVersion string) bool {
	compatibleVersions, exists := matrix[sourceVersion]
	if !exists {
		return false
	}

	for _, compatible := range compatibleVersions {
		if compatible == targetVersion {
			return true
		}
	}
	return false
}

// getRemoteClusterVersions retrieves version information from all ForeignCluster CRs
func (r *LiqoUpgradeReconciler) getRemoteClusterVersions(ctx context.Context, localVersion string) ([]string, error) {
	logger := log.FromContext(ctx)

	// List all ForeignCluster resources
	foreignClusterList := &unstructured.UnstructuredList{}
	foreignClusterList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "core.liqo.io",
		Version: "v1beta1",
		Kind:    "ForeignCluster",
	})

	if err := r.List(ctx, foreignClusterList); err != nil {
		return nil, fmt.Errorf("failed to list ForeignCluster resources: %w", err)
	}

	logger.Info("Found ForeignCluster resources", "count", len(foreignClusterList.Items))

	var remoteVersions []string
	for _, fc := range foreignClusterList.Items {
		clusterID := fc.GetName()

		// Extract status.remoteVersion if it exists
		remoteVersion, found, err := unstructured.NestedString(fc.Object, "status", "remoteVersion")
		if err != nil {
			logger.Info("Warning: failed to extract remoteVersion from ForeignCluster", "clusterID", clusterID, "error", err.Error())
			// If error accessing field, use local version as default
			remoteVersions = append(remoteVersions, localVersion)
			continue
		}

		if !found || remoteVersion == "" {
			// status.remoteVersion doesn't exist or is empty, use local version as default
			logger.Info("ForeignCluster has no remoteVersion, using local version", "clusterID", clusterID, "defaultVersion", localVersion)
			remoteVersions = append(remoteVersions, localVersion)
		} else {
			logger.Info("ForeignCluster version", "clusterID", clusterID, "version", remoteVersion)
			remoteVersions = append(remoteVersions, remoteVersion)
		}
	}

	return remoteVersions, nil
}

// findMinimumVersion finds the minimum version from a list of versions
func (r *LiqoUpgradeReconciler) findMinimumVersion(versions []string) string {
	if len(versions) == 0 {
		return ""
	}

	// Start with the first version as minimum
	minVersion := versions[0]

	// Compare with all other versions
	for _, version := range versions[1:] {
		if r.compareVersions(version, minVersion) < 0 {
			minVersion = version
		}
	}

	return minVersion
}

// compareVersions compares two version strings
// Returns: -1 if v1 < v2, 0 if v1 == v2, 1 if v1 > v2
func (r *LiqoUpgradeReconciler) compareVersions(v1, v2 string) int {
	// Remove 'v' prefix if present
	v1 = strings.TrimPrefix(v1, "v")
	v2 = strings.TrimPrefix(v2, "v")

	// Simple string comparison for semantic versions
	// This works for versions like "1.0.0", "1.0.1", etc.
	parts1 := strings.Split(v1, ".")
	parts2 := strings.Split(v2, ".")

	// Compare each part
	maxLen := len(parts1)
	if len(parts2) > maxLen {
		maxLen = len(parts2)
	}

	for i := 0; i < maxLen; i++ {
		var p1, p2 int
		if i < len(parts1) {
			fmt.Sscanf(parts1[i], "%d", &p1)
		}
		if i < len(parts2) {
			fmt.Sscanf(parts2[i], "%d", &p2)
		}

		if p1 < p2 {
			return -1
		} else if p1 > p2 {
			return 1
		}
	}

	return 0
}
