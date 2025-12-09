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
	"encoding/json"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	upgradev1alpha1 "github.com/thisiskazem/liqo-upgrade-controller/api/v1alpha1"
)

// Stage 1: Upgrade CRDs
func (r *LiqoUpgradeReconciler) startCRDUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Stage 1: Starting CRD upgrade")

	upgrade.Status.CurrentStage = 1

	job := r.buildCRDUpgradeJob(upgrade)
	if err := controllerutil.SetControllerReference(upgrade, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, job); err != nil {
		if !errors.IsAlreadyExists(err) {
			logger.Error(err, "Failed to create CRD upgrade job")
			return r.fail(ctx, upgrade, "Failed to create CRD upgrade job")
		}
	}

	// Preserve any status fields that were set before calling this function
	// (e.g., previousVersion, conditions, snapshotConfigMap from validation phase)
	statusUpdates := map[string]interface{}{
		"previousVersion":   upgrade.Status.PreviousVersion,
		"conditions":        upgrade.Status.Conditions,
		"snapshotConfigMap": upgrade.Status.SnapshotConfigMap,
		"planConfigMap":     upgrade.Status.PlanConfigMap,
		"planReady":         upgrade.Status.PlanReady,
	}

	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseCRDs, "Upgrading CRDs", statusUpdates)
}

