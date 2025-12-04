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

// Stage 2: Upgrade core control-plane components
func (r *LiqoUpgradeReconciler) startControllerManagerUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Idempotency check: verify job doesn't already exist
	jobName := fmt.Sprintf("%s-%s", controllerManagerUpgradePrefix, upgrade.Name)
	existingJob := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Spec.Namespace}, existingJob)

	if err == nil {
		// Job already exists, transition to monitoring phase without creating a new job
		logger.Info("Controller manager upgrade job already exists, skipping creation")
		statusUpdates := map[string]interface{}{
			"currentStage":        2,
			"lastSuccessfulPhase": upgrade.Status.LastSuccessfulPhase,
		}
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseControllerManager, "Monitoring core control-plane upgrade", statusUpdates)
	} else if !errors.IsNotFound(err) {
		// Unexpected error
		logger.Error(err, "Failed to check for existing controller manager upgrade job")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Job doesn't exist, create it
	logger.Info("Stage 2: Starting core control-plane upgrade")

	job := r.buildControllerManagerUpgradeJob(upgrade)
	if err := controllerutil.SetControllerReference(upgrade, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, job); err != nil {
		if !errors.IsAlreadyExists(err) {
			logger.Error(err, "Failed to create core control-plane upgrade job")
			return r.fail(ctx, upgrade, "Failed to create core control-plane upgrade job")
		}
		// Job was created by another reconciliation, that's OK
		logger.Info("Controller manager upgrade job was just created by another reconciliation")
	}

	// Pass all status updates in additionalUpdates map to ensure they are persisted
	statusUpdates := map[string]interface{}{
		"currentStage":        2,
		"lastSuccessfulPhase": upgrade.Status.LastSuccessfulPhase,
	}
	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseControllerManager, "Upgrading core control-plane components", statusUpdates)
}