func (r *LiqoUpgradeReconciler) monitorCRDUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	jobName := fmt.Sprintf("%s-%s", crdUpgradeJobPrefix, upgrade.Name)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Spec.Namespace}, job); err != nil {
		logger.Error(err, "Failed to get CRD upgrade job")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if job.Status.Succeeded > 0 {
		logger.Info("Stage 1 completed: CRDs upgraded successfully")

		// Perform schema diff analysis
		if err := r.analyzeCRDChanges(ctx, upgrade); err != nil {
			logger.Error(err, "Failed to analyze CRD changes", "note", "non-fatal error, continuing")
		}

		// Update lastSuccessfulPhase before transitioning to next phase
		// This must be done in the startControllerManagerUpgrade call to avoid race conditions
		upgrade.Status.LastSuccessfulPhase = upgradev1alpha1.PhaseCRDs
		return r.startControllerManagerUpgrade(ctx, upgrade)
	}

	if job.Status.Failed > 0 {
		logger.Error(nil, "Stage 1 failed: CRD upgrade failed")
		return r.startRollback(ctx, upgrade, "CRD upgrade failed")
	}

	logger.Info("CRD upgrade job still running")
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *LiqoUpgradeReconciler) buildCRDUpgradeJob(upgrade *upgradev1alpha1.LiqoUpgrade) *batchv1.Job {
	jobName := fmt.Sprintf("%s-%s", crdUpgradeJobPrefix, upgrade.Name)
	namespace := upgrade.Spec.Namespace
	if namespace == "" {
		namespace = defaultLiqoNamespace
	}

	script := fmt.Sprintf(`#!/bin/bash
set -e

echo "========================================="
echo "Stage 1: Upgrading CRDs"
echo "========================================="
echo ""

# Verify required tools are available
echo "Verifying required tools..."
if ! command -v kubectl &> /dev/null; then
  echo "❌ ERROR: kubectl not found"
  exit 1
fi
if ! command -v curl &> /dev/null; then
  echo "❌ ERROR: curl not found"
  exit 1
fi
echo "✓ kubectl version: $(kubectl version --client --short 2>/dev/null || kubectl version --client)"
echo "✓ curl is available"
echo ""

TARGET_VERSION="%s"
GITHUB_API_URL="https://api.github.com/repos/liqotech/liqo/contents/deployments/liqo/charts/liqo-crds/crds?ref=${TARGET_VERSION}"
RAW_BASE_URL="https://raw.githubusercontent.com/liqotech/liqo/${TARGET_VERSION}/deployments/liqo/charts/liqo-crds/crds"
TIMEOUT=120
POLL_INTERVAL=3

echo "Fetching CRD list from GitHub for version ${TARGET_VERSION}..."
echo "API URL: ${GITHUB_API_URL}"
echo ""

# Fetch list of CRD files from GitHub API (disable pipefail for this command)
set +o pipefail
CRD_FILES=$(curl -fsSL "${GITHUB_API_URL}" 2>&1 | grep '"name":' | grep '.yaml"' | cut -d'"' -f4 || true)
set -o pipefail

if [ -z "$CRD_FILES" ]; then
  echo "❌ ERROR: Failed to fetch CRD list from GitHub"
  echo "   Tried URL: ${GITHUB_API_URL}"
  echo "   Possible causes:"
  echo "   - Network connectivity issues"
  echo "   - GitHub API rate limit"
  echo "   - Invalid target version: ${TARGET_VERSION}"
  echo "   - Repository structure changed"
  exit 1
fi

CRD_COUNT=$(echo "$CRD_FILES" | wc -l)
echo "Found ${CRD_COUNT} CRD files to apply"
echo ""

# Apply each CRD and collect CRD names
SUCCESS_COUNT=0
FAILED_COUNT=0
declare -a CRD_NAMES

for crd_file in $CRD_FILES; do
  echo "Applying ${crd_file}..."

  # Fetch CRD YAML content
  set +e
  YAML_CONTENT=$(curl -fsSL "${RAW_BASE_URL}/${crd_file}" 2>&1)
  CURL_EXIT=$?
  set -e

  if [ $CURL_EXIT -ne 0 ]; then
    echo "  ✗ Failed to fetch ${crd_file}"
    echo "     Error: ${YAML_CONTENT}"
    FAILED_COUNT=$((FAILED_COUNT + 1))
    continue
  fi

  # Apply CRD
  set +e
  APPLY_OUTPUT=$(echo "$YAML_CONTENT" | kubectl apply --server-side --force-conflicts -f - 2>&1)
  APPLY_EXIT=$?
  set -e

  if [ $APPLY_EXIT -eq 0 ]; then
    SUCCESS_COUNT=$((SUCCESS_COUNT + 1))
    echo "  ✓ ${crd_file} applied"

    # Extract CRD name from metadata.name (with improved robustness)
    # Look for metadata field, then find name within next 20 lines
    set +o pipefail
    CRD_NAME=$(echo "$YAML_CONTENT" | grep -A 20 "^metadata:" | grep "^  name:" | head -1 | awk '{print $2}' || true)
    set -o pipefail

    # Fallback: try to extract from spec.names.plural and group
    if [ -z "$CRD_NAME" ]; then
      set +o pipefail
      PLURAL=$(echo "$YAML_CONTENT" | grep -A 30 "^spec:" | grep "plural:" | head -1 | awk '{print $2}' || true)
      GROUP=$(echo "$YAML_CONTENT" | grep "^  group:" | head -1 | awk '{print $2}' || true)
      set -o pipefail

      if [ -n "$PLURAL" ] && [ -n "$GROUP" ]; then
        CRD_NAME="${PLURAL}.${GROUP}"
        echo "    (extracted name via fallback: ${CRD_NAME})"
      fi
    fi

    if [ -n "$CRD_NAME" ]; then
      CRD_NAMES+=("$CRD_NAME")
      echo "    CRD name: ${CRD_NAME}"
    else
      echo "    ⚠️  Warning: Could not extract CRD name from ${crd_file}"
    fi
  else
    FAILED_COUNT=$((FAILED_COUNT + 1))
    echo "  ✗ ${crd_file} failed"
    echo "     Error: ${APPLY_OUTPUT}"
  fi
  echo ""
done

echo "Summary: ${SUCCESS_COUNT} succeeded, ${FAILED_COUNT} failed"

if [ "$FAILED_COUNT" -gt 0 ]; then
  echo "❌ ERROR: Some CRDs failed to apply"
  exit 1
fi

echo ""
echo "========================================="
echo "Waiting for CRDs to be established..."
echo "Timeout: ${TIMEOUT}s, Poll interval: ${POLL_INTERVAL}s"
echo "========================================="
echo ""

# Check if we have any CRD names to wait for
if [ ${#CRD_NAMES[@]} -eq 0 ]; then
  echo "⚠️  Warning: No CRD names were extracted. Waiting for all Liqo CRDs to be established..."
  echo "   This may indicate an issue with CRD name extraction."
  echo ""

  # Fallback: wait for all liqo CRDs to be established
  sleep 10

  # List all liqo CRDs
  ALL_LIQO_CRDS=$(kubectl get crd -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep 'liqo.io' || true)

  if [ -n "$ALL_LIQO_CRDS" ]; then
    for crd in $ALL_LIQO_CRDS; do
      CRD_NAMES+=("$crd")
    done
    echo "Found ${#CRD_NAMES[@]} Liqo CRDs to wait for"
  fi
fi

# Wait for each CRD to be established
ELAPSED=0
PENDING_CRDS=("${CRD_NAMES[@]}")

while [ ${#PENDING_CRDS[@]} -gt 0 ] && [ $ELAPSED -lt $TIMEOUT ]; do
  NEW_PENDING=()

  for crd_name in "${PENDING_CRDS[@]}"; do
    # Check if CRD is Established
    ESTABLISHED=$(kubectl get crd "$crd_name" -o jsonpath='{.status.conditions[?(@.type=="Established")].status}' 2>/dev/null || echo "Unknown")

    if [ "$ESTABLISHED" == "True" ]; then
      echo "✓ CRD ${crd_name} is Established"
    else
      NEW_PENDING+=("$crd_name")
    fi
  done

  PENDING_CRDS=("${NEW_PENDING[@]}")

  if [ ${#PENDING_CRDS[@]} -gt 0 ]; then
    echo "Waiting for ${#PENDING_CRDS[@]} CRDs to be established..."
    sleep $POLL_INTERVAL
    ELAPSED=$((ELAPSED + POLL_INTERVAL))
  fi
done

if [ ${#PENDING_CRDS[@]} -gt 0 ]; then
  echo ""
  echo "❌ ERROR: The following CRDs did not become established within ${TIMEOUT}s:"
  for crd_name in "${PENDING_CRDS[@]}"; do
    echo "  - $crd_name"
    # Show CRD status for debugging
    echo "    Status:"
    kubectl get crd "$crd_name" -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}): {.message}{"\n"}{end}' 2>&1 | sed 's/^/      /'
  done
  exit 1
fi

echo ""
echo "✅ All CRDs are established"

# Verify minimum CRD count
LIQO_CRDS=$(kubectl get crd | grep liqo | wc -l)
echo "Found ${LIQO_CRDS} Liqo CRDs installed in cluster"

if [ "$LIQO_CRDS" -lt 15 ]; then
  echo "❌ ERROR: Expected at least 15 CRDs, found ${LIQO_CRDS}"
  exit 1
fi

echo ""
echo "========================================="
echo "Validating CRD Conversion Configuration"
echo "========================================="
echo ""

CONVERSION_WARNINGS=0

for crd_name in "${CRD_NAMES[@]}"; do
  # Get number of served versions using kubectl jsonpath (no jq needed)
  # Count versions where .served == true
  SERVED_VERSIONS=$(kubectl get crd "$crd_name" -o jsonpath='{range .spec.versions[?(@.served==true)]}{.name}{"\n"}{end}' 2>/dev/null | wc -l)

  if [ "$SERVED_VERSIONS" -gt 1 ]; then
    # Multiple served versions - check conversion strategy
    CONVERSION_STRATEGY=$(kubectl get crd "$crd_name" -o jsonpath='{.spec.conversion.strategy}' 2>/dev/null || echo "None")

    echo "CRD: ${crd_name}"
    echo "  Served versions: ${SERVED_VERSIONS}"
    echo "  Conversion strategy: ${CONVERSION_STRATEGY}"

    if [ "$CONVERSION_STRATEGY" == "None" ] || [ -z "$CONVERSION_STRATEGY" ]; then
      echo "  ⚠️  WARNING: Multiple served versions but conversion strategy is 'None'"
      echo "      This may cause issues when using different API versions."
      CONVERSION_WARNINGS=$((CONVERSION_WARNINGS + 1))
    else
      echo "  ✓ Conversion strategy configured"
    fi
    echo ""
  fi
done

if [ "$CONVERSION_WARNINGS" -gt 0 ]; then
  echo "⚠️  Found ${CONVERSION_WARNINGS} CRD(s) with potential conversion issues"
  echo "These warnings do not block the upgrade but should be reviewed."
  echo ""
fi

echo ""
echo "========================================="
echo "Scanning Existing CRD Objects"
echo "========================================="
echo ""

# For each CRD, count existing custom resources
TOTAL_OBJECTS=0

for crd_name in "${CRD_NAMES[@]}"; do
  # Extract resource name (plural) and scope from CRD
  RESOURCE_NAME=$(kubectl get crd "$crd_name" -o jsonpath='{.spec.names.plural}' 2>/dev/null || echo "")
  SCOPE=$(kubectl get crd "$crd_name" -o jsonpath='{.spec.scope}' 2>/dev/null || echo "")

  if [ -n "$RESOURCE_NAME" ]; then
    # Temporarily disable pipefail for kubectl get commands
    set +o pipefail
    set +e

    # Count objects - use appropriate flags based on scope
    if [ "$SCOPE" == "Namespaced" ]; then
      OBJECT_COUNT=$(kubectl get "$RESOURCE_NAME" --all-namespaces --ignore-not-found 2>/dev/null | tail -n +2 | wc -l)
    else
      # Cluster-scoped resource - don't use --all-namespaces
      OBJECT_COUNT=$(kubectl get "$RESOURCE_NAME" --ignore-not-found 2>/dev/null | tail -n +2 | wc -l)
    fi

    # Re-enable error handling
    set -e
    set -o pipefail

    if [ "$OBJECT_COUNT" -gt 0 ]; then
      echo "CRD: ${crd_name}"
      echo "  Resource: ${RESOURCE_NAME}"
      echo "  Scope: ${SCOPE}"
      echo "  Object count: ${OBJECT_COUNT}"
      TOTAL_OBJECTS=$((TOTAL_OBJECTS + OBJECT_COUNT))
      echo ""
    fi
  fi
done

echo "Total Liqo custom resource objects in cluster: ${TOTAL_OBJECTS}"
echo ""

echo "✅ Stage 1 complete: CRDs upgraded successfully"
echo ""
echo "Summary:"
echo "  - Applied: ${SUCCESS_COUNT} CRDs"
echo "  - Established: ${#CRD_NAMES[@]} CRDs"
echo "  - Conversion warnings: ${CONVERSION_WARNINGS}"
echo "  - Existing objects: ${TOTAL_OBJECTS}"
`, upgrade.Spec.TargetVersion)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "liqo-upgrade",
				"app.kubernetes.io/component": "crd-upgrade",
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: int32Ptr(300),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: "liqo-upgrade-controller",
					RestartPolicy:      corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "upgrade-crds",
							Image:   "bitnami/kubectl:latest",
							Command: []string{"/bin/bash", "-c", script},
						},
					},
				},
			},
			BackoffLimit: int32Ptr(0),
		},
	}
}

// analyzeCRDChanges compares CRDs before and after upgrade to detect incompatible changes
func (r *LiqoUpgradeReconciler) analyzeCRDChanges(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) error {
	logger := log.FromContext(ctx)
	logger.Info("Analyzing CRD changes after upgrade")

	namespace := upgrade.Spec.Namespace
	if namespace == "" {
		namespace = defaultLiqoNamespace
	}

	// Load old CRDs from snapshot
	snapshotConfigMapName := upgrade.Status.SnapshotConfigMap
	if snapshotConfigMapName == "" {
		return fmt.Errorf("no snapshot ConfigMap found in status")
	}

	snapshotConfigMap := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: snapshotConfigMapName, Namespace: namespace}, snapshotConfigMap); err != nil {
		return fmt.Errorf("failed to get snapshot ConfigMap: %w", err)
	}

	snapshotJSON, ok := snapshotConfigMap.Data["snapshot.json"]
	if !ok {
		return fmt.Errorf("snapshot.json not found in ConfigMap")
	}

	var snapshot ClusterSnapshot
	if err := json.Unmarshal([]byte(snapshotJSON), &snapshot); err != nil {
		return fmt.Errorf("failed to unmarshal snapshot: %w", err)
	}

	// Get current CRDs
	currentCRDs, err := r.inventoryCRDs(ctx)
	if err != nil {
		return fmt.Errorf("failed to inventory current CRDs: %w", err)
	}

	// Build maps for easy comparison
	oldCRDMap := make(map[string]CRDSnapshot)
	for _, crd := range snapshot.CRDs {
		oldCRDMap[crd.Name] = crd
	}

	newCRDMap := make(map[string]CRDSnapshot)
	for _, crd := range currentCRDs {
		newCRDMap[crd.Name] = crd
	}

	// Analyze changes
	var warnings []string
	changedCRDs := 0

	for crdName, newCRD := range newCRDMap {
		oldCRD, existed := oldCRDMap[crdName]

		if !existed {
			logger.Info("New CRD added", "crd", crdName, "versions", newCRD.Versions)
			continue
		}

		// Compare versions
		oldVersionsSet := make(map[string]bool)
		for _, v := range oldCRD.Versions {
			oldVersionsSet[v] = true
		}

		newVersionsSet := make(map[string]bool)
		for _, v := range newCRD.Versions {
			newVersionsSet[v] = true
		}

		// Detect removed versions
		var removedVersions []string
		for v := range oldVersionsSet {
			if !newVersionsSet[v] {
				removedVersions = append(removedVersions, v)
			}
		}

		// Detect added versions
		var addedVersions []string
		for v := range newVersionsSet {
			if !oldVersionsSet[v] {
				addedVersions = append(addedVersions, v)
			}
		}

		// Check storage version changes
		storageVersionChanged := oldCRD.StorageVersion != newCRD.StorageVersion

		if len(removedVersions) > 0 || len(addedVersions) > 0 || storageVersionChanged {
			changedCRDs++

			logger.Info("CRD schema changed",
				"crd", crdName,
				"addedVersions", addedVersions,
				"removedVersions", removedVersions,
				"oldStorageVersion", oldCRD.StorageVersion,
				"newStorageVersion", newCRD.StorageVersion,
			)

			if len(removedVersions) > 0 {
				warning := fmt.Sprintf("CRD %s: removed versions %v", crdName, removedVersions)
				warnings = append(warnings, warning)
				logger.Info("⚠️  Potential incompatibility", "warning", warning)
			}

			if storageVersionChanged && oldCRD.StorageVersion != "" {
				warning := fmt.Sprintf("CRD %s: storage version changed from %s to %s",
					crdName, oldCRD.StorageVersion, newCRD.StorageVersion)
				warnings = append(warnings, warning)
				logger.Info("⚠️  Storage version changed", "warning", warning)
			}
		}
	}

	// Check for deleted CRDs
	for crdName := range oldCRDMap {
		if _, exists := newCRDMap[crdName]; !exists {
			warning := fmt.Sprintf("CRD %s was removed", crdName)
			warnings = append(warnings, warning)
			logger.Info("⚠️  CRD removed", "crd", crdName)
		}
	}

	logger.Info("CRD change analysis complete",
		"totalCRDs", len(newCRDMap),
		"changedCRDs", changedCRDs,
		"warnings", len(warnings),
	)

	// Store warnings in upgrade status condition if any exist
	if len(warnings) > 0 {
		warningsMsg := fmt.Sprintf("Found %d CRD change(s) that may affect compatibility", len(warnings))
		condition := metav1.Condition{
			Type:               string(upgradev1alpha1.ConditionCRDWarnings),
			Status:             metav1.ConditionTrue,
			Reason:             "CRDSchemaChanged",
			Message:            warningsMsg,
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: upgrade.Generation,
		}

		// Update status with retry logic
		if err := r.updateStatusCondition(ctx, upgrade, condition); err != nil {
			return fmt.Errorf("failed to update CRD warnings condition: %w", err)
		}

		logger.Info("CRD upgrade warnings", "count", len(warnings), "details", warnings)
	} else {
		// No warnings - set condition to False
		condition := metav1.Condition{
			Type:               string(upgradev1alpha1.ConditionCRDWarnings),
			Status:             metav1.ConditionFalse,
			Reason:             "NoSchemaChanges",
			Message:            "No incompatible CRD changes detected",
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: upgrade.Generation,
		}

		// Update status with retry logic
		if err := r.updateStatusCondition(ctx, upgrade, condition); err != nil {
			return fmt.Errorf("failed to update CRD warnings condition: %w", err)
		}
	}

	return nil
}