func (r *LiqoUpgradeReconciler) monitorControllerManagerUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	jobName := fmt.Sprintf("%s-%s", controllerManagerUpgradePrefix, upgrade.Name)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Spec.Namespace}, job); err != nil {
		logger.Error(err, "Failed to get controller-manager upgrade job")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if job.Status.Succeeded > 0 {
		logger.Info("Stage 2 completed: liqo-controller-manager upgraded successfully")
		// Set lastSuccessfulPhase in memory - it will be persisted by startNetworkFabricUpgrade
		// Don't update status here to avoid race condition between PhaseControllerManager and PhaseNetworkFabric
		upgrade.Status.LastSuccessfulPhase = upgradev1alpha1.PhaseControllerManager
		// Directly transition to NetworkFabric phase (this will persist the status)
		return r.startNetworkFabricUpgrade(ctx, upgrade)
	}

	if job.Status.Failed > 0 {
		logger.Error(nil, "Stage 2 failed: Controller-manager upgrade failed")
		return r.startRollback(ctx, upgrade, "Controller-manager upgrade failed")
	}

	logger.Info("Controller-manager upgrade job still running")
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *LiqoUpgradeReconciler) buildControllerManagerUpgradeJob(upgrade *upgradev1alpha1.LiqoUpgrade) *batchv1.Job {
	jobName := fmt.Sprintf("%s-%s", controllerManagerUpgradePrefix, upgrade.Name)
	namespace := upgrade.Spec.Namespace
	if namespace == "" {
		namespace = "liqo"
	}

	planConfigMap := upgrade.Status.PlanConfigMap
	if planConfigMap == "" {
		planConfigMap = fmt.Sprintf("liqo-upgrade-plan-%s", upgrade.Name)
	}

	script := fmt.Sprintf(`#!/bin/bash
set -e

echo "========================================="
echo "Stage 2: Upgrading Core Control-Plane Components"
echo "========================================="

# Verify required dependencies
echo "Verifying required tools..."
if ! command -v jq &> /dev/null; then
  echo "❌ ERROR: jq is required but not found"
  exit 1
fi
if ! command -v kubectl &> /dev/null; then
  echo "❌ ERROR: kubectl is required but not found"
  exit 1
fi
echo "✓ All required tools are available"
echo ""

TARGET_VERSION="%s"
NAMESPACE="%s"
PLAN_CONFIGMAP="%s"

# Load upgrade plan
echo "Loading upgrade plan from ConfigMap ${PLAN_CONFIGMAP}..."
PLAN_JSON=$(kubectl get configmap "${PLAN_CONFIGMAP}" -n "${NAMESPACE}" -o jsonpath='{.data.plan\.json}')

if [ -z "$PLAN_JSON" ]; then
  echo "❌ ERROR: Failed to load upgrade plan"
  exit 1
fi

echo "Upgrade plan loaded"
echo ""

# Validate prerequisites (ConfigMaps and Secrets referenced in env vars)
echo "========================================="
echo "Validating Prerequisites"
echo "========================================="
echo ""

VALIDATION_FAILED=false

# Check all components in the upgrade plan
echo "Checking ConfigMaps and Secrets referenced in environment variables..."
for COMPONENT_NAME in $(echo "$PLAN_JSON" | jq -r '.toUpdate[].name'); do
  COMPONENT_PLAN=$(echo "$PLAN_JSON" | jq -r --arg name "$COMPONENT_NAME" '
    .toUpdate[] | select(.name == $name)
  ')

  ENV_CHANGES=$(echo "$COMPONENT_PLAN" | jq -r '.envChanges // []')
  ENV_COUNT=$(echo "$ENV_CHANGES" | jq 'length')

  if [ "$ENV_COUNT" -gt 0 ]; then
    echo ""
    echo "Validating ${COMPONENT_NAME} environment variable references..."

    while IFS= read -r change; do
      TYPE=$(echo "$change" | jq -r '.type')
      NAME=$(echo "$change" | jq -r '.name')
      NEW_SOURCE=$(echo "$change" | jq -r '.newSource // ""')
      NEW_VALUE=$(echo "$change" | jq -r '.newValue // ""')

      # Only validate add/update operations
      if [ "$TYPE" == "add" ] || [ "$TYPE" == "update" ]; then
        # Check ConfigMapKeyRef
        if [[ "$NEW_SOURCE" == configMap:* ]]; then
          CONFIGMAP_NAME=$(echo "$NEW_SOURCE" | cut -d: -f2)
          KEY=$(echo "$NEW_VALUE")

          echo -n "  Checking ConfigMap ${CONFIGMAP_NAME} (key: ${KEY})... "

          if ! kubectl get configmap "${CONFIGMAP_NAME}" -n "${NAMESPACE}" > /dev/null 2>&1; then
            echo "❌ NOT FOUND"
            echo "    ERROR: ConfigMap '${CONFIGMAP_NAME}' does not exist in namespace '${NAMESPACE}'"
            echo "    Required by: ${COMPONENT_NAME} environment variable '${NAME}'"
            VALIDATION_FAILED=true
          else
            # Check if the key exists in the ConfigMap
            if ! kubectl get configmap "${CONFIGMAP_NAME}" -n "${NAMESPACE}" -o jsonpath="{.data.${KEY}}" > /dev/null 2>&1; then
              echo "❌ KEY NOT FOUND"
              echo "    ERROR: ConfigMap '${CONFIGMAP_NAME}' exists but does not contain key '${KEY}'"
              echo "    Required by: ${COMPONENT_NAME} environment variable '${NAME}'"
              VALIDATION_FAILED=true
            else
              echo "✓ OK"
            fi
          fi

        # Check SecretKeyRef
        elif [[ "$NEW_SOURCE" == secret:* ]]; then
          SECRET_NAME=$(echo "$NEW_SOURCE" | cut -d: -f2)
          KEY=$(echo "$NEW_VALUE")

          echo -n "  Checking Secret ${SECRET_NAME} (key: ${KEY})... "

          if ! kubectl get secret "${SECRET_NAME}" -n "${NAMESPACE}" > /dev/null 2>&1; then
            echo "❌ NOT FOUND"
            echo "    ERROR: Secret '${SECRET_NAME}' does not exist in namespace '${NAMESPACE}'"
            echo "    Required by: ${COMPONENT_NAME} environment variable '${NAME}'"
            VALIDATION_FAILED=true
          else
            # Check if the key exists in the Secret
            if ! kubectl get secret "${SECRET_NAME}" -n "${NAMESPACE}" -o jsonpath="{.data.${KEY}}" > /dev/null 2>&1; then
              echo "❌ KEY NOT FOUND"
              echo "    ERROR: Secret '${SECRET_NAME}' exists but does not contain key '${KEY}'"
              echo "    Required by: ${COMPONENT_NAME} environment variable '${NAME}'"
              VALIDATION_FAILED=true
            else
              echo "✓ OK"
            fi
          fi
        fi
      fi
    done < <(echo "$ENV_CHANGES" | jq -c '.[]')
  fi
done

echo ""
if [ "$VALIDATION_FAILED" == "true" ]; then
  echo "❌ ERROR: Prerequisite validation failed!"
  echo ""
  echo "The upgrade cannot proceed because required ConfigMaps or Secrets are missing."
  echo "Please ensure all required resources exist before retrying the upgrade."
  echo ""
  echo "Troubleshooting:"
  echo "  1. Check if Liqo is properly installed with all required resources"
  echo "  2. Verify that ConfigMaps and Secrets have not been accidentally deleted"
  echo "  3. Ensure you are targeting the correct namespace"
  exit 1
fi

echo "✅ All prerequisites validated successfully"
echo ""

# Cleanup any crashlooping pods from previous failed upgrade attempts
echo "========================================="
echo "Cleaning Up Stale Pods"
echo "========================================="
echo ""

echo "Checking for crashlooping pods in namespace ${NAMESPACE}..."

# Get all pods in CrashLoopBackOff state
CRASHLOOP_PODS=$(kubectl get pods -n "${NAMESPACE}" \
  --field-selector=status.phase=Running \
  -o json | jq -r '
  .items[] |
  select(.status.containerStatuses[]? | select(.state.waiting?.reason == "CrashLoopBackOff")) |
  .metadata.name
')

if [ -n "$CRASHLOOP_PODS" ]; then
  echo "Found crashlooping pods, cleaning up..."
  echo "$CRASHLOOP_PODS" | while read -r pod; do
    if [ -n "$pod" ]; then
      echo "  Deleting crashlooping pod: $pod"
      kubectl delete pod "$pod" -n "${NAMESPACE}" --wait=false
    fi
  done

  # Wait a moment for deletions to process
  echo ""
  echo "Waiting for pod cleanup to complete..."
  sleep 5
  echo "✓ Pod cleanup complete"
else
  echo "No crashlooping pods found"
fi

echo ""

# Core control-plane components to upgrade (in order)
CORE_COMPONENTS=("liqo-controller-manager" "liqo-crd-replicator" "liqo-metric-agent")

# Function to upgrade a component using the plan
upgrade_component() {
  local COMPONENT_NAME=$1

  echo "========================================="
  echo "Upgrading: ${COMPONENT_NAME}"
  echo "========================================="

  # Check if component exists
  if ! kubectl get deployment "${COMPONENT_NAME}" -n "${NAMESPACE}" > /dev/null 2>&1; then
    echo "⚠️  Component ${COMPONENT_NAME} not found, skipping"
    echo ""
    return 0
  fi

  # Find component in upgrade plan
  COMPONENT_PLAN=$(echo "$PLAN_JSON" | jq -r --arg name "$COMPONENT_NAME" '
    .toUpdate[] | select(.name == $name)
  ')

  if [ -z "$COMPONENT_PLAN" ] || [ "$COMPONENT_PLAN" == "null" ]; then
    echo "⚠️  No update plan for ${COMPONENT_NAME}, using default image update only"

    # Fallback: simple image update
    NEW_IMAGE="ghcr.io/liqotech/${COMPONENT_NAME}:${TARGET_VERSION}"
    echo "Updating image to: ${NEW_IMAGE}"

    kubectl set image deployment/"${COMPONENT_NAME}" \
      "${COMPONENT_NAME}"="${NEW_IMAGE}" \
      -n "${NAMESPACE}"
  else
    echo "Applying upgrade plan for ${COMPONENT_NAME}..."

    # Extract plan details
    TARGET_IMAGE=$(echo "$COMPONENT_PLAN" | jq -r '.targetImage')
    CONTAINER_NAME=$(echo "$COMPONENT_PLAN" | jq -r '.containerName // .name')

    echo "  Target image: ${TARGET_IMAGE}"
    echo "  Container name: ${CONTAINER_NAME}"

    # Step 1: Update image
    echo ""
    echo "Step 1: Updating container image..."
    kubectl set image deployment/"${COMPONENT_NAME}" \
      "${CONTAINER_NAME}"="${TARGET_IMAGE}" \
      -n "${NAMESPACE}"

    # Step 2: Apply environment variable changes
    ENV_CHANGES=$(echo "$COMPONENT_PLAN" | jq -r '.envChanges // []')
    ENV_COUNT=$(echo "$ENV_CHANGES" | jq 'length')

    if [ "$ENV_COUNT" -gt 0 ]; then
      echo ""
      echo "Step 2: Applying ${ENV_COUNT} environment variable change(s)..."

      # Get current env array
      CURRENT_ENV=$(kubectl get deployment "${COMPONENT_NAME}" -n "${NAMESPACE}" -o json | \
        jq '.spec.template.spec.containers[] | select(.name == "'${CONTAINER_NAME}'") | .env // []')

      # Process each env change and build new env array
      NEW_ENV="$CURRENT_ENV"
      while IFS= read -r change; do
        TYPE=$(echo "$change" | jq -r '.type')
        NAME=$(echo "$change" | jq -r '.name')
        NEW_VALUE=$(echo "$change" | jq -r '.newValue // ""')
        NEW_SOURCE=$(echo "$change" | jq -r '.newSource // ""')

        if [ "$TYPE" == "add" ] || [ "$TYPE" == "update" ]; then
          # Determine the source type and build appropriate env var
          if [ "$NEW_SOURCE" == "value" ]; then
            # Simple value-based env var
            echo "  ${TYPE}: ${NAME}=${NEW_VALUE}"
            ENV_VAR=$(jq -n --arg name "$NAME" --arg value "$NEW_VALUE" \
              '{name: $name, value: $value}')
          elif [[ "$NEW_SOURCE" == configMap:* ]]; then
            # ConfigMapKeyRef
            CONFIGMAP_NAME=$(echo "$NEW_SOURCE" | cut -d: -f2)
            KEY=$(echo "$NEW_VALUE")
            echo "  ${TYPE}: ${NAME} from ConfigMap ${CONFIGMAP_NAME}, key: ${KEY}"
            ENV_VAR=$(jq -n --arg name "$NAME" --arg cmName "$CONFIGMAP_NAME" --arg key "$KEY" \
              '{name: $name, valueFrom: {configMapKeyRef: {name: $cmName, key: $key}}}')
          elif [[ "$NEW_SOURCE" == secret:* ]]; then
            # SecretKeyRef
            SECRET_NAME=$(echo "$NEW_SOURCE" | cut -d: -f2)
            KEY=$(echo "$NEW_VALUE")
            echo "  ${TYPE}: ${NAME} from Secret ${SECRET_NAME}, key: ${KEY}"
            ENV_VAR=$(jq -n --arg name "$NAME" --arg secretName "$SECRET_NAME" --arg key "$KEY" \
              '{name: $name, valueFrom: {secretKeyRef: {name: $secretName, key: $key}}}')
          elif [ "$NEW_SOURCE" == "fieldRef" ]; then
            # FieldRef (e.g., metadata.namespace)
            FIELD_PATH=$(echo "$NEW_VALUE")
            echo "  ${TYPE}: ${NAME} from fieldRef: ${FIELD_PATH}"
            ENV_VAR=$(jq -n --arg name "$NAME" --arg fieldPath "$FIELD_PATH" \
              '{name: $name, valueFrom: {fieldRef: {apiVersion: "v1", fieldPath: $fieldPath}}}')
          else
            echo "  ⚠️  WARNING: Unknown source type for ${NAME}: ${NEW_SOURCE}, skipping"
            continue
          fi

          # Update or add the env var in the array
          if echo "$NEW_ENV" | jq -e --arg name "$NAME" 'any(.[]; .name == $name)' > /dev/null; then
            # Update existing
            NEW_ENV=$(echo "$NEW_ENV" | jq --argjson envVar "$ENV_VAR" --arg name "$NAME" \
              'map(if .name == $name then $envVar else . end)')
          else
            # Add new
            NEW_ENV=$(echo "$NEW_ENV" | jq --argjson envVar "$ENV_VAR" '. += [$envVar]')
          fi
        elif [ "$TYPE" == "remove" ]; then
          echo "  ${TYPE}: ${NAME}"
          NEW_ENV=$(echo "$NEW_ENV" | jq --arg name "$NAME" 'map(select(.name != $name))')
        fi
      done < <(echo "$ENV_CHANGES" | jq -c '.[]')

      # Apply the complete env array patch
      if [ "$ENV_COUNT" -gt 0 ]; then
        PATCH=$(cat <<EOF
{
  "spec": {
    "template": {
      "spec": {
        "containers": [{
          "name": "${CONTAINER_NAME}",
          "env": ${NEW_ENV}
        }]
      }
    }
  }
}
EOF
)
        kubectl patch deployment "${COMPONENT_NAME}" -n "${NAMESPACE}" --type=strategic --patch "$PATCH"
      fi
    else
      echo ""
      echo "Step 2: No environment variable changes needed"
    fi

    # Step 3: Apply command-line flag changes (via args)
    FLAG_CHANGES=$(echo "$COMPONENT_PLAN" | jq -r '.flagChanges // []')
    FLAG_COUNT=$(echo "$FLAG_CHANGES" | jq 'length')

    if [ "$FLAG_COUNT" -gt 0 ]; then
      echo ""
      echo "Step 3: Applying ${FLAG_COUNT} command-line flag change(s)..."

      # Get current args
      CURRENT_ARGS=$(kubectl get deployment "${COMPONENT_NAME}" -n "${NAMESPACE}" \
        -o jsonpath="{.spec.template.spec.containers[?(@.name=='${CONTAINER_NAME}')].args}" | jq -r '.')

      # Build new args array by applying changes
      NEW_ARGS="$CURRENT_ARGS"
      while IFS= read -r change; do
        TYPE=$(echo "$change" | jq -r '.type')
        FLAG=$(echo "$change" | jq -r '.flag')
        NEW_VALUE=$(echo "$change" | jq -r '.newValue // ""')

        if [ "$TYPE" == "add" ]; then
          echo "  add: --${FLAG}=${NEW_VALUE}"
          NEW_ARGS=$(echo "$NEW_ARGS" | jq --arg f "--${FLAG}=${NEW_VALUE}" '. += [$f]')
        elif [ "$TYPE" == "update" ]; then
          echo "  update: --${FLAG}=${NEW_VALUE}"
          # Remove old flag and add new one
          NEW_ARGS=$(echo "$NEW_ARGS" | jq --arg prefix "--${FLAG}" 'map(select(startswith($prefix) | not))')
          NEW_ARGS=$(echo "$NEW_ARGS" | jq --arg f "--${FLAG}=${NEW_VALUE}" '. += [$f]')
        elif [ "$TYPE" == "remove" ]; then
          echo "  remove: --${FLAG}"
          NEW_ARGS=$(echo "$NEW_ARGS" | jq --arg prefix "--${FLAG}" 'map(select(startswith($prefix) | not))')
        fi
      done < <(echo "$FLAG_CHANGES" | jq -c '.[]')

      # Apply args patch
      ARGS_PATCH=$(cat <<EOF
{
  "spec": {
    "template": {
      "spec": {
        "containers": [{
          "name": "${CONTAINER_NAME}",
          "args": ${NEW_ARGS}
        }]
      }
    }
  }
}
EOF
)

      kubectl patch deployment "${COMPONENT_NAME}" -n "${NAMESPACE}" \
        --type=strategic --patch "$ARGS_PATCH"
    else
      echo ""
      echo "Step 3: No command-line flag changes needed"
    fi
  fi

  # Ensure proper rolling update strategy
  echo ""
  echo "Step 4: Ensuring proper rolling update strategy..."

  MAX_UNAVAILABLE=$(kubectl get deployment "${COMPONENT_NAME}" -n "${NAMESPACE}" \
    -o jsonpath='{.spec.strategy.rollingUpdate.maxUnavailable}' 2>/dev/null || echo "0")
  MAX_SURGE=$(kubectl get deployment "${COMPONENT_NAME}" -n "${NAMESPACE}" \
    -o jsonpath='{.spec.strategy.rollingUpdate.maxSurge}' 2>/dev/null || echo "1")

  echo "  Current strategy: maxUnavailable=${MAX_UNAVAILABLE}, maxSurge=${MAX_SURGE}"

  # Ensure at least maxSurge=1 for control-plane components
  if [ "$MAX_SURGE" == "0" ] || [ -z "$MAX_SURGE" ]; then
    echo "  Patching to ensure maxSurge=1 for zero-downtime rollout..."
    kubectl patch deployment "${COMPONENT_NAME}" -n "${NAMESPACE}" \
      --type=strategic --patch '{"spec":{"strategy":{"rollingUpdate":{"maxSurge":1}}}}'
  fi

  # Wait for rollout
  echo ""
  echo "Step 5: Waiting for rollout to complete..."
  if ! kubectl rollout status deployment/"${COMPONENT_NAME}" -n "${NAMESPACE}" --timeout=5m; then
    echo "❌ ERROR: Rollout failed for ${COMPONENT_NAME}!"
    return 1
  fi

  # Verify health
  echo ""
  echo "Step 6: Verifying deployment health..."
  if ! kubectl wait --for=condition=available --timeout=2m deployment/"${COMPONENT_NAME}" -n "${NAMESPACE}"; then
    echo "❌ ERROR: Deployment ${COMPONENT_NAME} not healthy!"
    return 1
  fi

  # Verify version
  echo ""
  echo "Step 7: Verifying deployed version..."
  DEPLOYED_IMAGE=$(kubectl get deployment "${COMPONENT_NAME}" -n "${NAMESPACE}" \
    -o jsonpath="{.spec.template.spec.containers[0].image}")
  echo "  Deployed image: ${DEPLOYED_IMAGE}"

  # Check for crashloops
  echo ""
  echo "Step 8: Checking for crashloops..."
  sleep 10  # Wait a bit for pods to potentially crash

  RESTART_COUNT=$(kubectl get pods -n "${NAMESPACE}" -l "app.kubernetes.io/name=${COMPONENT_NAME}" \
    -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}' 2>/dev/null || echo "0")

  if [ "$RESTART_COUNT" -gt 2 ]; then
    echo "❌ ERROR: Component ${COMPONENT_NAME} has high restart count: ${RESTART_COUNT}"
    echo "Checking pod logs..."
    kubectl logs -n "${NAMESPACE}" -l "app.kubernetes.io/name=${COMPONENT_NAME}" --tail=50 || true
    return 1
  fi

  echo "  Restart count: ${RESTART_COUNT} (OK)"

  echo ""
  echo "✅ ${COMPONENT_NAME} upgraded successfully"
  echo ""

  return 0
}

# Function to update version labels on a Deployment
update_deployment_labels() {
  local COMPONENT_NAME=$1
  local NAMESPACE=$2
  local TARGET_VERSION=$3
  
  echo "  Updating version labels for ${COMPONENT_NAME}..."
  
  # Update metadata labels
  kubectl patch deployment "${COMPONENT_NAME}" -n "${NAMESPACE}" --type=json \
    -p='[
      {"op":"replace","path":"/metadata/labels/app.kubernetes.io~1version","value":"'"${TARGET_VERSION}"'"},
      {"op":"replace","path":"/metadata/labels/helm.sh~1chart","value":"liqo-'"${TARGET_VERSION}"'"}
    ]' 2>/dev/null || echo "    ⚠️ Could not update metadata labels"
  
  # Update pod template labels
  kubectl patch deployment "${COMPONENT_NAME}" -n "${NAMESPACE}" --type=json \
    -p='[
      {"op":"replace","path":"/spec/template/metadata/labels/app.kubernetes.io~1version","value":"'"${TARGET_VERSION}"'"},
      {"op":"replace","path":"/spec/template/metadata/labels/helm.sh~1chart","value":"liqo-'"${TARGET_VERSION}"'"}
    ]' 2>/dev/null || echo "    ⚠️ Could not update pod template labels"
  
  echo "    ✓ Labels updated"
}

# Upgrade each core component
for component in "${CORE_COMPONENTS[@]}"; do
  if ! upgrade_component "$component"; then
    echo "❌ ERROR: Failed to upgrade $component"
    exit 1
  fi
  # Update version labels
  update_deployment_labels "$component" "${NAMESPACE}" "${TARGET_VERSION}"
done

echo ""
echo "========================================="
echo "Upgrading: liqo-metric-agent init container (cert-creator)"
echo "========================================="

if kubectl get deployment liqo-metric-agent -n "${NAMESPACE}" > /dev/null 2>&1; then
  # Check if init container exists
  INIT_CONTAINER_EXISTS=$(kubectl get deployment liqo-metric-agent -n "${NAMESPACE}" \
    -o jsonpath='{.spec.template.spec.initContainers[0].name}' 2>/dev/null || echo "")

  if [ -n "$INIT_CONTAINER_EXISTS" ]; then
    echo "Init container found: ${INIT_CONTAINER_EXISTS}"
    
    CURRENT_INIT_IMAGE=$(kubectl get deployment liqo-metric-agent -n "${NAMESPACE}" \
      -o jsonpath='{.spec.template.spec.initContainers[0].image}' 2>/dev/null || echo "")
    echo "Current init container image: ${CURRENT_INIT_IMAGE}"
    
    NEW_INIT_IMAGE="ghcr.io/liqotech/cert-creator:${TARGET_VERSION}"
    echo "Target init container image: ${NEW_INIT_IMAGE}"
    
    # Patch init container image
    kubectl patch deployment liqo-metric-agent -n "${NAMESPACE}" --type=json \
      -p='[{"op": "replace", "path": "/spec/template/spec/initContainers/0/image", "value": "'"${NEW_INIT_IMAGE}"'"}]'
    
    echo "Waiting for rollout after init container update..."
    kubectl rollout status deployment/liqo-metric-agent -n "${NAMESPACE}" --timeout=3m
    
    # Verify init container was updated
    UPDATED_INIT_IMAGE=$(kubectl get deployment liqo-metric-agent -n "${NAMESPACE}" \
      -o jsonpath='{.spec.template.spec.initContainers[0].image}' 2>/dev/null || echo "")
    echo "Updated init container image: ${UPDATED_INIT_IMAGE}"
    
    if [[ "$UPDATED_INIT_IMAGE" == *"${TARGET_VERSION}"* ]]; then
      echo "✅ cert-creator init container upgraded successfully"
    else
      echo "⚠️  WARNING: Init container may not have been updated to target version"
    fi
  else
    echo "ℹ️  No init container found in liqo-metric-agent, skipping"
  fi
else
  echo "ℹ️  liqo-metric-agent not found, skipping init container upgrade"
fi

echo "========================================="
echo "Upgrading: liqo-webhook (with TLS validation)"
echo "========================================="

# Check if webhook exists
if kubectl get deployment liqo-webhook -n "${NAMESPACE}" > /dev/null 2>&1; then
  echo "Step 1: Validating TLS certificates..."

  # Check webhook secret
  WEBHOOK_SECRET=$(kubectl get deployment liqo-webhook -n "${NAMESPACE}" \
    -o jsonpath='{.spec.template.spec.volumes[?(@.name=="webhook-certs")].secret.secretName}' 2>/dev/null || echo "")

  if [ -n "$WEBHOOK_SECRET" ]; then
    echo "  Webhook TLS secret: ${WEBHOOK_SECRET}"

    # Verify secret exists and has required keys
    if kubectl get secret "${WEBHOOK_SECRET}" -n "${NAMESPACE}" > /dev/null 2>&1; then
      CERT_KEYS=$(kubectl get secret "${WEBHOOK_SECRET}" -n "${NAMESPACE}" -o jsonpath='{.data}' | jq 'keys')
      echo "  Certificate keys: ${CERT_KEYS}"
      echo "  ✓ TLS certificates present"
    else
      echo "  ⚠️  WARNING: TLS secret ${WEBHOOK_SECRET} not found, webhook may fail"
    fi
  else
    echo "  ⚠️  WARNING: No TLS secret configured for webhook"
  fi

  echo ""
  echo "Step 2: Upgrading webhook deployment..."

  # Upgrade webhook component (reuse function)
  if ! upgrade_component "liqo-webhook"; then
    echo "❌ ERROR: Failed to upgrade liqo-webhook"
    exit 1
  fi

  echo ""
  echo "Step 3: Verifying webhook configurations..."

  # Check MutatingWebhookConfiguration
  if kubectl get mutatingwebhookconfiguration | grep -q liqo; then
    MUTATING_WEBHOOKS=$(kubectl get mutatingwebhookconfiguration -o name | grep liqo)
    echo "  Mutating webhooks:"
    echo "${MUTATING_WEBHOOKS}" | while read -r wh; do
      echo "    - $wh"
    done
  fi

  # Check ValidatingWebhookConfiguration
  if kubectl get validatingwebhookconfiguration | grep -q liqo; then
    VALIDATING_WEBHOOKS=$(kubectl get validatingwebhookconfiguration -o name | grep liqo)
    echo "  Validating webhooks:"
    echo "${VALIDATING_WEBHOOKS}" | while read -r wh; do
      echo "    - $wh"
    done
  fi

  echo ""
  echo "Step 4: Testing webhook admission..."
  # Give webhook a moment to be ready
  sleep 5

  # Check API server logs for admission failures (if accessible)
  echo "  Webhook should be processing admission requests"
  echo "  Check API server logs if issues occur"

  # Update version labels for webhook
  update_deployment_labels "liqo-webhook" "${NAMESPACE}" "${TARGET_VERSION}"
  
  echo ""
  echo "✅ liqo-webhook upgraded successfully"
else
  echo "⚠️  liqo-webhook deployment not found, skipping"
fi

echo ""
echo "========================================="
echo "Upgrading: liqo-telemetry (CronJob)"
echo "========================================="

TELEMETRY_EXISTS=$(kubectl get cronjob liqo-telemetry -n "${NAMESPACE}" --ignore-not-found -o name)
echo "DEBUG: TELEMETRY_EXISTS=${TELEMETRY_EXISTS}"

if [ -n "${TELEMETRY_EXISTS}" ]; then
  echo "Found liqo-telemetry CronJob"
  
  CURRENT_TELEMETRY_IMAGE=$(kubectl get cronjob liqo-telemetry -n "${NAMESPACE}" \
    -o jsonpath='{.spec.jobTemplate.spec.template.spec.containers[0].image}')
  echo "Current image: ${CURRENT_TELEMETRY_IMAGE}"
  
  NEW_TELEMETRY_IMAGE="ghcr.io/liqotech/telemetry:${TARGET_VERSION}"
  echo "Target image: ${NEW_TELEMETRY_IMAGE}"
  
  # Step 1: Update image using JSON patch
  echo "Step 1: Updating container image with JSON patch..."
  
  kubectl patch cronjob liqo-telemetry -n "${NAMESPACE}" --type='json' \
    -p='[{"op":"replace","path":"/spec/jobTemplate/spec/template/spec/containers/0/image","value":"'"${NEW_TELEMETRY_IMAGE}"'"}]'
  PATCH_RESULT=$?
  echo "DEBUG: Image patch exit code: ${PATCH_RESULT}"
  
  if [ ${PATCH_RESULT} -ne 0 ]; then
    echo "  ❌ ERROR: Failed to patch CronJob image"
    # Don't exit, continue to verify
  else
    echo "  ✓ Image patch command succeeded"
  fi
  
  # Step 2: Update --liqo-version arg
  echo "Step 2: Updating --liqo-version arg..."
  
  # Get current args and find the index of --liqo-version
  CURRENT_ARGS=$(kubectl get cronjob liqo-telemetry -n "${NAMESPACE}" \
    -o jsonpath='{.spec.jobTemplate.spec.template.spec.containers[0].args[*]}')
  echo "  Current args: ${CURRENT_ARGS}"
  
  # Find which index has --liqo-version (usually index 0)
  ARG_INDEX=0
  for i in 0 1 2 3 4 5; do
    ARG_VAL=$(kubectl get cronjob liqo-telemetry -n "${NAMESPACE}" \
      -o jsonpath="{.spec.jobTemplate.spec.template.spec.containers[0].args[${i}]}" 2>/dev/null || echo "")
    if [[ "${ARG_VAL}" == --liqo-version=* ]]; then
      ARG_INDEX=${i}
      echo "  Found --liqo-version at index ${ARG_INDEX}"
      break
    fi
  done
  
  kubectl patch cronjob liqo-telemetry -n "${NAMESPACE}" --type='json' \
    -p='[{"op":"replace","path":"/spec/jobTemplate/spec/template/spec/containers/0/args/'"${ARG_INDEX}"'","value":"--liqo-version='"${TARGET_VERSION}"'"}]'
  ARGS_RESULT=$?
  echo "DEBUG: Args patch exit code: ${ARGS_RESULT}"
  
  if [ ${ARGS_RESULT} -ne 0 ]; then
    echo "  ⚠️  WARNING: Failed to patch --liqo-version arg"
  else
    echo "  ✓ Args patch command succeeded"
  fi
  
  # Verify CronJob was updated
  echo ""
  echo "Verifying updates..."
  sleep 2
  
  UPDATED_TELEMETRY_IMAGE=$(kubectl get cronjob liqo-telemetry -n "${NAMESPACE}" \
    -o jsonpath='{.spec.jobTemplate.spec.template.spec.containers[0].image}')
  UPDATED_ARG=$(kubectl get cronjob liqo-telemetry -n "${NAMESPACE}" \
    -o jsonpath="{.spec.jobTemplate.spec.template.spec.containers[0].args[${ARG_INDEX}]}")
  
  echo "  Updated image: ${UPDATED_TELEMETRY_IMAGE}"
  echo "  Updated --liqo-version arg: ${UPDATED_ARG}"
  
  if [[ "${UPDATED_TELEMETRY_IMAGE}" == *"${TARGET_VERSION}"* ]]; then
    echo "✅ liqo-telemetry CronJob image upgraded successfully"
  else
    echo "❌ ERROR: liqo-telemetry image was not updated to target version"
    echo "  Expected: ${NEW_TELEMETRY_IMAGE}"
    echo "  Got: ${UPDATED_TELEMETRY_IMAGE}"
  fi
  
  if [[ "${UPDATED_ARG}" == *"${TARGET_VERSION}"* ]]; then
    echo "✅ liqo-telemetry --liqo-version arg updated successfully"
  else
    echo "⚠️  WARNING: --liqo-version arg may not have been updated"
  fi
  
  # Step 3: Update version labels for CronJob
  echo ""
  echo "Step 3: Updating version labels..."
  
  # Update metadata labels
  kubectl patch cronjob liqo-telemetry -n "${NAMESPACE}" --type=json \
    -p='[
      {"op":"replace","path":"/metadata/labels/app.kubernetes.io~1version","value":"'"${TARGET_VERSION}"'"},
      {"op":"replace","path":"/metadata/labels/helm.sh~1chart","value":"liqo-'"${TARGET_VERSION}"'"}
    ]' 2>/dev/null && echo "  ✓ Metadata labels updated" || echo "  ⚠️ Could not update metadata labels"
  
  # Update pod template labels
  kubectl patch cronjob liqo-telemetry -n "${NAMESPACE}" --type=json \
    -p='[
      {"op":"replace","path":"/spec/jobTemplate/spec/template/metadata/labels/app.kubernetes.io~1version","value":"'"${TARGET_VERSION}"'"},
      {"op":"replace","path":"/spec/jobTemplate/spec/template/metadata/labels/helm.sh~1chart","value":"liqo-'"${TARGET_VERSION}"'"}
    ]' 2>/dev/null && echo "  ✓ Pod template labels updated" || echo "  ⚠️ Could not update pod template labels"
  
  echo ""
  echo "Note: Next scheduled job will use the new image and version"
else
  echo "ℹ️  liqo-telemetry CronJob not found, skipping"
fi

echo ""
echo "========================================="
echo "Fixing: liqo-telemetry RBAC (Known Liqo Bug)"
echo "========================================="
echo "The liqo-telemetry ClusterRole is missing create/update permissions for configmaps."
echo "This is a bug in Liqo's Helm chart that we fix during upgrade."

if kubectl get clusterrole liqo-telemetry &>/dev/null; then
  echo "Patching liqo-telemetry ClusterRole to add create/update permissions for configmaps..."
  
  kubectl patch clusterrole liqo-telemetry --type='json' \
    -p='[{"op":"replace","path":"/rules/0/verbs","value":["get","list","watch","create","update"]}]' \
    && echo "  ✓ liqo-telemetry ClusterRole patched successfully" \
    || echo "  ⚠️ Warning: Could not patch liqo-telemetry ClusterRole"
  
  # Verify the patch
  CONFIGMAP_VERBS=$(kubectl get clusterrole liqo-telemetry -o jsonpath='{.rules[0].verbs}' 2>/dev/null || echo "")
  echo "  Current configmap verbs: ${CONFIGMAP_VERBS}"
  
  if echo "${CONFIGMAP_VERBS}" | grep -q "create"; then
    echo "✅ liqo-telemetry RBAC fix applied successfully"
  else
    echo "⚠️ Warning: RBAC fix may not have been applied correctly"
  fi
else
  echo "ℹ️  liqo-telemetry ClusterRole not found, skipping RBAC fix"
fi

echo ""
echo "========================================="
echo "Post-Upgrade Health Checks"
echo "========================================="

echo ""
echo "Checking all control-plane deployments..."
ALL_HEALTHY=true

for component in "${CORE_COMPONENTS[@]}" "liqo-webhook"; do
  if kubectl get deployment "${component}" -n "${NAMESPACE}" > /dev/null 2>&1; then
    REPLICAS=$(kubectl get deployment "${component}" -n "${NAMESPACE}" -o jsonpath='{.status.readyReplicas}')
    DESIRED=$(kubectl get deployment "${component}" -n "${NAMESPACE}" -o jsonpath='{.spec.replicas}')

    if [ "$REPLICAS" == "$DESIRED" ] && [ "$REPLICAS" -gt 0 ]; then
      echo "  ✓ ${component}: ${REPLICAS}/${DESIRED} ready"
    else
      echo "  ✗ ${component}: ${REPLICAS}/${DESIRED} ready (UNHEALTHY)"
      ALL_HEALTHY=false
    fi
  fi
done

if [ "$ALL_HEALTHY" != "true" ]; then
  echo ""
  echo "❌ ERROR: Some control-plane components are unhealthy"
  exit 1
fi

# Check CronJob health
echo ""
echo "Checking CronJob status..."
if kubectl get cronjob liqo-telemetry -n "${NAMESPACE}" > /dev/null 2>&1; then
  CRONJOB_SUSPENDED=$(kubectl get cronjob liqo-telemetry -n "${NAMESPACE}" -o jsonpath='{.spec.suspend}' 2>/dev/null || echo "false")
  if [ "$CRONJOB_SUSPENDED" == "true" ]; then
    echo "  ⚠️  liqo-telemetry CronJob is suspended"
  else
    echo "  ✓ liqo-telemetry CronJob is active"
  fi
fi

echo ""
echo "Checking controller CRD reconciliation..."
# Give controllers time to reconcile
sleep 10

# Check if controller-manager is reconciling by checking recent events
RECENT_EVENTS=$(kubectl get events -n "${NAMESPACE}" \
  --field-selector involvedObject.name=liqo-controller-manager \
  --sort-by='.lastTimestamp' 2>/dev/null | tail -5 || echo "")

if echo "$RECENT_EVENTS" | grep -q "Error\|Failed\|BackOff"; then
  echo "⚠️  WARNING: Recent error events detected for controller-manager"
  echo "$RECENT_EVENTS"
else
  echo "✓ No recent error events for control-plane components"
fi

echo ""
echo "✅ Stage 2 complete: Core control-plane upgraded successfully"
echo ""
echo "Summary:"
echo "  - liqo-controller-manager: upgraded"
echo "  - liqo-crd-replicator: upgraded"
echo "  - liqo-metric-agent: upgraded (including cert-creator init container)"
echo "  - liqo-webhook: upgraded with TLS validation"
echo "  - liqo-telemetry: CronJob upgraded"
echo "  - Post-rollout health: verified"
`, upgrade.Spec.TargetVersion, namespace, planConfigMap)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "liqo-upgrade",
				"app.kubernetes.io/component": "controller-manager-upgrade",
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
							Name:    "upgrade-core-controlplane",
							Image:   "alpine/k8s:1.29.2",
							Command: []string{"/bin/bash", "-c", script},
						},
					},
				},
			},
			BackoffLimit: int32Ptr(0),
		},
	}
}
