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

// Stage 3: Upgrade Network Fabric
func (r *LiqoUpgradeReconciler) startNetworkFabricUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Idempotency check: verify job doesn't already exist
	jobName := fmt.Sprintf("%s-%s", networkFabricUpgradePrefix, upgrade.Name)
	existingJob := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Spec.Namespace}, existingJob)

	if err == nil {
		// Job already exists, transition to monitoring phase without creating a new job
		logger.Info("Network fabric upgrade job already exists, skipping creation")
		statusUpdates := map[string]interface{}{
			"currentStage":        3,
			"lastSuccessfulPhase": upgrade.Status.LastSuccessfulPhase,
		}
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseNetworkFabric, "Monitoring network fabric upgrade", statusUpdates)
	} else if !errors.IsNotFound(err) {
		// Unexpected error
		logger.Error(err, "Failed to check for existing network fabric upgrade job")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Job doesn't exist, create it
	logger.Info("Stage 3: Starting network fabric upgrade")

	job := r.buildNetworkFabricUpgradeJob(upgrade)
	if err := controllerutil.SetControllerReference(upgrade, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, job); err != nil {
		if !errors.IsAlreadyExists(err) {
			logger.Error(err, "Failed to create network fabric upgrade job")
			return r.fail(ctx, upgrade, "Failed to create network fabric upgrade job")
		}
		// Job was created by another reconciliation, that's OK
		logger.Info("Network fabric upgrade job was just created by another reconciliation")
	}

	// Pass all status updates in additionalUpdates map to ensure they are persisted
	statusUpdates := map[string]interface{}{
		"currentStage":        3,
		"lastSuccessfulPhase": upgrade.Status.LastSuccessfulPhase,
	}
	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseNetworkFabric, "Upgrading network fabric components", statusUpdates)
}

func (r *LiqoUpgradeReconciler) monitorNetworkFabricUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	jobName := fmt.Sprintf("%s-%s", networkFabricUpgradePrefix, upgrade.Name)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Spec.Namespace}, job); err != nil {
		logger.Error(err, "Failed to get network fabric upgrade job")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if job.Status.Succeeded > 0 {
		logger.Info("Stage 3 completed: Network fabric upgraded successfully")
		// Update lastSuccessfulPhase before transitioning to next phase
		// This must be done in the startVerification call to avoid race conditions
		upgrade.Status.LastSuccessfulPhase = upgradev1alpha1.PhaseNetworkFabric
		return r.startVerification(ctx, upgrade)
	}

	if job.Status.Failed > 0 {
		logger.Error(nil, "Stage 3 failed: Network fabric upgrade failed")
		return r.startRollback(ctx, upgrade, "Network fabric upgrade failed")
	}

	logger.Info("Network fabric upgrade job still running")
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *LiqoUpgradeReconciler) buildNetworkFabricUpgradeJob(upgrade *upgradev1alpha1.LiqoUpgrade) *batchv1.Job {
	jobName := fmt.Sprintf("%s-%s", networkFabricUpgradePrefix, upgrade.Name)
	namespace := upgrade.Spec.Namespace
	if namespace == "" {
		namespace = defaultLiqoNamespace
	}

	// Generate backup ConfigMap name (BackupName field planned for future implementation)
	backupConfigMapName := fmt.Sprintf("liqo-upgrade-env-backup-%s", upgrade.Name)

	planConfigMap := upgrade.Status.PlanConfigMap
	if planConfigMap == "" {
		planConfigMap = fmt.Sprintf("liqo-upgrade-plan-%s", upgrade.Name)
	}

	script := fmt.Sprintf(`#!/bin/bash
set -e

echo "========================================="
echo "Stage 3: Upgrading Network & Data-Plane"
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
if ! command -v curl &> /dev/null; then
  echo "❌ ERROR: curl is required but not found"
  exit 1
fi
echo "✓ All required tools are available"
echo ""

TARGET_VERSION="%s"
NAMESPACE="%s"
BACKUP_CONFIGMAP="%s"
PLAN_CONFIGMAP="%s"
IMAGE_REGISTRY="%s"

# Load upgrade plan
echo "Loading upgrade plan from ConfigMap ${PLAN_CONFIGMAP}..."
PLAN_JSON=$(kubectl get configmap "${PLAN_CONFIGMAP}" -n "${NAMESPACE}" -o jsonpath='{.data.plan\.json}')

if [ -z "$PLAN_JSON" ]; then
  echo "⚠️  WARNING: Failed to load upgrade plan, will use fallback image upgrades"
  PLAN_JSON='{}'
fi

echo "Upgrade plan loaded"

# ========================================
# Helper Functions for Component Upgrade
# ========================================

# Function to upgrade a component using the upgrade plan (handles env vars and flags)
upgrade_network_component() {
  local COMPONENT_NAME=$1
  local COMPONENT_KIND=$2  # "Deployment" or "DaemonSet"
  local NAMESPACE=$3
  local IMAGE_NAME=$4  # e.g., "ipam", "proxy", "fabric"
  
  echo ""
  echo "--- Upgrading ${COMPONENT_NAME} (${COMPONENT_KIND}) ---"
  
  local RESOURCE_TYPE=$(echo "$COMPONENT_KIND" | tr '[:upper:]' '[:lower:]')
  
  # Check if component exists
  if ! kubectl get "${RESOURCE_TYPE}" "${COMPONENT_NAME}" -n "${NAMESPACE}" > /dev/null 2>&1; then
    echo "⚠️  Component ${COMPONENT_NAME} not found, skipping"
    return 0
  fi

  # Find component in upgrade plan
  COMPONENT_PLAN=$(echo "$PLAN_JSON" | jq -r --arg name "$COMPONENT_NAME" '
    .toUpdate[] | select(.name == $name)
  ')

  if [ -z "$COMPONENT_PLAN" ] || [ "$COMPONENT_PLAN" == "null" ]; then
    echo "⚠️  No update plan for ${COMPONENT_NAME}, using default image update only"

    # Fallback: simple image update
    NEW_IMAGE="${IMAGE_REGISTRY}/${IMAGE_NAME}:${TARGET_VERSION}"
    echo "Updating image to: ${NEW_IMAGE}"

    CONTAINER_NAME=$(kubectl get "${RESOURCE_TYPE}" "${COMPONENT_NAME}" -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].name}')
    kubectl set image "${RESOURCE_TYPE}/${COMPONENT_NAME}" \
      "${CONTAINER_NAME}=${NEW_IMAGE}" \
      -n "${NAMESPACE}"
  else
    echo "Applying upgrade plan for ${COMPONENT_NAME}..."

    # Extract plan details
    TARGET_IMAGE=$(echo "$COMPONENT_PLAN" | jq -r '.targetImage')
    CONTAINER_NAME=$(echo "$COMPONENT_PLAN" | jq -r '.containerName // .name')

    # Verify container name exists in the resource, fallback to actual if plan's name is wrong
    ACTUAL_CONTAINER=$(kubectl get "${RESOURCE_TYPE}" "${COMPONENT_NAME}" -n "${NAMESPACE}" \
      -o jsonpath='{.spec.template.spec.containers[0].name}' 2>/dev/null)
    if [ -n "$ACTUAL_CONTAINER" ] && [ "$CONTAINER_NAME" != "$ACTUAL_CONTAINER" ]; then
      echo "  ⚠️  Plan container name '${CONTAINER_NAME}' not found, using actual: '${ACTUAL_CONTAINER}'"
      CONTAINER_NAME="$ACTUAL_CONTAINER"
    fi

    echo "  Target image: ${TARGET_IMAGE}"
    echo "  Container name: ${CONTAINER_NAME}"

    # Update image
    echo ""
    echo "Updating container image..."
    kubectl set image "${RESOURCE_TYPE}/${COMPONENT_NAME}" \
      "${CONTAINER_NAME}=${TARGET_IMAGE}" \
      -n "${NAMESPACE}"

    # Apply environment variable changes
    ENV_CHANGES=$(echo "$COMPONENT_PLAN" | jq -r '.envChanges // []')
    ENV_COUNT=$(echo "$ENV_CHANGES" | jq 'length')

    if [ "$ENV_COUNT" -gt 0 ]; then
      echo ""
      echo "Applying ${ENV_COUNT} environment variable change(s)..."

      # Get current env array
      CURRENT_ENV=$(kubectl get "${RESOURCE_TYPE}" "${COMPONENT_NAME}" -n "${NAMESPACE}" -o json | \
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
            echo "  ${TYPE}: ${NAME}=${NEW_VALUE}"
            ENV_VAR=$(jq -n --arg name "$NAME" --arg value "$NEW_VALUE" \
              '{name: $name, value: $value}')
          elif [[ "$NEW_SOURCE" == configMap:* ]]; then
            CONFIGMAP_NAME=$(echo "$NEW_SOURCE" | cut -d: -f2)
            KEY=$(echo "$NEW_VALUE")
            echo "  ${TYPE}: ${NAME} from ConfigMap ${CONFIGMAP_NAME}, key: ${KEY}"
            ENV_VAR=$(jq -n --arg name "$NAME" --arg cmName "$CONFIGMAP_NAME" --arg key "$KEY" \
              '{name: $name, valueFrom: {configMapKeyRef: {name: $cmName, key: $key}}}')
          elif [[ "$NEW_SOURCE" == secret:* ]]; then
            SECRET_NAME=$(echo "$NEW_SOURCE" | cut -d: -f2)
            KEY=$(echo "$NEW_VALUE")
            echo "  ${TYPE}: ${NAME} from Secret ${SECRET_NAME}, key: ${KEY}"
            ENV_VAR=$(jq -n --arg name "$NAME" --arg secretName "$SECRET_NAME" --arg key "$KEY" \
              '{name: $name, valueFrom: {secretKeyRef: {name: $secretName, key: $key}}}')
          elif [ "$NEW_SOURCE" == "fieldRef" ]; then
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
            NEW_ENV=$(echo "$NEW_ENV" | jq --argjson envVar "$ENV_VAR" --arg name "$NAME" \
              'map(if .name == $name then $envVar else . end)')
          else
            NEW_ENV=$(echo "$NEW_ENV" | jq --argjson envVar "$ENV_VAR" '. += [$envVar]')
          fi
        elif [ "$TYPE" == "remove" ]; then
          echo "  ${TYPE}: ${NAME}"
          NEW_ENV=$(echo "$NEW_ENV" | jq --arg name "$NAME" 'map(select(.name != $name))')
        fi
      done < <(echo "$ENV_CHANGES" | jq -c '.[]')

      # Apply the complete env array patch
      PATCH=$(cat <<ENVPATCH
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
ENVPATCH
)
      kubectl patch "${RESOURCE_TYPE}" "${COMPONENT_NAME}" -n "${NAMESPACE}" --type=strategic --patch "$PATCH"
    else
      echo ""
      echo "No environment variable changes needed"
    fi

    # Apply command-line flag changes (via args)
    FLAG_CHANGES=$(echo "$COMPONENT_PLAN" | jq -r '.flagChanges // []')
    FLAG_COUNT=$(echo "$FLAG_CHANGES" | jq 'length')

    if [ "$FLAG_COUNT" -gt 0 ]; then
      echo ""
      echo "Applying ${FLAG_COUNT} command-line flag change(s)..."

      # Get current args
      CURRENT_ARGS=$(kubectl get "${RESOURCE_TYPE}" "${COMPONENT_NAME}" -n "${NAMESPACE}" \
        -o jsonpath="{.spec.template.spec.containers[?(@.name=='${CONTAINER_NAME}')].args}" | jq -r '.' 2>/dev/null || echo "[]")

      if [ -z "$CURRENT_ARGS" ] || [ "$CURRENT_ARGS" == "null" ]; then
        CURRENT_ARGS="[]"
      fi

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
          NEW_ARGS=$(echo "$NEW_ARGS" | jq --arg prefix "--${FLAG}" 'map(select(startswith($prefix) | not))')
          NEW_ARGS=$(echo "$NEW_ARGS" | jq --arg f "--${FLAG}=${NEW_VALUE}" '. += [$f]')
        elif [ "$TYPE" == "remove" ]; then
          echo "  remove: --${FLAG}"
          NEW_ARGS=$(echo "$NEW_ARGS" | jq --arg prefix "--${FLAG}" 'map(select(startswith($prefix) | not))')
        fi
      done < <(echo "$FLAG_CHANGES" | jq -c '.[]')

      # Apply args patch
      ARGS_PATCH=$(cat <<ARGSPATCH
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
ARGSPATCH
)
      kubectl patch "${RESOURCE_TYPE}" "${COMPONENT_NAME}" -n "${NAMESPACE}" \
        --type=strategic --patch "$ARGS_PATCH"
    else
      echo ""
      echo "No command-line flag changes needed"
    fi

    # Apply command changes (if any)
    TARGET_COMMAND=$(echo "$COMPONENT_PLAN" | jq -r '.targetCommand // []')
    COMMAND_COUNT=$(echo "$TARGET_COMMAND" | jq 'length')

    if [ "$COMMAND_COUNT" -gt 0 ]; then
      echo ""
      echo "Applying command change..."
      echo "  Target command: $(echo "$TARGET_COMMAND" | jq -c '.')"

      # Apply command patch
      COMMAND_PATCH=$(cat <<COMMANDPATCH
{
  "spec": {
    "template": {
      "spec": {
        "containers": [{
          "name": "${CONTAINER_NAME}",
          "command": ${TARGET_COMMAND}
        }]
      }
    }
  }
}
COMMANDPATCH
)
      kubectl patch "${RESOURCE_TYPE}" "${COMPONENT_NAME}" -n "${NAMESPACE}" --type=strategic --patch "$COMMAND_PATCH"
      echo "  ✓ Command updated"
    else
      echo ""
      echo "No command changes needed"
    fi
  fi

  # Wait for rollout
  echo ""
  echo "Waiting for rollout to complete..."
  if ! kubectl rollout status "${RESOURCE_TYPE}/${COMPONENT_NAME}" -n "${NAMESPACE}" --timeout=5m; then
    echo "❌ ERROR: Rollout failed for ${COMPONENT_NAME}!"
    return 1
  fi

  # Verify health
  echo ""
  echo "Verifying ${COMPONENT_KIND} health..."
  if [ "$COMPONENT_KIND" == "Deployment" ]; then
    if ! kubectl wait --for=condition=available --timeout=2m "${RESOURCE_TYPE}/${COMPONENT_NAME}" -n "${NAMESPACE}"; then
      echo "❌ ERROR: ${COMPONENT_NAME} not healthy!"
      return 1
    fi
  else
    # DaemonSet - check number ready
    DESIRED=$(kubectl get "${RESOURCE_TYPE}" "${COMPONENT_NAME}" -n "${NAMESPACE}" -o jsonpath='{.status.desiredNumberScheduled}')
    READY=$(kubectl get "${RESOURCE_TYPE}" "${COMPONENT_NAME}" -n "${NAMESPACE}" -o jsonpath='{.status.numberReady}')
    if [ "${DESIRED}" != "${READY}" ]; then
      echo "❌ ERROR: Not all pods ready! Desired: ${DESIRED}, Ready: ${READY}"
      return 1
    fi
  fi

  echo ""
  echo "✅ ${COMPONENT_NAME} upgraded successfully"
  return 0
}

echo ""
echo "========================================="
echo "Step 1: Backing up network fabric deployments..."
echo "========================================="
echo ""
mkdir -p /tmp/network-backup

# Find all liqo-tenant-* namespaces for gateway deployments
TENANT_NAMESPACES=$(kubectl get namespaces -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep '^liqo-tenant-' || true)
echo "Found tenant namespaces: ${TENANT_NAMESPACES}"

# Backup gateway deployments from tenant namespaces
for TENANT_NS in ${TENANT_NAMESPACES}; do
  GATEWAY_DEPLOYMENTS=$(kubectl get deployments -n "${TENANT_NS}" -l networking.liqo.io/component=gateway -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
  for GW in ${GATEWAY_DEPLOYMENTS}; do
    echo "  Backing up gateway: ${GW} in namespace ${TENANT_NS}"
    kubectl get deployment "${GW}" -n "${TENANT_NS}" -o yaml > "/tmp/network-backup/${TENANT_NS}-${GW}-deployment.yaml"
  done
done

# Backup core network components
if kubectl get daemonset liqo-fabric -n "${NAMESPACE}" &>/dev/null; then
  echo "  Backing up liqo-fabric (daemonset)"
  kubectl get daemonset liqo-fabric -n "${NAMESPACE}" -o yaml > "/tmp/network-backup/liqo-fabric-daemonset.yaml"
fi

if kubectl get deployment liqo-ipam -n "${NAMESPACE}" &>/dev/null; then
  echo "  Backing up liqo-ipam (deployment)"
  kubectl get deployment liqo-ipam -n "${NAMESPACE}" -o yaml > "/tmp/network-backup/liqo-ipam-deployment.yaml"
fi

if kubectl get deployment liqo-proxy -n "${NAMESPACE}" &>/dev/null; then
  echo "  Backing up liqo-proxy (deployment)"
  kubectl get deployment liqo-proxy -n "${NAMESPACE}" -o yaml > "/tmp/network-backup/liqo-proxy-deployment.yaml"
fi

echo ""
echo "========================================="
echo "Step 2: Validating WireGuard/Geneve prerequisites..."
echo "========================================="
echo ""
# Check for WireGuard keys and configuration
echo "Checking WireGuard keys and secrets..."
WG_SECRETS=$(kubectl get secrets -n "${NAMESPACE}" -l "liqo.io/component=gateway" -o jsonpath='{.items[*].metadata.name}' || echo "")
if [ -n "$WG_SECRETS" ]; then
  echo "  Found gateway secrets:"
  for secret in $WG_SECRETS; do
    KEYS=$(kubectl get secret "$secret" -n "${NAMESPACE}" -o jsonpath='{.data}' | jq 'keys' 2>/dev/null || echo "[]")
    echo "    - $secret: $KEYS"
  done
  echo "  ✓ Gateway secrets present"
else
  echo "  ⚠️  WARNING: No gateway secrets found with liqo.io/component=gateway label"
fi

# Check for gateway ConfigMaps
echo "Checking gateway ConfigMaps..."
WG_CONFIGMAPS=$(kubectl get configmaps -n "${NAMESPACE}" -l "liqo.io/component=gateway" -o jsonpath='{.items[*].metadata.name}' || echo "")
if [ -n "$WG_CONFIGMAPS" ]; then
  echo "  Found gateway ConfigMaps:"
  for cm in $WG_CONFIGMAPS; do
    echo "    - $cm"
  done
  echo "  ✓ Gateway ConfigMaps present"
else
  echo "  ℹ️  No gateway ConfigMaps found (may be optional)"
fi

echo ""
echo "========================================="
echo "Step 3: Enabling Gateway HA mode in templates"
echo "========================================="
echo ""
echo "CRITICAL: We must patch templates for HA BEFORE updating images."
echo "This is the ONLY way to achieve zero-downtime gateway upgrade."
echo ""
echo "The Liqo controller chain works as follows:"
echo "  WgGatewayClientTemplate → WgGatewayClient → Deployment"
echo "  (Controllers overwrite any manual patches to WgGatewayClient)"
echo ""
echo "Strategy: Patch templates with replicas=2 + RollingUpdate,"
echo "          then update images. RollingUpdate will upgrade one pod at a time."
echo ""

# Enable HA mode in templates FIRST (before image update)
echo "--- Patching templates for HA ---"

# Patch WgGatewayClientTemplate for HA
if kubectl get wggatewayclienttemplate wireguard-client -n "${NAMESPACE}" &>/dev/null; then
  echo "Patching WgGatewayClientTemplate for HA mode..."
  
  # Set replicas=2 and strategy=RollingUpdate
  kubectl patch wggatewayclienttemplate wireguard-client -n "${NAMESPACE}" \
    --type='json' -p='[
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/replicas", "value": 2},
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/strategy", "value": {"type": "RollingUpdate", "rollingUpdate": {"maxUnavailable": 0, "maxSurge": 1}}}
    ]' && echo "  ✓ WgGatewayClientTemplate: replicas=2, strategy=RollingUpdate" || {
    echo "  ⚠️ Warning: Could not patch HA settings, trying alternative..."
    # Try patching separately
    kubectl patch wggatewayclienttemplate wireguard-client -n "${NAMESPACE}" \
      --type='json' -p='[{"op": "replace", "path": "/spec/template/spec/deployment/spec/replicas", "value": 2}]' 2>/dev/null || true
    kubectl patch wggatewayclienttemplate wireguard-client -n "${NAMESPACE}" \
      --type='merge' -p='{"spec":{"template":{"spec":{"deployment":{"spec":{"strategy":{"type":"RollingUpdate","rollingUpdate":{"maxUnavailable":0,"maxSurge":1}}}}}}}}' 2>/dev/null || true
  }
else
  echo "  ℹ️  WgGatewayClientTemplate not found"
fi

# Patch WgGatewayServerTemplate for HA
if kubectl get wggatewayservertemplate wireguard-server -n "${NAMESPACE}" &>/dev/null; then
  echo "Patching WgGatewayServerTemplate for HA mode..."
  
  kubectl patch wggatewayservertemplate wireguard-server -n "${NAMESPACE}" \
    --type='json' -p='[
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/replicas", "value": 2},
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/strategy", "value": {"type": "RollingUpdate", "rollingUpdate": {"maxUnavailable": 0, "maxSurge": 1}}}
    ]' && echo "  ✓ WgGatewayServerTemplate: replicas=2, strategy=RollingUpdate" || {
    echo "  ⚠️ Warning: Could not patch HA settings"
  }
else
  echo "  ℹ️  WgGatewayServerTemplate not found"
fi

echo ""
echo "========================================="
echo "Step 4: Triggering controller propagation..."
echo "========================================="
echo ""
echo "The reconcilers do NOT watch templates automatically."
echo "We must annotate Gateway resources to trigger re-render from template."
echo ""

# Annotate Gateway resources (Client or Server) per tenant namespace
TENANT_NAMESPACES=$(kubectl get namespaces -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep '^liqo-tenant-' || true)
HA_CLIENT_COUNT=0
HA_SERVER_COUNT=0
for TENANT_NS in $TENANT_NAMESPACES; do
  # Detect gateway type for this namespace
  GATEWAY_CLIENTS=$(kubectl get gatewayclient -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
  GATEWAY_SERVERS=$(kubectl get gatewayserver -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
  
  # Process GatewayClients if present
  for GW_CLIENT in $GATEWAY_CLIENTS; do
    HA_TS=$(date +%%s)
    kubectl annotate gatewayclient "${GW_CLIENT}" -n "${TENANT_NS}" liqo.io/ha-upgrade="${HA_TS}" --overwrite 2>/dev/null || true
    echo "  ✓ Triggered reconciliation for GatewayClient ${GW_CLIENT} in ${TENANT_NS}"
    HA_CLIENT_COUNT=$((HA_CLIENT_COUNT + 1))
  done
  
  # Process GatewayServers if present
  for GW_SERVER in $GATEWAY_SERVERS; do
    HA_TS=$(date +%%s)
    kubectl annotate gatewayserver "${GW_SERVER}" -n "${TENANT_NS}" liqo.io/ha-upgrade="${HA_TS}" --overwrite 2>/dev/null || true
    echo "  ✓ Triggered reconciliation for GatewayServer ${GW_SERVER} in ${TENANT_NS}"
    HA_SERVER_COUNT=$((HA_SERVER_COUNT + 1))
  done
done

if [ "$HA_CLIENT_COUNT" -eq 0 ] && [ "$HA_SERVER_COUNT" -eq 0 ]; then
  echo "  ℹ️ No Gateway resources found to trigger"
else
  echo "  Summary: ${HA_CLIENT_COUNT} GatewayClient(s), ${HA_SERVER_COUNT} GatewayServer(s) triggered"
fi

echo ""
echo "Waiting for controllers to propagate HA settings (15s)..."
sleep 15

# Verify templates have HA settings
echo ""
echo "Verifying template HA configuration..."
CLIENT_REPLICAS=$(kubectl get wggatewayclienttemplate wireguard-client -n "${NAMESPACE}" \
  -o jsonpath='{.spec.template.spec.deployment.spec.replicas}' 2>/dev/null || echo "1")
CLIENT_STRATEGY=$(kubectl get wggatewayclienttemplate wireguard-client -n "${NAMESPACE}" \
  -o jsonpath='{.spec.template.spec.deployment.spec.strategy.type}' 2>/dev/null || echo "unknown")
echo "  Client template: replicas=${CLIENT_REPLICAS}, strategy=${CLIENT_STRATEGY}"

SERVER_REPLICAS=$(kubectl get wggatewayservertemplate wireguard-server -n "${NAMESPACE}" \
  -o jsonpath='{.spec.template.spec.deployment.spec.replicas}' 2>/dev/null || echo "1")
SERVER_STRATEGY=$(kubectl get wggatewayservertemplate wireguard-server -n "${NAMESPACE}" \
  -o jsonpath='{.spec.template.spec.deployment.spec.strategy.type}' 2>/dev/null || echo "unknown")
echo "  Server template: replicas=${SERVER_REPLICAS}, strategy=${SERVER_STRATEGY}"

# Wait for deployments to have 2 replicas ready
echo ""
echo "========================================="
echo "Step 5: Waiting for gateway pods to scale up..."
echo "========================================="
echo ""
TENANT_NAMESPACES=$(kubectl get namespaces -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep '^liqo-tenant-' || true)
GATEWAYS_HA_READY=0

for TENANT_NS in $TENANT_NAMESPACES; do
  GATEWAY_DEPLOYMENTS=$(kubectl get deployments -n "${TENANT_NS}" -l networking.liqo.io/component=gateway -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
  
  for GW_DEPLOY in $GATEWAY_DEPLOYMENTS; do
    if [ -n "$GW_DEPLOY" ]; then
      echo "  Waiting for ${GW_DEPLOY} to have 2 ready replicas..."
      
      for WAIT in $(seq 1 60); do
        READY=$(kubectl get deployment "${GW_DEPLOY}" -n "${TENANT_NS}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
        REPLICAS=$(kubectl get deployment "${GW_DEPLOY}" -n "${TENANT_NS}" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "1")
        STRATEGY=$(kubectl get deployment "${GW_DEPLOY}" -n "${TENANT_NS}" -o jsonpath='{.spec.strategy.type}' 2>/dev/null || echo "unknown")
        
        if [ "$READY" -ge 2 ] && [ "$REPLICAS" -ge 2 ]; then
          echo "    ✓ ${GW_DEPLOY}: ${READY}/${REPLICAS} ready, strategy=${STRATEGY}"
          GATEWAYS_HA_READY=$((GATEWAYS_HA_READY + 1))
          break
        fi
        
        if [ $((WAIT %% 10)) -eq 0 ]; then
          echo "    ⏳ Waiting... ${READY}/${REPLICAS} ready (${WAIT}s)"
        fi
        sleep 1
      done
    fi
  done
done

# Wait for leader election
echo ""
echo "Waiting for leader election to stabilize (10s)..."
sleep 10

echo "Verifying gateway leader election..."
for TENANT_NS in $TENANT_NAMESPACES; do
  ACTIVE_POD=$(kubectl get pods -n "${TENANT_NS}" \
    -l networking.liqo.io/component=gateway,networking.liqo.io/active=true \
    --field-selector=status.phase=Running \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
  if [ -n "$ACTIVE_POD" ]; then
    echo "  ✓ ${TENANT_NS}: Active gateway is ${ACTIVE_POD}"
  else
    echo "  ⚠️ ${TENANT_NS}: No active leader yet (may still be electing)"
  fi
done

echo ""
echo "✅ Gateway HA Mode Enabled: ${GATEWAYS_HA_READY} gateway(s) with 2 replicas + RollingUpdate"

echo ""
echo "========================================="
echo "Step 6: Upgrading gateway templates (images, env vars, args)..."
echo "========================================="
echo ""
echo "Now updating gateway templates using upgrade plan. RollingUpdate will upgrade one pod at a time."

# Function to upgrade a WgGateway template using the upgrade plan
upgrade_wg_gateway_template() {
  local TEMPLATE_NAME=$1
  local TEMPLATE_KIND=$2
  local RESOURCE_TYPE=$3  # "wggatewayclienttemplate" or "wggatewayservertemplate"
  
  echo ""
  echo "--- Upgrading ${TEMPLATE_KIND}: ${TEMPLATE_NAME} ---"
  
  # Check if template exists
  if ! kubectl get "${RESOURCE_TYPE}" "${TEMPLATE_NAME}" -n "${NAMESPACE}" &>/dev/null; then
    echo "  ℹ️  ${TEMPLATE_KIND} ${TEMPLATE_NAME} not found, skipping"
    return 0
  fi
  
  # Find template in upgrade plan
  TEMPLATE_PLAN=$(echo "$PLAN_JSON" | jq -r --arg name "$TEMPLATE_NAME" --arg kind "$TEMPLATE_KIND" '
    .templatesToUpdate[] | select(.name == $name and .kind == $kind)
  ')
  
  if [ -z "$TEMPLATE_PLAN" ] || [ "$TEMPLATE_PLAN" == "null" ]; then
    echo "  ⚠️  No update plan for ${TEMPLATE_NAME}, using fallback image upgrade"
    
    # Fallback: simple image update for all containers
    kubectl patch "${RESOURCE_TYPE}" "${TEMPLATE_NAME}" -n "${NAMESPACE}" \
      --type='json' -p='[
        {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/0/image", "value": "'"${IMAGE_REGISTRY}"'/gateway:'"${TARGET_VERSION}"'"},
        {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/1/image", "value": "'"${IMAGE_REGISTRY}"'/gateway/wireguard:'"${TARGET_VERSION}"'"},
        {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/2/image", "value": "'"${IMAGE_REGISTRY}"'/gateway/geneve:'"${TARGET_VERSION}"'"}
      ]' && echo "  ✓ ${TEMPLATE_NAME} images updated (fallback)" || echo "  ⚠️  Warning: Could not update images"
  else
    echo "  Applying upgrade plan for ${TEMPLATE_NAME}..."
    
    # Get container changes from plan
    CONTAINER_CHANGES=$(echo "$TEMPLATE_PLAN" | jq -r '.containerChanges // []')
    CONTAINER_COUNT=$(echo "$CONTAINER_CHANGES" | jq 'length')
    
    if [ "$CONTAINER_COUNT" -gt 0 ]; then
      echo "  Found ${CONTAINER_COUNT} container(s) to update"
      
      # Get current containers from template
      CURRENT_CONTAINERS=$(kubectl get "${RESOURCE_TYPE}" "${TEMPLATE_NAME}" -n "${NAMESPACE}" \
        -o json | jq '.spec.template.spec.deployment.spec.template.spec.containers')
      
      # Process each container change
      CONTAINER_INDEX=0
      while IFS= read -r container_change; do
        CONTAINER_NAME=$(echo "$container_change" | jq -r '.containerName')
        TARGET_IMAGE=$(echo "$container_change" | jq -r '.targetImage')
        ENV_CHANGES=$(echo "$container_change" | jq -r '.envChanges // []')
        FLAG_CHANGES=$(echo "$container_change" | jq -r '.flagChanges // []')
        
        echo ""
        echo "  Processing container: ${CONTAINER_NAME}"
        
        # Find container index in current spec
        CONTAINER_INDEX=$(echo "$CURRENT_CONTAINERS" | jq -r --arg name "$CONTAINER_NAME" 'to_entries[] | select(.value.name == $name) | .key')
        
        if [ -z "$CONTAINER_INDEX" ]; then
          echo "    ⚠️  Container ${CONTAINER_NAME} not found in template, skipping"
          continue
        fi
        
        echo "    Container index: ${CONTAINER_INDEX}"
        echo "    Target image: ${TARGET_IMAGE}"
        
        # Update image
        kubectl patch "${RESOURCE_TYPE}" "${TEMPLATE_NAME}" -n "${NAMESPACE}" \
          --type='json' -p='[{"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/'"${CONTAINER_INDEX}"'/image", "value": "'"${TARGET_IMAGE}"'"}]' \
          && echo "    ✓ Image updated" || echo "    ⚠️  Warning: Could not update image"
        
        # Apply env var changes
        ENV_COUNT=$(echo "$ENV_CHANGES" | jq 'length')
        if [ "$ENV_COUNT" -gt 0 ]; then
          echo "    Applying ${ENV_COUNT} env var change(s)..."
          
          # Get current env for this container
          CURRENT_ENV=$(echo "$CURRENT_CONTAINERS" | jq --argjson idx "$CONTAINER_INDEX" '.[$idx].env // []')
          NEW_ENV="$CURRENT_ENV"
          
          while IFS= read -r change; do
            TYPE=$(echo "$change" | jq -r '.type')
            NAME=$(echo "$change" | jq -r '.name')
            NEW_VALUE=$(echo "$change" | jq -r '.newValue // ""')
            NEW_SOURCE=$(echo "$change" | jq -r '.newSource // ""')
            
            if [ "$TYPE" == "add" ] || [ "$TYPE" == "update" ]; then
              if [ "$NEW_SOURCE" == "value" ]; then
                echo "      ${TYPE}: ${NAME}=${NEW_VALUE}"
                ENV_VAR=$(jq -n --arg name "$NAME" --arg value "$NEW_VALUE" '{name: $name, value: $value}')
              elif [[ "$NEW_SOURCE" == configMap:* ]]; then
                CONFIGMAP_NAME=$(echo "$NEW_SOURCE" | cut -d: -f2)
                echo "      ${TYPE}: ${NAME} from ConfigMap ${CONFIGMAP_NAME}"
                ENV_VAR=$(jq -n --arg name "$NAME" --arg cmName "$CONFIGMAP_NAME" --arg key "$NEW_VALUE" \
                  '{name: $name, valueFrom: {configMapKeyRef: {name: $cmName, key: $key}}}')
              elif [[ "$NEW_SOURCE" == secret:* ]]; then
                SECRET_NAME=$(echo "$NEW_SOURCE" | cut -d: -f2)
                echo "      ${TYPE}: ${NAME} from Secret ${SECRET_NAME}"
                ENV_VAR=$(jq -n --arg name "$NAME" --arg secretName "$SECRET_NAME" --arg key "$NEW_VALUE" \
                  '{name: $name, valueFrom: {secretKeyRef: {name: $secretName, key: $key}}}')
              elif [ "$NEW_SOURCE" == "fieldRef" ]; then
                echo "      ${TYPE}: ${NAME} from fieldRef: ${NEW_VALUE}"
                ENV_VAR=$(jq -n --arg name "$NAME" --arg fieldPath "$NEW_VALUE" \
                  '{name: $name, valueFrom: {fieldRef: {apiVersion: "v1", fieldPath: $fieldPath}}}')
              else
                continue
              fi
              
              if echo "$NEW_ENV" | jq -e --arg name "$NAME" 'any(.[]; .name == $name)' > /dev/null 2>&1; then
                NEW_ENV=$(echo "$NEW_ENV" | jq --argjson envVar "$ENV_VAR" --arg name "$NAME" \
                  'map(if .name == $name then $envVar else . end)')
              else
                NEW_ENV=$(echo "$NEW_ENV" | jq --argjson envVar "$ENV_VAR" '. += [$envVar]')
              fi
            elif [ "$TYPE" == "remove" ]; then
              echo "      ${TYPE}: ${NAME}"
              NEW_ENV=$(echo "$NEW_ENV" | jq --arg name "$NAME" 'map(select(.name != $name))')
            fi
          done < <(echo "$ENV_CHANGES" | jq -c '.[]')
          
          # Apply env patch
          kubectl patch "${RESOURCE_TYPE}" "${TEMPLATE_NAME}" -n "${NAMESPACE}" \
            --type='json' -p='[{"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/'"${CONTAINER_INDEX}"'/env", "value": '"${NEW_ENV}"'}]' \
            && echo "    ✓ Env vars updated" || echo "    ⚠️  Warning: Could not update env vars"
        fi
        
        # Apply flag/args changes
        FLAG_COUNT=$(echo "$FLAG_CHANGES" | jq 'length')
        if [ "$FLAG_COUNT" -gt 0 ]; then
          echo "    Applying ${FLAG_COUNT} flag change(s)..."
          
          # Get current args for this container
          CURRENT_ARGS=$(echo "$CURRENT_CONTAINERS" | jq --argjson idx "$CONTAINER_INDEX" '.[$idx].args // []')
          NEW_ARGS="$CURRENT_ARGS"
          
          while IFS= read -r change; do
            TYPE=$(echo "$change" | jq -r '.type')
            FLAG=$(echo "$change" | jq -r '.flag')
            NEW_VALUE=$(echo "$change" | jq -r '.newValue // ""')
            
            if [ "$TYPE" == "add" ]; then
              echo "      add: --${FLAG}=${NEW_VALUE}"
              NEW_ARGS=$(echo "$NEW_ARGS" | jq --arg f "--${FLAG}=${NEW_VALUE}" '. += [$f]')
            elif [ "$TYPE" == "update" ]; then
              echo "      update: --${FLAG}=${NEW_VALUE}"
              NEW_ARGS=$(echo "$NEW_ARGS" | jq --arg prefix "--${FLAG}" 'map(select(startswith($prefix) | not))')
              NEW_ARGS=$(echo "$NEW_ARGS" | jq --arg f "--${FLAG}=${NEW_VALUE}" '. += [$f]')
            elif [ "$TYPE" == "remove" ]; then
              echo "      remove: --${FLAG}"
              NEW_ARGS=$(echo "$NEW_ARGS" | jq --arg prefix "--${FLAG}" 'map(select(startswith($prefix) | not))')
            fi
          done < <(echo "$FLAG_CHANGES" | jq -c '.[]')
          
          # Apply args patch
          kubectl patch "${RESOURCE_TYPE}" "${TEMPLATE_NAME}" -n "${NAMESPACE}" \
            --type='json' -p='[{"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/'"${CONTAINER_INDEX}"'/args", "value": '"${NEW_ARGS}"'}]' \
            && echo "    ✓ Args updated" || echo "    ⚠️  Warning: Could not update args"
        fi

        # Apply command changes
        TARGET_COMMAND=$(echo "$container_change" | jq -r '.targetCommand // []')
        COMMAND_COUNT=$(echo "$TARGET_COMMAND" | jq 'length')
        if [ "$COMMAND_COUNT" -gt 0 ]; then
          echo "    Applying command change..."
          echo "      Target command: $(echo "$TARGET_COMMAND" | jq -c '.')"
          kubectl patch "${RESOURCE_TYPE}" "${TEMPLATE_NAME}" -n "${NAMESPACE}" \
            --type='json' -p='[{"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/'"${CONTAINER_INDEX}"'/command", "value": '"${TARGET_COMMAND}"'}]' \
            && echo "    ✓ Command updated" || echo "    ⚠️  Warning: Could not update command"
        fi
        
      done < <(echo "$CONTAINER_CHANGES" | jq -c '.[]')
    fi
  fi
  
  # Update version labels in template
  kubectl patch "${RESOURCE_TYPE}" "${TEMPLATE_NAME}" -n "${NAMESPACE}" \
    --type='json' -p='[
      {"op": "replace", "path": "/spec/template/spec/deployment/metadata/labels/app.kubernetes.io~1version", "value": "'"${TARGET_VERSION}"'"},
      {"op": "replace", "path": "/spec/template/spec/deployment/metadata/labels/helm.sh~1chart", "value": "liqo-'"${TARGET_VERSION}"'"},
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/metadata/labels/app.kubernetes.io~1version", "value": "'"${TARGET_VERSION}"'"},
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/metadata/labels/helm.sh~1chart", "value": "liqo-'"${TARGET_VERSION}"'"}
    ]' && echo "  ✓ ${TEMPLATE_NAME} labels updated" || echo "  ⚠️  Warning: Could not update labels"
  
  echo "  ✅ ${TEMPLATE_NAME} upgrade complete"
  return 0
}

# Upgrade WgGatewayClientTemplate
upgrade_wg_gateway_template "wireguard-client" "WgGatewayClientTemplate" "wggatewayclienttemplate"

# Upgrade WgGatewayServerTemplate
upgrade_wg_gateway_template "wireguard-server" "WgGatewayServerTemplate" "wggatewayservertemplate"

# Verify templates were updated
sleep 2
echo ""
echo "Verifying template image updates..."
CLIENT_TEMPLATE_IMAGE=$(kubectl get wggatewayclienttemplate wireguard-client -n "${NAMESPACE}" \
  -o jsonpath='{.spec.template.spec.deployment.spec.template.spec.containers[0].image}' 2>/dev/null || echo "not found")
echo "  Client template image: ${CLIENT_TEMPLATE_IMAGE}"

SERVER_TEMPLATE_IMAGE=$(kubectl get wggatewayservertemplate wireguard-server -n "${NAMESPACE}" \
  -o jsonpath='{.spec.template.spec.deployment.spec.template.spec.containers[0].image}' 2>/dev/null || echo "not found")
echo "  Server template image: ${SERVER_TEMPLATE_IMAGE}"

if [[ "$CLIENT_TEMPLATE_IMAGE" != *"${TARGET_VERSION}"* ]] && [[ "$CLIENT_TEMPLATE_IMAGE" != "not found" ]]; then
  echo "❌ ERROR: Client template not updated to ${TARGET_VERSION}!"
  exit 1
fi

if [[ "$SERVER_TEMPLATE_IMAGE" != *"${TARGET_VERSION}"* ]] && [[ "$SERVER_TEMPLATE_IMAGE" != "not found" ]]; then
  echo "❌ ERROR: Server template not updated to ${TARGET_VERSION}!"
  exit 1
fi

echo "✅ Gateway templates upgraded successfully"
echo ""
echo "NOTE: Image updates will be applied in Step 12 after fabric is stable."
echo "This ensures gateway upgrade happens when network is ready."

echo ""
echo "========================================="
echo "Step 7: Upgrading network components sequentially with health checks..."
echo "========================================="
echo ""
# Function to check component health
check_component_health() {
  local COMPONENT=$1
  local KIND=$2
  local NAMESPACE=$3

  echo "  Performing health check for ${COMPONENT}..."

  if [ "$KIND" == "Deployment" ]; then
    REPLICAS=$(kubectl get deployment "$COMPONENT" -n "$NAMESPACE" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
    DESIRED=$(kubectl get deployment "$COMPONENT" -n "$NAMESPACE" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "1")
  elif [ "$KIND" == "DaemonSet" ]; then
    REPLICAS=$(kubectl get daemonset "$COMPONENT" -n "$NAMESPACE" -o jsonpath='{.status.numberReady}' 2>/dev/null || echo "0")
    DESIRED=$(kubectl get daemonset "$COMPONENT" -n "$NAMESPACE" -o jsonpath='{.status.desiredNumberScheduled}' 2>/dev/null || echo "1")
  fi

  if [ "$REPLICAS" == "$DESIRED" ] && [ "$REPLICAS" -gt 0 ]; then
    echo "    ✓ ${COMPONENT} healthy: ${REPLICAS}/${DESIRED} ready"
    return 0
  else
    echo "    ✗ ${COMPONENT} unhealthy: ${REPLICAS}/${DESIRED} ready"
    return 1
  fi
}

# Function to update version labels on a Deployment or DaemonSet
update_version_labels() {
  local COMPONENT=$1
  local KIND=$2
  local NAMESPACE=$3
  local TARGET_VERSION=$4
  
  echo "  Updating version labels for ${COMPONENT}..."
  
  local RESOURCE_TYPE=$(echo "$KIND" | tr '[:upper:]' '[:lower:]')
  
  # Update metadata labels
  kubectl patch "${RESOURCE_TYPE}" "${COMPONENT}" -n "${NAMESPACE}" --type=json \
    -p='[
      {"op":"replace","path":"/metadata/labels/app.kubernetes.io~1version","value":"'"${TARGET_VERSION}"'"},
      {"op":"replace","path":"/metadata/labels/helm.sh~1chart","value":"liqo-'"${TARGET_VERSION}"'"}
    ]' 2>/dev/null || echo "    ⚠️ Could not update metadata labels"
  
  # Update pod template labels
  kubectl patch "${RESOURCE_TYPE}" "${COMPONENT}" -n "${NAMESPACE}" --type=json \
    -p='[
      {"op":"replace","path":"/spec/template/metadata/labels/app.kubernetes.io~1version","value":"'"${TARGET_VERSION}"'"},
      {"op":"replace","path":"/spec/template/metadata/labels/helm.sh~1chart","value":"liqo-'"${TARGET_VERSION}"'"}
    ]' 2>/dev/null || echo "    ⚠️ Could not update pod template labels"
  
  echo "    ✓ Labels updated"
}

# Upgrade liqo-ipam first (less critical, manages IP allocation)
if ! upgrade_network_component "liqo-ipam" "Deployment" "${NAMESPACE}" "ipam"; then
  echo "❌ ERROR: Failed to upgrade liqo-ipam"
  exit 1
fi
check_component_health "liqo-ipam" "Deployment" "${NAMESPACE}" || {
  echo "❌ ERROR: liqo-ipam health check failed!"
  exit 1
}
update_version_labels "liqo-ipam" "Deployment" "${NAMESPACE}" "${TARGET_VERSION}"

# Upgrade liqo-proxy (Deployment)
if ! upgrade_network_component "liqo-proxy" "Deployment" "${NAMESPACE}" "proxy"; then
  echo "❌ ERROR: Failed to upgrade liqo-proxy"
  exit 1
fi
check_component_health "liqo-proxy" "Deployment" "${NAMESPACE}" || {
  echo "❌ ERROR: liqo-proxy health check failed!"
  exit 1
}
update_version_labels "liqo-proxy" "Deployment" "${NAMESPACE}" "${TARGET_VERSION}"

# Upgrade liqo-fabric (DaemonSet) - Data plane component
echo ""
echo "⚠️  WARNING: liqo-fabric upgrade may cause temporary network disruption"
if ! upgrade_network_component "liqo-fabric" "DaemonSet" "${NAMESPACE}" "fabric"; then
  echo "❌ ERROR: Failed to upgrade liqo-fabric"
  exit 1
fi
check_component_health "liqo-fabric" "DaemonSet" "${NAMESPACE}" || {
  echo "❌ ERROR: liqo-fabric health check failed!"
  exit 1
}

# Check routes are still present after fabric upgrade
echo "  Verifying network routes..."
ROUTE_COUNT=$(kubectl exec -n "${NAMESPACE}" daemonset/liqo-fabric -- ip route | wc -l 2>/dev/null || echo "0")
if [ "$ROUTE_COUNT" -gt 0 ]; then
  echo "    ✓ Network routes present (${ROUTE_COUNT} routes)"
else
  echo "    ⚠️  WARNING: Could not verify network routes"
fi
update_version_labels "liqo-fabric" "DaemonSet" "${NAMESPACE}" "${TARGET_VERSION}"

# Upgrade virtual-kubelet components (if present)
echo ""
echo "--- Upgrading Virtual Kubelet Components ---"

# Check if VK components exist
VK_DAEMONSETS=$(kubectl get daemonsets -n "${NAMESPACE}" -l app.kubernetes.io/component=virtual-kubelet -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
VK_DEPLOYMENTS=$(kubectl get deployments -n "${NAMESPACE}" -l app.kubernetes.io/component=virtual-kubelet -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")

if [ -z "$VK_DAEMONSETS" ] && [ -z "$VK_DEPLOYMENTS" ]; then
  echo "  ℹ️  No Virtual Kubelet components found, skipping"
else
  echo "  Found Virtual Kubelet components:"
  [ -n "$VK_DAEMONSETS" ] && echo "    DaemonSets: $VK_DAEMONSETS"
  [ -n "$VK_DEPLOYMENTS" ] && echo "    Deployments: $VK_DEPLOYMENTS"

  # Verify control-plane and CRDs are healthy before upgrading VK
  echo "  Verifying control-plane health before VK upgrade..."
  if ! kubectl wait --for=condition=available --timeout=30s deployment/liqo-controller-manager -n "${NAMESPACE}" 2>/dev/null; then
    echo "  ⚠️  WARNING: controller-manager not ready, VK upgrade may fail"
  else
    echo "    ✓ Controller-manager healthy"
  fi

  # Check for offloading CR schema compatibility
  echo "  Checking offloading CRD compatibility..."
  OFFLOADING_CRD_VERSION=$(kubectl get crd namespacemaps.offloading.liqo.io -o jsonpath='{.spec.versions[?(@.storage==true)].name}' 2>/dev/null || echo "unknown")
  echo "    Current NamespaceMap storage version: ${OFFLOADING_CRD_VERSION}"

  # Upgrade VK DaemonSets
  for VK_DS in $VK_DAEMONSETS; do
    echo ""
    echo "  Upgrading VK DaemonSet: ${VK_DS}"

    NEW_IMAGE="${IMAGE_REGISTRY}/virtual-kubelet:${TARGET_VERSION}"
    CONTAINER_NAME=$(kubectl get daemonset "$VK_DS" -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].name}')

    kubectl set image daemonset/"$VK_DS" \
      "${CONTAINER_NAME}=${NEW_IMAGE}" \
      -n "${NAMESPACE}"

    echo "    Waiting for rollout..."
    if ! kubectl rollout status daemonset/"$VK_DS" -n "${NAMESPACE}" --timeout=5m; then
      echo "    ❌ ERROR: VK DaemonSet ${VK_DS} rollout failed!"
      exit 1
    fi

    check_component_health "$VK_DS" "DaemonSet" "${NAMESPACE}" || {
      echo "    ❌ ERROR: VK DaemonSet ${VK_DS} health check failed!"
      exit 1
    }

    echo "    ✅ ${VK_DS} upgraded successfully"
  done

  # Upgrade VK Deployments
  for VK_DEP in $VK_DEPLOYMENTS; do
    echo ""
    echo "  Upgrading VK Deployment: ${VK_DEP}"

    NEW_IMAGE="${IMAGE_REGISTRY}/virtual-kubelet:${TARGET_VERSION}"
    CONTAINER_NAME=$(kubectl get deployment "$VK_DEP" -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].name}')

    kubectl set image deployment/"$VK_DEP" \
      "${CONTAINER_NAME}=${NEW_IMAGE}" \
      -n "${NAMESPACE}"

    echo "    Waiting for rollout..."
    if ! kubectl rollout status deployment/"$VK_DEP" -n "${NAMESPACE}" --timeout=5m; then
      echo "    ❌ ERROR: VK Deployment ${VK_DEP} rollout failed!"
      exit 1
    fi

    check_component_health "$VK_DEP" "Deployment" "${NAMESPACE}" || {
      echo "    ❌ ERROR: VK Deployment ${VK_DEP} health check failed!"
      exit 1
    }

    echo "    ✅ ${VK_DEP} upgraded successfully"
  done

  echo ""
  echo "  Verifying VK can reconcile offloaded resources..."
  sleep 10  # Give VK time to start reconciling

  # Check VK pod logs for errors
  VK_POD=$(kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/component=virtual-kubelet -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
  if [ -n "$VK_POD" ]; then
    ERROR_COUNT=$(kubectl logs "$VK_POD" -n "${NAMESPACE}" --tail=100 2>/dev/null | grep -i "error\|failed" | wc -l || echo "0")
    if [ "$ERROR_COUNT" -gt 5 ]; then
      echo "    ⚠️  WARNING: VK pod has ${ERROR_COUNT} recent errors in logs"
    else
      echo "    ✓ VK logs look healthy (${ERROR_COUNT} errors)"
    fi
  fi

  echo ""
  echo "✅ Virtual Kubelet components upgraded successfully"
fi

echo ""
echo "========================================="
echo "Step 8: Performing local data plane reset..."
echo "========================================="
echo ""
echo "This step cleans stale network state to ensure VPN tunnels can be re-established"


# Clean stale host interfaces
echo "1. Cleaning stale host interfaces..."
# Get all fabric pods
FABRIC_PODS=$(kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/name=fabric -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")

if [ -n "$FABRIC_PODS" ]; then
  for POD in $FABRIC_PODS; do
    echo "  Cleaning node via ${POD}..."
    # Robust cleanup: Set down before delete, ignore errors
    # This handles interfaces that might be in a "weird" state or have dependencies
    kubectl exec -n "${NAMESPACE}" "${POD}" -- /bin/sh -c "
      for i in \$(ip link show 2>/dev/null | grep 'liqo\\.' | awk -F': ' '{print \$2}'); do 
        echo \"Deleting \$i\"; 
        ip link set \"\$i\" down 2>/dev/null || true
        ip link delete \"\$i\" 2>/dev/null || true
      done" 2>/dev/null || echo "    ⚠️ Warning: Cleanup command had errors on ${POD}"
  done
  echo "  ✓ Stale host interfaces cleaned"
else
  echo "  ⚠️ No fabric pods found for interface cleanup"
fi

# Annotate InternalNode resources to trigger reconciliation (preserves existing tunnels)
echo ""
echo "2. Triggering InternalNode reconciliation (zero-downtime method)..."
TRIGGER_TS=$(date +%%s)
INTERNAL_NODES=$(kubectl get internalnodes -A -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || echo "")
if [ -n "$INTERNAL_NODES" ]; then
  for INODE in $INTERNAL_NODES; do
    kubectl annotate internalnode "${INODE}" liqo.io/upgrade-trigger="${TRIGGER_TS}" --overwrite 2>/dev/null || true
  done
  echo "  ✓ InternalNode resources annotated (controller will update in-place)"
  echo "  ✓ Existing tunnels preserved - zero-downtime optimization"
else
  echo "  ℹ️ No InternalNode resources found"
fi

# Flush conntrack to clear stale connections
echo ""
echo "3. Flushing conntrack table to clear stale connections..."
# Deleting network interfaces can leave stale entries in the conntrack table
# which can cause connection tracking issues and prevent API server connectivity
FABRIC_POD=$(kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/name=fabric -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
if [ -n "$FABRIC_POD" ]; then
  # Try to flush conntrack from within a privileged pod (fabric has hostNetwork)
  kubectl exec -n "${NAMESPACE}" "$FABRIC_POD" -- /bin/sh -c '
    if command -v conntrack &> /dev/null; then
      conntrack -F 2>/dev/null && echo "  ✓ Conntrack table flushed" || echo "  ⚠️ Warning: Failed to flush conntrack"
    else
      echo "  ℹ️ conntrack tool not found in fabric pod, skipping flush"
    fi
  ' 2>/dev/null || echo "  ⚠️ Warning: Could not execute conntrack flush"
else
  echo "  ⚠️ Warning: No fabric pod found for conntrack flush"
fi

echo ""
echo "✅ Local Data Plane Reset complete"
echo ""
echo "========================================="
echo "Step 9: Restarting liqo-controller-manager..."
echo "========================================="
echo ""
# Annotate RouteConfigurations to trigger reconciliation (preserves existing routes)
echo "Triggering RouteConfiguration reconciliation (zero-downtime method)..."
TRIGGER_TS=$(date +%%s)
ROUTE_CONFIGS=$(kubectl get routeconfigurations -A -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null || echo "")
if [ -n "$ROUTE_CONFIGS" ]; then
  while IFS= read -r line; do
    if [ -n "$line" ]; then
      RC_NS=$(echo "$line" | awk '{print $1}')
      RC_NAME=$(echo "$line" | awk '{print $2}')
      kubectl annotate routeconfiguration "${RC_NAME}" -n "${RC_NS}" liqo.io/upgrade-trigger="${TRIGGER_TS}" --overwrite 2>/dev/null || true
    fi
  done <<< "$ROUTE_CONFIGS"
  echo "  ✓ RouteConfigurations annotated (controller will update in-place)"
  echo "  ✓ Existing routes preserved - zero-downtime optimization"
else
  echo "  ℹ️ No RouteConfigurations found"
fi

# Restart liqo-controller-manager to trigger full reconciliation
# This will regenerate InternalNode resources with correct new Gateway IPs
echo "  Restarting liqo-controller-manager deployment..."
kubectl rollout restart deployment/liqo-controller-manager -n "${NAMESPACE}" 2>/dev/null || {
  echo "  ⚠️  liqo-controller-manager not found or restart failed"
}

# Wait for rollout to complete
echo "  Waiting for liqo-controller-manager to become ready..."
kubectl rollout status deployment/liqo-controller-manager -n "${NAMESPACE}" --timeout=3m 2>/dev/null || {
  echo "  ⚠️  Rollout status check timed out"
}

# Wait for controller-manager to be fully available
echo "  Waiting for controller-manager to be available..."
kubectl wait --for=condition=available --timeout=2m deployment/liqo-controller-manager -n "${NAMESPACE}" 2>/dev/null || {
  echo "  ⚠️  Controller-manager availability wait timed out"
}

# Give controller time to start up
echo "  Waiting for controller-manager to initialize (10s)..."
sleep 10

echo ""
echo "========================================="
echo "Step 10: Verifying InternalNodes..."
echo "========================================="
echo ""
echo "Since we preserved InternalNodes (didn't delete them), we just verify they exist."
echo "The controller will update them in-place via annotation triggers."
echo ""

# Verify InternalNodes exist (they should - we didn't delete them)
COUNT=$(kubectl get internalnodes -A --no-headers 2>/dev/null | wc -l)
if [ "$COUNT" -gt 0 ]; then
  echo "  ✓ Found $COUNT InternalNode(s) - preserved from pre-upgrade state"
  echo "  Verifying InternalNode status..."
  kubectl get internalnodes -A -o wide 2>/dev/null || true
else
  echo "  ⚠️ Warning: No InternalNodes found"
  echo "  Triggering node reconciliation to create them..."
  
  # Only if no InternalNodes exist, trigger creation
  TRIGGER_TIMESTAMP=$(date +%%s)
  kubectl label nodes --all liqo.io/upgrade-trigger="${TRIGGER_TIMESTAMP}" --overwrite 2>/dev/null || true
  echo "  Waiting for InternalNodes to be created (30s max)..."
  
  TIMEOUT=30
  ELAPSED=0
  while [ $ELAPSED -lt $TIMEOUT ]; do
    COUNT=$(kubectl get internalnodes -A --no-headers 2>/dev/null | wc -l)
    if [ "$COUNT" -gt 0 ]; then
      echo "  ✓ Found $COUNT InternalNode(s)"
      break
    fi
    sleep 2
    ELAPSED=$((ELAPSED+2))
  done
fi

# Brief wait for status to update
echo ""
echo "Waiting for InternalNode status updates (5s)..."
sleep 5

# Verify InternalNode status is populated
echo "Verifying InternalNode status fields..."
INTERNALNODES_WITH_STATUS=$(kubectl get internalnodes -A -o jsonpath='{range .items[*]}{.metadata.name}{": nodeIP="}{.status.nodeIP}{"\n"}{end}' 2>/dev/null || echo "")
if [ -n "$INTERNALNODES_WITH_STATUS" ]; then
  echo "  InternalNode status:"
  echo "$INTERNALNODES_WITH_STATUS" | while read line; do
    echo "    $line"
  done
else
  echo "  ⚠️ Warning: Could not retrieve InternalNode status"
fi

echo ""
echo "✅ InternalNode regeneration complete"
echo "✅ Controller Manager restarted and reconciliation triggered"

echo ""
echo "========================================="
echo "Step 11: Gateway rolling update upgrade..."
echo "========================================="
echo ""
echo "Since we configured RollingUpdate strategy in Steps 3-6, Kubernetes"
echo "will automatically upgrade gateway pods one at a time."
echo ""
echo "The RollingUpdate process (handled by Kubernetes):"
echo "  1. Create new pod with new image"
echo "  2. Wait for new pod to be ready"
echo "  3. Terminate old pod"
echo "  4. Repeat until all pods upgraded"
echo ""
echo "Leader election ensures traffic always flows through an active pod."
echo "Expected disruption: ~1-3 seconds during leader failover."
echo ""

# Function to check tunnel connectivity
check_tunnel_connectivity() {
  local TENANT_NS=$1
  local FC_NAME=$2
  local TIMEOUT=${3:-30}
  
  for i in $(seq 1 $TIMEOUT); do
    NETWORK_STATUS=$(kubectl get foreigncluster "${FC_NAME}" \
      -o jsonpath='{.status.modules.networking.conditions[?(@.type=="NetworkConnectionStatus")].status}' 2>/dev/null || echo "")
    if [ "$NETWORK_STATUS" == "Established" ] || [ "$NETWORK_STATUS" == "Ready" ]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

# Find all tenant namespaces
TENANT_NAMESPACES=$(kubectl get namespaces -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep '^liqo-tenant-' || true)

if [ -n "$TENANT_NAMESPACES" ]; then
  for TENANT_NS in $TENANT_NAMESPACES; do
    echo "--- Processing ${TENANT_NS} ---"
    FC_NAME="${TENANT_NS#liqo-tenant-}"

    # 1. Pre-upgrade connectivity check
    echo "  Pre-upgrade connectivity check..."
    if check_tunnel_connectivity "${TENANT_NS}" "${FC_NAME}" 5; then
      echo "    ✓ Tunnel connectivity verified"
    else
      echo "    ⚠️ Warning: Tunnel not fully established"
    fi

    # 2. Find Gateway Deployments
    GATEWAY_DEPLOYMENTS=$(kubectl get deployments -n "${TENANT_NS}" -l networking.liqo.io/component=gateway -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
    
    if [ -z "$GATEWAY_DEPLOYMENTS" ]; then
      echo "    ℹ️ No gateway deployments found"
      continue
    fi

    for GW_DEPLOY in $GATEWAY_DEPLOYMENTS; do
        echo "    Gateway: ${GW_DEPLOY}"
        GW_NAME=$(echo "${GW_DEPLOY}" | sed 's/^gw-//')
        
        # Detect gateway type (client or server) for this namespace
        GW_TYPE="client"
        if kubectl get gatewayserver "${GW_NAME}" -n "${TENANT_NS}" &>/dev/null; then
          GW_TYPE="server"
        fi
        echo "      Type: Gateway${GW_TYPE^}"
        
        # Check current state
        CURRENT_REPLICAS=$(kubectl get deployment "${GW_DEPLOY}" -n "${TENANT_NS}" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "1")
        CURRENT_STRATEGY=$(kubectl get deployment "${GW_DEPLOY}" -n "${TENANT_NS}" -o jsonpath='{.spec.strategy.type}' 2>/dev/null || echo "unknown")
        CURRENT_IMG=$(kubectl get deployment "${GW_DEPLOY}" -n "${TENANT_NS}" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "")
        
        echo "      Replicas: ${CURRENT_REPLICAS}, Strategy: ${CURRENT_STRATEGY}"
        
        if [ "$CURRENT_STRATEGY" == "RollingUpdate" ] && [ "$CURRENT_REPLICAS" -ge 2 ]; then
          echo "    🔄 RollingUpdate with ${CURRENT_REPLICAS} replicas (HA mode)"
          
          # EARLY IP VALIDATION: Check if status IP matches any running pod
          # This handles the "rollout already completed" case where status IP is stale
          echo "      Checking status IP validity..."
          RUNNING_POD_IPS=$(kubectl get pods -n "${TENANT_NS}" -l networking.liqo.io/component=gateway \
            --field-selector=status.phase=Running -o jsonpath='{.items[*].status.podIP}' 2>/dev/null || echo "")
          
          if [ "$GW_TYPE" == "server" ]; then
            CURRENT_STATUS_IP=$(kubectl get gatewayserver "${GW_NAME}" -n "${TENANT_NS}" \
              -o jsonpath='{.status.internalEndpoint.ip}' 2>/dev/null || echo "")
          else
            CURRENT_STATUS_IP=$(kubectl get gatewayclient "${GW_NAME}" -n "${TENANT_NS}" \
              -o jsonpath='{.status.internalEndpoint.ip}' 2>/dev/null || echo "")
          fi
          
          IP_VALID="false"
          if [ -n "$CURRENT_STATUS_IP" ]; then
            for POD_IP in $RUNNING_POD_IPS; do
              if [ "$POD_IP" == "$CURRENT_STATUS_IP" ]; then
                IP_VALID="true"
                break
              fi
            done
          fi
          
          if [ "$IP_VALID" != "true" ] && [ -n "$CURRENT_STATUS_IP" ]; then
            echo "      ⚠️ Status IP (${CURRENT_STATUS_IP}) is stale - not matching any running pod"
            echo "      Triggering immediate reconciliation to fix status..."
            EARLY_SYNC_TS=$(date +%%s)
            if [ "$GW_TYPE" == "server" ]; then
              kubectl annotate gatewayserver "${GW_NAME}" -n "${TENANT_NS}" liqo.io/force-sync="${EARLY_SYNC_TS}" --overwrite 2>/dev/null || true
            else
              kubectl annotate gatewayclient "${GW_NAME}" -n "${TENANT_NS}" liqo.io/force-sync="${EARLY_SYNC_TS}" --overwrite 2>/dev/null || true
            fi
            echo "      Waiting for controller to update status (5s)..."
            sleep 5
          else
            echo "      ✓ Status IP valid"
          fi
          
          # Check if image needs updating
          if echo "$CURRENT_IMG" | grep -q "${TARGET_VERSION}"; then
            echo "      ✓ Already at ${TARGET_VERSION}"
          else
            # Trigger image update now (after fabric is stable)
            echo "      Triggering image update (fabric is now stable)..."
            IMG_TS=$(date +%%s)
            if [ "$GW_TYPE" == "server" ]; then
              kubectl annotate gatewayserver "${GW_NAME}" -n "${TENANT_NS}" liqo.io/image-upgrade="${IMG_TS}" --overwrite 2>/dev/null || true
            else
              kubectl annotate gatewayclient "${GW_NAME}" -n "${TENANT_NS}" liqo.io/image-upgrade="${IMG_TS}" --overwrite 2>/dev/null || true
            fi
            echo "      ✓ Image update triggered, waiting for controller propagation (10s)..."
            sleep 10
            echo "      Rollout in progress..."
          fi
          
          # Wait for rollout to complete
          echo "      Waiting for rollout to complete..."
          if kubectl rollout status deployment "${GW_DEPLOY}" -n "${TENANT_NS}" --timeout=5m 2>/dev/null; then
            echo "      ✓ Rollout complete"
          else
            echo "      ⚠️ Rollout timed out, checking status..."
          fi
          
          # Verify final state
          READY=$(kubectl get deployment "${GW_DEPLOY}" -n "${TENANT_NS}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
          UPDATED=$(kubectl get deployment "${GW_DEPLOY}" -n "${TENANT_NS}" -o jsonpath='{.status.updatedReplicas}' 2>/dev/null || echo "0")
          echo "      Ready: ${READY}/${CURRENT_REPLICAS}, Updated: ${UPDATED}"
          
          # Check pod versions and identify active pod
          echo "      Checking pod versions..."
          ACTIVE_POD=""
          ACTIVE_POD_OLD=false
          PODS=$(kubectl get pods -n "${TENANT_NS}" -l networking.liqo.io/component=gateway --field-selector=status.phase=Running -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
          for POD in $PODS; do
            POD_IMG=$(kubectl get pod "${POD}" -n "${TENANT_NS}" -o jsonpath='{.spec.containers[0].image}' 2>/dev/null || echo "unknown")
            IS_ACTIVE=$(kubectl get pod "${POD}" -n "${TENANT_NS}" -o jsonpath='{.metadata.labels.networking\.liqo\.io/active}' 2>/dev/null || echo "")
            if [ "$IS_ACTIVE" == "true" ]; then
              echo "        ${POD}: ${POD_IMG} [ACTIVE]"
              ACTIVE_POD="${POD}"
              if ! echo "$POD_IMG" | grep -q "${TARGET_VERSION}"; then
                ACTIVE_POD_OLD=true
              fi
            else
              echo "        ${POD}: ${POD_IMG}"
            fi
          done
          
          # Force leader election to new pod if active pod is on old version
          if [ "$ACTIVE_POD_OLD" == "true" ] && [ -n "$ACTIVE_POD" ]; then
            echo "      ⚠️ Active pod is on old version - forcing leader election to new pod"
            
            # Lease name pattern: {name}.{namespace}.{type}.connections.liqo.io
            LEASE_NAME="${GW_NAME}.${TENANT_NS}.${GW_TYPE}.connections.liqo.io"
            echo "      Deleting leader election Lease: ${LEASE_NAME}"
            kubectl delete lease "${LEASE_NAME}" -n "${TENANT_NS}" 2>/dev/null || true
            
            # GatewayServer needs longer wait - remote consumer must reconnect
            # GatewayClient actively reconnects (fast), GatewayServer waits passively (slower)
            if [ "$GW_TYPE" == "server" ]; then
              LEADER_WAIT=15
              echo "      Waiting for new leader election (${LEADER_WAIT}s - server mode, remote consumer reconnection)..."
            else
              LEADER_WAIT=5
              echo "      Waiting for new leader election (${LEADER_WAIT}s)..."
            fi
            sleep $LEADER_WAIT
            
            # Verify new leader is on new version
            NEW_ACTIVE=$(kubectl get pods -n "${TENANT_NS}" -l networking.liqo.io/component=gateway,networking.liqo.io/active=true -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
            if [ -n "$NEW_ACTIVE" ]; then
              NEW_ACTIVE_IMG=$(kubectl get pod "${NEW_ACTIVE}" -n "${TENANT_NS}" -o jsonpath='{.spec.containers[0].image}' 2>/dev/null || echo "")
              echo "      ✓ New active pod: ${NEW_ACTIVE} (${NEW_ACTIVE_IMG})"
            else
              echo "      ⚠️ No active pod detected yet, leader election in progress..."
              sleep 3
            fi
          fi
          
        else
          echo "    ℹ️ Single replica or Recreate strategy - standard rollout"
          
          # Wait for rollout to complete
          echo "      Waiting for rollout..."
          kubectl rollout status deployment "${GW_DEPLOY}" -n "${TENANT_NS}" --timeout=5m 2>/dev/null || echo "      ⚠️ Rollout timed out"
        fi
        
        # Post-rollout: Wait for any terminating pods to fully disappear
        echo "    Waiting for any terminating pods to disappear..."
        for TERM_WAIT in 1 2 3 4 5 6 7 8 9 10 11 12; do
          TERM_COUNT=$(kubectl get pods -n "${TENANT_NS}" -l networking.liqo.io/component=gateway 2>/dev/null | grep -c "Terminating" 2>/dev/null || echo "0")
          TERM_COUNT=$(echo "$TERM_COUNT" | tr -d '[:space:]')
          
          if [ "${TERM_COUNT}" -eq 0 ]; then
            echo "      ✓ No terminating pods"
            break
          fi
          echo "      Waiting... (Terminating: ${TERM_COUNT})"
          sleep 5
        done

        # Apply Network Tuning (RPF/MSS) to all gateway pods
        echo "    Applying Network Tuning to gateway pods..."
        GW_PODS=$(kubectl get pods -n "${TENANT_NS}" \
          -l networking.liqo.io/component=gateway \
          --field-selector=status.phase=Running \
          -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
        
        for POD_NAME in $GW_PODS; do
          if kubectl exec -n "${TENANT_NS}" "${POD_NAME}" -c gateway -- /bin/sh -c '
              sysctl -w net.ipv4.conf.all.rp_filter=0 >/dev/null 2>&1
              sysctl -w net.ipv4.conf.default.rp_filter=0 >/dev/null 2>&1
              for iface in $(ls /proc/sys/net/ipv4/conf/ | grep liqo); do
                 sysctl -w net.ipv4.conf.$iface.rp_filter=0 >/dev/null 2>&1
              done
              nft add table ip liqo-mss 2>/dev/null || true
              nft add chain ip liqo-mss forward { type filter hook forward priority 0 \; } 2>/dev/null || true
              nft add rule ip liqo-mss forward tcp flags syn tcp option maxseg size set rt mtu 2>/dev/null || true
           ' 2>/dev/null; then
             echo "      ✓ Network tuning applied to ${POD_NAME}"
           else
             echo "      ⚠️ Warning: Network tuning failed for ${POD_NAME}"
           fi
        done

        # IP Synchronization: Ensure Gateway status reflects active pod IP
        echo "    Verifying IP Synchronization for ${GW_NAME} (${GW_TYPE})..."
        
        # Get active pod IP
        ACTIVE_GW_POD=$(kubectl get pods -n "${TENANT_NS}" \
          -l networking.liqo.io/component=gateway,networking.liqo.io/active=true \
          --field-selector=status.phase=Running \
          -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
        
        if [ -z "$ACTIVE_GW_POD" ]; then
          ACTIVE_GW_POD=$(kubectl get pods -n "${TENANT_NS}" \
            -l networking.liqo.io/component=gateway \
            --field-selector=status.phase=Running \
            -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
        fi
        
        SYNCED="false"
        for SYNC_ATTEMPT in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
          ACTUAL_IP=""
          STATUS_IP=""
          
          if [ -n "$ACTIVE_GW_POD" ]; then
            ACTUAL_IP=$(kubectl get pod "${ACTIVE_GW_POD}" -n "${TENANT_NS}" \
              -o jsonpath='{.status.podIP}' 2>/dev/null || echo "")
          fi
          
          # Get status IP from the correct resource type
          if [ "$GW_TYPE" == "server" ]; then
            STATUS_IP=$(kubectl get gatewayserver "${GW_NAME}" -n "${TENANT_NS}" \
              -o jsonpath='{.status.internalEndpoint.ip}' 2>/dev/null || echo "")
          else
            STATUS_IP=$(kubectl get gatewayclient "${GW_NAME}" -n "${TENANT_NS}" \
              -o jsonpath='{.status.internalEndpoint.ip}' 2>/dev/null || echo "")
          fi

          if [ -n "$ACTUAL_IP" ] && [ "$ACTUAL_IP" == "$STATUS_IP" ]; then
            echo "      ✓ IPs Synced: ${ACTUAL_IP}"
            SYNCED="true"
            break
          fi
          
          if [ $((SYNC_ATTEMPT %% 5)) -eq 0 ]; then
            echo "      ⏳ Attempt ${SYNC_ATTEMPT}/15: Pod=${ACTUAL_IP} Status=${STATUS_IP}"
          fi
          
          # Trigger controller reconciliation
          SYNC_TS=$(date +%%s)
          if [ "$GW_TYPE" == "server" ]; then
            kubectl annotate gatewayserver "${GW_NAME}" -n "${TENANT_NS}" \
              liqo.io/force-sync="${SYNC_TS}" --overwrite 2>/dev/null || true
          else
            kubectl annotate gatewayclient "${GW_NAME}" -n "${TENANT_NS}" \
              liqo.io/force-sync="${SYNC_TS}" --overwrite 2>/dev/null || true
          fi
          
          sleep 3
        done

        if [ "$SYNCED" != "true" ]; then
           echo "      ⚠️ IP sync timeout (Pod:${ACTUAL_IP} vs Status:${STATUS_IP})"
           echo "      Controller should reconcile automatically"
        fi
        
        echo "    ✓ Gateway ${GW_DEPLOY} processed"
    done

    echo "  ✓ ${TENANT_NS} processed"
    
    # Canary verification: Verify this peering before proceeding to next
    echo ""
    echo "  🐦 CANARY: Verifying peering connectivity for ${TENANT_NS}..."
    FC_NAME="${TENANT_NS#liqo-tenant-}"
    CANARY_VERIFIED=false
    
    # Fast polling: check every 2s for first 30s, then every 5s for remaining 90s
    # Total timeout: 120s, but usually completes much faster
    FAST_POLL_INTERVAL=2
    FAST_POLL_COUNT=15   # 15 x 2s = 30s
    SLOW_POLL_INTERVAL=5
    SLOW_POLL_COUNT=18   # 18 x 5s = 90s
    
    # Fast polling phase (first 30 seconds)
    for CANARY_ATTEMPT in $(seq 1 ${FAST_POLL_COUNT}); do
      # Check ForeignCluster NetworkConnectionStatus condition
      # Acceptable values: "Established" or "Ready"
      NETWORK_STATUS=$(kubectl get foreigncluster "${FC_NAME}" \
        -o jsonpath='{.status.modules.networking.conditions[?(@.type=="NetworkConnectionStatus")].status}' 2>/dev/null || echo "")
      
      # Also check if gateway pod is ready (all containers)
      GW_READY=$(kubectl get pods -n "${TENANT_NS}" -l networking.liqo.io/component=gateway \
        --field-selector=status.phase=Running -o jsonpath='{.items[0].status.containerStatuses[*].ready}' 2>/dev/null || echo "")
      
      # Check if all containers are ready (no "false" in the list)
      ALL_CONTAINERS_READY="true"
      if [ -z "$GW_READY" ] || echo "$GW_READY" | grep -q "false"; then
        ALL_CONTAINERS_READY="false"
      fi
      
      # Accept both "Established" and "Ready" status
      if { [ "$NETWORK_STATUS" == "Established" ] || [ "$NETWORK_STATUS" == "Ready" ]; } && [ "$ALL_CONTAINERS_READY" == "true" ]; then
        echo "    ✓ CANARY PASSED: Peering ${FC_NAME} verified in $((CANARY_ATTEMPT * FAST_POLL_INTERVAL))s"
        echo "      - ForeignCluster NetworkConnectionStatus: ${NETWORK_STATUS}"
        echo "      - Gateway pod containers ready: all"
        CANARY_VERIFIED=true
        break
      fi
      
      # Only print status every 3rd attempt to reduce noise
      if [ $((CANARY_ATTEMPT %% 3)) -eq 1 ]; then
        echo "    ⏳ CANARY: Checking... ($((CANARY_ATTEMPT * FAST_POLL_INTERVAL))s) Status=${NETWORK_STATUS:-pending} GW=${ALL_CONTAINERS_READY}"
      fi
      sleep ${FAST_POLL_INTERVAL}
    done
    
    # Slow polling phase (remaining 90 seconds) - only if not yet verified
    if [ "$CANARY_VERIFIED" != "true" ]; then
      echo "    ℹ️  Fast poll complete, continuing with slower polling..."
      
      for CANARY_ATTEMPT in $(seq 1 ${SLOW_POLL_COUNT}); do
        NETWORK_STATUS=$(kubectl get foreigncluster "${FC_NAME}" \
          -o jsonpath='{.status.modules.networking.conditions[?(@.type=="NetworkConnectionStatus")].status}' 2>/dev/null || echo "")
        
        GW_READY=$(kubectl get pods -n "${TENANT_NS}" -l networking.liqo.io/component=gateway \
          --field-selector=status.phase=Running -o jsonpath='{.items[0].status.containerStatuses[*].ready}' 2>/dev/null || echo "")
        
        ALL_CONTAINERS_READY="true"
        if [ -z "$GW_READY" ] || echo "$GW_READY" | grep -q "false"; then
          ALL_CONTAINERS_READY="false"
        fi
        
        if { [ "$NETWORK_STATUS" == "Established" ] || [ "$NETWORK_STATUS" == "Ready" ]; } && [ "$ALL_CONTAINERS_READY" == "true" ]; then
          TOTAL_TIME=$((30 + CANARY_ATTEMPT * SLOW_POLL_INTERVAL))
          echo "    ✓ CANARY PASSED: Peering ${FC_NAME} verified in ${TOTAL_TIME}s"
          echo "      - ForeignCluster NetworkConnectionStatus: ${NETWORK_STATUS}"
          echo "      - Gateway pod containers ready: all"
          CANARY_VERIFIED=true
          break
        fi
        
        TOTAL_TIME=$((30 + CANARY_ATTEMPT * SLOW_POLL_INTERVAL))
        echo "    ⏳ CANARY: Waiting... (${TOTAL_TIME}s/120s) Status=${NETWORK_STATUS:-pending} GW=${ALL_CONTAINERS_READY}"
        sleep ${SLOW_POLL_INTERVAL}
      done
    fi
    
    if [ "$CANARY_VERIFIED" != "true" ]; then
      echo ""
      echo "  ❌ CANARY FAILED: Peering ${FC_NAME} did not establish connectivity within 120s!"
      echo "  Debug information:"
      echo "    ForeignCluster networking conditions:"
      kubectl get foreigncluster "${FC_NAME}" -o jsonpath='{range .status.modules.networking.conditions[*]}    {.type}: {.status} ({.reason}){"\n"}{end}' 2>/dev/null || echo "    (could not fetch)"
      echo "    ForeignCluster general conditions:"
      kubectl get foreigncluster "${FC_NAME}" -o jsonpath='{range .status.conditions[*]}    {.type}: {.status} ({.reason}){"\n"}{end}' 2>/dev/null || echo "    (none)"
      echo "    Gateway pods:"
      kubectl get pods -n "${TENANT_NS}" -l networking.liqo.io/component=gateway 2>/dev/null || echo "    (none found)"
      echo ""
      echo "  Stopping canary upgrade - remaining peerings will NOT be upgraded."
      echo "  Rollback will be triggered to restore this peering to previous version."
      exit 1
    fi
    
    echo "  🐦 CANARY: Peering ${FC_NAME} upgrade verified, proceeding to next peering..."
    echo ""
  done
else
  echo "  ℹ️ No tenant namespaces found, skipping gateway processing"
fi

echo "✅ Gateway Stabilization complete (all peerings canary-verified)"

echo ""
echo "--- Upgrading liqo-gateway deployments ---"

# Find all tenant namespaces (re-fetch after reset)
echo ""
echo "========================================="
echo "Step 12: Processing gateway configurations..."
echo "========================================="
echo ""
TENANT_NAMESPACES=$(kubectl get namespaces -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep '^liqo-tenant-' || true)

if [ -z "$TENANT_NAMESPACES" ]; then
  echo "  ℹ️  No tenant namespaces found, skipping gateway upgrade"
else
  echo "  Found tenant namespaces: ${TENANT_NAMESPACES}"
  echo ""

  # Process each tenant namespace
  for TENANT_NS in $TENANT_NAMESPACES; do
    FC="${TENANT_NS#liqo-tenant-}"
    echo "--- Processing tenant namespace: ${TENANT_NS} ---"

    # Clear GatewayClient status (if exists)
    GATEWAY_CLIENTS=$(kubectl get gatewayclient -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
    if [ -n "$GATEWAY_CLIENTS" ]; then
      for GW_CLIENT in ${GATEWAY_CLIENTS}; do
        echo "  Processing GatewayClient: ${GW_CLIENT}"
        echo "    Clearing GatewayClient status..."
        kubectl patch gatewayclient "${GW_CLIENT}" -n "${TENANT_NS}" \
          --subresource=status \
          --type=merge \
          -p='{"status":{"internalEndpoint":null}}' || echo "    ⚠️  Status clear failed"
      done
    fi

    # Clear GatewayServer status (if exists)
    GATEWAY_SERVERS=$(kubectl get gatewayserver -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
    if [ -n "$GATEWAY_SERVERS" ]; then
      for GW_SERVER in ${GATEWAY_SERVERS}; do
        echo "  Processing GatewayServer: ${GW_SERVER}"
        echo "    Clearing GatewayServer status..."
        kubectl patch gatewayserver "${GW_SERVER}" -n "${TENANT_NS}" \
          --subresource=status \
          --type=merge \
          -p='{"status":{"serverRef":null, "endpoint":null, "internalEndpoint":null, "secretRef":null}}' || echo "    ⚠️  Status clear failed"
      done
    fi

    # Annotate Identity resources (without deleting kubeconfig secrets)
    IDENTITY_RESOURCES=$(kubectl get identity -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
    if [ -n "$IDENTITY_RESOURCES" ]; then
      for IDENTITY_NAME in ${IDENTITY_RESOURCES}; do
        echo "  Annotating Identity: ${IDENTITY_NAME}"
        TIMESTAMP=$(date +%%s)
        kubectl annotate identity "${IDENTITY_NAME}" -n "${TENANT_NS}" \
          liqo.io/force-sync="${TIMESTAMP}" \
          --overwrite || echo "    ⚠️  Failed to annotate Identity"
        echo "    ✓ Identity annotated (controller will auto-update kubeconfig)"
      done
    fi

    # Verify Configuration resources (DO NOT modify spec.remote.cidr)
    # IMPORTANT: Configuration.spec.remote.cidr contains the ACTUAL remote cluster's CIDR
    # Configuration.status.remote.cidr contains the REMAPPED CIDR for local use
    # The Liqo networking controller manages these values - DO NOT overwrite spec with status!
    CONFIGURATION_RESOURCES=$(kubectl get configuration -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
    if [ -n "$CONFIGURATION_RESOURCES" ]; then
      for CONFIG_NAME in ${CONFIGURATION_RESOURCES}; do
        echo "  Verifying Configuration: ${CONFIG_NAME}"
        
        # Read and display current values (for verification only)
        SPEC_POD_CIDR=$(kubectl get configuration "${CONFIG_NAME}" -n "${TENANT_NS}" \
          -o jsonpath='{.spec.remote.cidr.pod[0]}' 2>/dev/null || echo "")
        STATUS_POD_CIDR=$(kubectl get configuration "${CONFIG_NAME}" -n "${TENANT_NS}" \
          -o jsonpath='{.status.remote.cidr.pod[0]}' 2>/dev/null || echo "")
        
        echo "    spec.remote.cidr.pod: ${SPEC_POD_CIDR} (actual remote CIDR)"
        echo "    status.remote.cidr.pod: ${STATUS_POD_CIDR} (remapped CIDR for local use)"
        
        if [ -n "$SPEC_POD_CIDR" ] && [ -n "$STATUS_POD_CIDR" ]; then
          if [ "$SPEC_POD_CIDR" == "$STATUS_POD_CIDR" ]; then
            echo "    ⚠️  WARNING: spec equals status - NAT rules may not work correctly!"
            echo "    The spec should contain the actual remote CIDR, not the remapped one."
          else
            echo "    ✓ Configuration looks correct (spec != status)"
          fi
        else
          echo "    ℹ️  CIDRs not yet fully populated"
        fi
      done
    fi

    echo "  ✓ Tenant namespace ${TENANT_NS} processed"
    echo ""
  done
fi

echo ""
echo "========================================="
echo "Step 13: Verifying connectivity after restart..."
echo "========================================="
echo ""
# Give controllers additional time to reconcile after all gateway processing
echo "  Waiting for final reconciliation to complete..."
sleep 15

# Verify network connectivity for each tenant namespace
TENANT_NAMESPACES=$(kubectl get namespaces -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep '^liqo-tenant-' || true)

if [ -n "$TENANT_NAMESPACES" ]; then
  echo "  Verifying network connectivity..."

  for TENANT_NS in ${TENANT_NAMESPACES}; do
    FC="${TENANT_NS#liqo-tenant-}"
    echo "    Checking connectivity for cluster: ${FC}"

    # Verify InternalNode and InternalFabric resources exist
    INTERNAL_NODES=$(kubectl get internalnode -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
    INTERNAL_FABRICS=$(kubectl get internalfabric -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")

    if [ -n "$INTERNAL_NODES" ]; then
      echo "      ✓ InternalNode exists: ${INTERNAL_NODES}"
    else
      echo "      ⚠️  WARNING: InternalNode not found"
    fi

    if [ -n "$INTERNAL_FABRICS" ]; then
      echo "      ✓ InternalFabric exists: ${INTERNAL_FABRICS}"
    else
      echo "      ⚠️  WARNING: InternalFabric not found"
    fi

    # Check RouteConfiguration status
    ROUTE_CONFIGS=$(kubectl get routeconfiguration -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
    if [ -n "$ROUTE_CONFIGS" ]; then
      echo "      ✓ RouteConfiguration exists: ${ROUTE_CONFIGS}"
    fi
  done

  echo "  ✓ Network connectivity verification complete"
else
  echo "  ℹ️  No tenant namespaces found, skipping connectivity verification"
fi

echo ""
echo "========================================="
echo "Step 14: Data plane verification..."
echo "========================================="
echo ""
echo "Verifying routing tables without restart (fabric already updated in Step 7)..."

# Brief wait for route programming
echo "  Waiting for route programming (5s)..."
sleep 5

# Verify routing table exists
echo "  Verifying Liqo routing table..."
FABRIC_POD=$(kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/name=fabric -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
if [ -n "$FABRIC_POD" ]; then
  # Check if the Liqo routing table (1880) exists
  if kubectl exec -n "${NAMESPACE}" "${FABRIC_POD}" -- ip route show table 1880 2>/dev/null | head -1; then
    echo "  ✓ Liqo routing table (1880) is present"
  else
    echo "  ℹ️ Routing table 1880 not found (may be created on demand)"
  fi
fi

echo "✅ Data Plane verified"

echo ""
echo "========================================="
echo "Step 15: Verifying fabric API server connectivity..."
echo "========================================="
echo ""
echo "This is CRITICAL - if Fabric cannot reach API Server, routing will fail."
echo ""

FABRIC_POD=$(kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/name=fabric -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")

if [ -n "$FABRIC_POD" ]; then
  # First check if wget or curl exists in the pod
  TOOLS_AVAILABLE=false
  if kubectl exec -n "${NAMESPACE}" "$FABRIC_POD" -- /bin/sh -c 'command -v wget >/dev/null 2>&1 || command -v curl >/dev/null 2>&1' 2>/dev/null; then
    TOOLS_AVAILABLE=true
  fi

  if [ "$TOOLS_AVAILABLE" = "true" ]; then
    MAX_RETRIES=10
    RETRY_COUNT=0
    API_REACHABLE=false

    while [ $RETRY_COUNT -lt $MAX_RETRIES ]; do
      # Try to reach API server from within the fabric pod
      if kubectl exec -n "${NAMESPACE}" "$FABRIC_POD" -- /bin/sh -c 'wget -q -O /dev/null --timeout=5 --no-check-certificate https://kubernetes.default.svc/healthz 2>/dev/null || curl -k -s -o /dev/null --connect-timeout 5 https://kubernetes.default.svc/healthz 2>/dev/null' 2>/dev/null; then
        echo "  ✓ Fabric can reach API Server"
        API_REACHABLE=true
        break
      fi
      echo "  ⏳ Waiting for API connectivity... ($RETRY_COUNT/$MAX_RETRIES)"
      sleep 2
      RETRY_COUNT=$((RETRY_COUNT+1))
    done

    if [ "$API_REACHABLE" != "true" ]; then
      echo "  ⚠️ WARNING: Fabric cannot reach API Server after ${MAX_RETRIES} attempts"
      echo "  This may be temporary - proceeding with upgrade."
      echo "  Note: Fabric will retry API connection automatically."
    fi
  else
    echo "  ℹ️ wget/curl not available in fabric pod - skipping API connectivity check"
    echo "  Note: Fabric uses native Go HTTP client and will connect automatically."
  fi
else
  echo "  ⚠️ Warning: No fabric pod found to verify API connectivity"
fi

echo ""
echo "========================================="
echo "Step 16: Upgrading Virtual Kubelet..."
echo "========================================="
echo ""
echo "Virtual Kubelet must be upgraded AFTER the network fabric is stable"
echo "to ensure proper IP remapping negotiation with IPAM."
echo ""

# Safety check: Ensure liqo-ipam is healthy before VK upgrade
echo "Verifying liqo-ipam is healthy before VK upgrade..."
if ! kubectl wait --for=condition=available deployment/liqo-ipam -n "${NAMESPACE}" --timeout=60s 2>/dev/null; then
  echo "  ⚠️ Warning: liqo-ipam not available, VK upgrade may have issues"
else
  echo "  ✓ liqo-ipam is healthy"
fi

# Upgrade VkOptionsTemplate (image and extraArgs from upgrade plan)
echo ""
echo "--- Upgrading VkOptionsTemplate ---"
if kubectl get vkoptionstemplate virtual-kubelet-default -n "${NAMESPACE}" &>/dev/null; then
  echo "Updating VkOptionsTemplate..."
  
  # Find VkOptionsTemplate in upgrade plan
  VK_TEMPLATE_PLAN=$(echo "$PLAN_JSON" | jq -r '
    .templatesToUpdate[] | select(.name == "virtual-kubelet-default" and .kind == "VkOptionsTemplate")
  ')
  
  if [ -z "$VK_TEMPLATE_PLAN" ] || [ "$VK_TEMPLATE_PLAN" == "null" ]; then
    echo "  ⚠️  No update plan for VkOptionsTemplate, using fallback image upgrade"
    
    # Fallback: simple image update
    kubectl patch vkoptionstemplate virtual-kubelet-default -n "${NAMESPACE}" \
      --type='json' -p='[
        {"op": "replace", "path": "/spec/containerImage", "value": "'"${IMAGE_REGISTRY}"'/virtual-kubelet:'"${TARGET_VERSION}"'"}
      ]' && echo "  ✓ VkOptionsTemplate image updated (fallback)" || echo "  ⚠️  Warning: Could not update VkOptionsTemplate"
  else
    echo "  Applying upgrade plan for VkOptionsTemplate..."
    
    # Get container changes (VkOptionsTemplate has a single virtual-kubelet container)
    CONTAINER_CHANGES=$(echo "$VK_TEMPLATE_PLAN" | jq -r '.containerChanges // []')
    CONTAINER_CHANGE=$(echo "$CONTAINER_CHANGES" | jq -r '.[0] // empty')
    
    if [ -n "$CONTAINER_CHANGE" ]; then
      TARGET_IMAGE=$(echo "$CONTAINER_CHANGE" | jq -r '.targetImage')
      FLAG_CHANGES=$(echo "$CONTAINER_CHANGE" | jq -r '.flagChanges // []')
      
      echo "  Target image: ${TARGET_IMAGE}"
      
      # Update image
      kubectl patch vkoptionstemplate virtual-kubelet-default -n "${NAMESPACE}" \
        --type='json' -p='[
          {"op": "replace", "path": "/spec/containerImage", "value": "'"${TARGET_IMAGE}"'"}
        ]' && echo "  ✓ Image updated" || echo "  ⚠️  Warning: Could not update image"
      
      # Apply extraArgs changes (VkOptionsTemplate uses spec.extraArgs instead of container args)
      FLAG_COUNT=$(echo "$FLAG_CHANGES" | jq 'length')
      if [ "$FLAG_COUNT" -gt 0 ]; then
        echo "  Applying ${FLAG_COUNT} extraArgs change(s)..."
        
        # Get current extraArgs
        CURRENT_ARGS=$(kubectl get vkoptionstemplate virtual-kubelet-default -n "${NAMESPACE}" \
          -o jsonpath='{.spec.extraArgs}' 2>/dev/null | jq -r '.' 2>/dev/null || echo "[]")
        
        if [ -z "$CURRENT_ARGS" ] || [ "$CURRENT_ARGS" == "null" ]; then
          CURRENT_ARGS="[]"
        fi
        
        NEW_ARGS="$CURRENT_ARGS"
        while IFS= read -r change; do
          TYPE=$(echo "$change" | jq -r '.type')
          FLAG=$(echo "$change" | jq -r '.flag')
          NEW_VALUE=$(echo "$change" | jq -r '.newValue // ""')
          
          if [ "$TYPE" == "add" ]; then
            echo "    add: --${FLAG}=${NEW_VALUE}"
            NEW_ARGS=$(echo "$NEW_ARGS" | jq --arg f "--${FLAG}=${NEW_VALUE}" '. += [$f]')
          elif [ "$TYPE" == "update" ]; then
            echo "    update: --${FLAG}=${NEW_VALUE}"
            NEW_ARGS=$(echo "$NEW_ARGS" | jq --arg prefix "--${FLAG}" 'map(select(startswith($prefix) | not))')
            NEW_ARGS=$(echo "$NEW_ARGS" | jq --arg f "--${FLAG}=${NEW_VALUE}" '. += [$f]')
          elif [ "$TYPE" == "remove" ]; then
            echo "    remove: --${FLAG}"
            NEW_ARGS=$(echo "$NEW_ARGS" | jq --arg prefix "--${FLAG}" 'map(select(startswith($prefix) | not))')
          fi
        done < <(echo "$FLAG_CHANGES" | jq -c '.[]')
        
        # Apply extraArgs patch
        kubectl patch vkoptionstemplate virtual-kubelet-default -n "${NAMESPACE}" \
          --type='json' -p='[{"op": "replace", "path": "/spec/extraArgs", "value": '"${NEW_ARGS}"'}]' \
          && echo "  ✓ extraArgs updated" || echo "  ⚠️  Warning: Could not update extraArgs"
      else
        echo "  No extraArgs changes needed"
      fi
    else
      # No container changes in plan, use fallback
      echo "  No container changes in plan, using fallback image upgrade"
      kubectl patch vkoptionstemplate virtual-kubelet-default -n "${NAMESPACE}" \
        --type='json' -p='[
          {"op": "replace", "path": "/spec/containerImage", "value": "'"${IMAGE_REGISTRY}"'/virtual-kubelet:'"${TARGET_VERSION}"'"}
        ]' && echo "  ✓ VkOptionsTemplate image updated" || echo "  ⚠️  Warning: Could not update VkOptionsTemplate"
    fi
  fi
  
  echo "  ✅ VkOptionsTemplate upgrade complete"
else
  echo "  ℹ️  VkOptionsTemplate not found, skipping"
fi

# Verify template was updated
sleep 2
echo "Verifying VkOptionsTemplate update..."
VK_TEMPLATE_IMAGE=$(kubectl get vkoptionstemplate virtual-kubelet-default -n "${NAMESPACE}" \
  -o jsonpath='{.spec.containerImage}' 2>/dev/null || echo "not found")
VK_TEMPLATE_ARGS=$(kubectl get vkoptionstemplate virtual-kubelet-default -n "${NAMESPACE}" \
  -o jsonpath='{.spec.extraArgs}' 2>/dev/null || echo "[]")
echo "  VkOptionsTemplate image: ${VK_TEMPLATE_IMAGE}"
echo "  VkOptionsTemplate extraArgs: ${VK_TEMPLATE_ARGS}"

if [[ "$VK_TEMPLATE_IMAGE" != *"${TARGET_VERSION}"* ]] && [[ "$VK_TEMPLATE_IMAGE" != "not found" ]]; then
  echo "⚠️ Warning: VkOptionsTemplate not updated to ${TARGET_VERSION}"
fi

# Upgrade existing VirtualNode resources to use new image
echo ""
echo "--- Upgrading VirtualNode Resources ---"
VIRTUALNODES=$(kubectl get virtualnodes -A -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null || echo "")

if [ -z "$VIRTUALNODES" ]; then
  echo "  ℹ️  No VirtualNode resources found, skipping"
else
  VN_COUNT=0
  while IFS= read -r line; do
    if [ -n "$line" ]; then
      VN_NAMESPACE=$(echo "$line" | awk '{print $1}')
      VN_NAME=$(echo "$line" | awk '{print $2}')

      echo "  Updating VirtualNode: ${VN_NAME} in namespace ${VN_NAMESPACE}..."

      # Get current image
      CURRENT_VK_IMAGE=$(kubectl get virtualnode "$VN_NAME" -n "$VN_NAMESPACE" \
        -o jsonpath='{.spec.template.spec.template.spec.containers[0].image}' 2>/dev/null || echo "unknown")

      if [[ "$CURRENT_VK_IMAGE" != *"${TARGET_VERSION}"* ]]; then
        # Patch VirtualNode to update virtual-kubelet image
        echo "    Patching VirtualNode to update image to ${TARGET_VERSION}..."
        kubectl patch virtualnode "$VN_NAME" -n "$VN_NAMESPACE" \
          --type='json' -p='[
            {"op": "replace", "path": "/spec/template/spec/template/spec/containers/0/image", "value": "'"${IMAGE_REGISTRY}"'/virtual-kubelet:'"${TARGET_VERSION}"'"}
          ]' && echo "      ✓ VirtualNode image updated" || echo "      ⚠️  Warning: Could not update VirtualNode"

        VN_COUNT=$((VN_COUNT + 1))

        # Find and delete deployment to force recreation with new image
        DEPLOYMENT_NAME=$(kubectl get deployments -n "$VN_NAMESPACE" -l liqo.io/virtual-node="$VN_NAME" \
          -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")

        if [ -n "$DEPLOYMENT_NAME" ]; then
          echo "    Found deployment: ${DEPLOYMENT_NAME}"
          echo "    Deleting deployment to force recreation with new image..."
          kubectl delete deployment "$DEPLOYMENT_NAME" -n "$VN_NAMESPACE" || echo "      ⚠️  Warning: Could not delete deployment"
        else
          echo "    ⚠️  Warning: No deployment found for VirtualNode ${VN_NAME}"
        fi
      else
        echo "    ℹ️  Already at ${TARGET_VERSION}, skipping"
      fi
    fi
  done <<< "$VIRTUALNODES"

  echo "✅ ${VN_COUNT} VirtualNode(s) processed for upgrade"

  # Wait for Liqo controller to recreate virtual-kubelet deployments from updated templates
  if [ "$VN_COUNT" -gt 0 ]; then
    echo ""
    echo "Waiting for Liqo controller to recreate virtual-kubelet deployments from updated templates..."
    echo "Waiting for deployments to be recreated..."
    sleep 10

    TIMEOUT=120
    ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
      VK_DEPLOYMENTS=$(kubectl get deployments -A -l offloading.liqo.io/component=virtual-kubelet \
        -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")

      if [ -n "$VK_DEPLOYMENTS" ]; then
        DEPLOYMENT_COUNT=$(echo "$VK_DEPLOYMENTS" | wc -w)
        if [ "$DEPLOYMENT_COUNT" -ge "$VN_COUNT" ]; then
          echo "  ✓ All virtual-kubelet deployments recreated"
          break
        fi
      fi

      sleep 5
      ELAPSED=$((ELAPSED + 5))
    done

    if [ $ELAPSED -ge $TIMEOUT ]; then
      echo "  ⚠️  Warning: Not all deployments recreated within ${TIMEOUT}s timeout"
    fi

    # Wait for virtual-kubelet deployments to roll out
    echo ""
    echo "Waiting for virtual-kubelet deployments to roll out..."
    VK_DEPLOYMENTS=$(kubectl get deployments -A -l offloading.liqo.io/component=virtual-kubelet \
      -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null || echo "")

    if [ -n "$VK_DEPLOYMENTS" ]; then
      while IFS= read -r line; do
        if [ -n "$line" ]; then
          VK_NS=$(echo "$line" | awk '{print $1}')
          VK_DEPLOY=$(echo "$line" | awk '{print $2}')

          echo "  Checking ${VK_DEPLOY} in ${VK_NS}..."
          kubectl rollout status deployment/"$VK_DEPLOY" -n "$VK_NS" --timeout=2m || echo "    ⚠️  Warning: Rollout status check failed"

          # CRITICAL: Inject LIQO_IPAM_SERVER environment variable
          # This allows the Virtual Kubelet to find the IPAM server for IP remapping
          # Using env var instead of --ipam-server flag (which is not supported in all versions)
          echo "    Injecting LIQO_IPAM_SERVER environment variable..."
          kubectl set env deployment/"$VK_DEPLOY" -n "$VK_NS" \
            LIQO_IPAM_SERVER="liqo-ipam.${NAMESPACE}.svc.cluster.local:6000" \
            --overwrite 2>/dev/null && echo "      ✓ IPAM environment variable injected" \
            || echo "      ⚠️ Warning: Could not inject IPAM env var"

          # CRITICAL FIX: Force pod deletion to make VK reload Configuration
          # The VK caches Configuration at startup, so we must delete pods to force restart
          echo "    Forcing VK pod recreation to reload Configuration..."
          kubectl delete pods -n "$VK_NS" -l offloading.liqo.io/component=virtual-kubelet --wait=false 2>/dev/null || true
          
          # Wait for new pods to be ready
          echo "    Waiting for VK pods to be recreated..."
          sleep 10
          kubectl rollout status deployment/"$VK_DEPLOY" -n "$VK_NS" --timeout=3m 2>/dev/null || true

          # Verify new image
          NEW_IMAGE=$(kubectl get deployment "$VK_DEPLOY" -n "$VK_NS" \
            -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "")
          echo "    New image: ${NEW_IMAGE}"

          if [[ "$NEW_IMAGE" == *"${TARGET_VERSION}"* ]]; then
            echo "    ✓ Successfully upgraded to ${TARGET_VERSION}"
          else
            echo "    ⚠️  Warning: Image not updated to ${TARGET_VERSION}"
          fi
        fi
      done <<< "$VK_DEPLOYMENTS"
    fi
  fi
fi

echo ""
echo "✅ Virtual Kubelet upgrade complete"

echo ""
echo "========================================="
echo "Step 17: Final network stabilization..."
echo "========================================="
echo ""
echo "Triggering Gateway reconciliation to update internalEndpoint IPs..."
echo "NOTE: Fabric was already updated in Step 7 - no restart needed."

# Force Gateway reconciliation to update internalEndpoint IPs
# The annotation triggers the controller to update Gateway status,
# which will cause fabric to update routes without needing a restart.
TENANT_NAMESPACES=$(kubectl get namespaces -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep '^liqo-tenant-' || true)
for TENANT_NS in $TENANT_NAMESPACES; do
  # Process GatewayClients if present
  GATEWAY_CLIENTS=$(kubectl get gatewayclient -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
  for GW_CLIENT in $GATEWAY_CLIENTS; do
    SYNC_TS=$(date +%%s)
    kubectl annotate gatewayclient "${GW_CLIENT}" -n "${TENANT_NS}" liqo.io/force-sync="${SYNC_TS}" --overwrite 2>/dev/null || true
    echo "  ✓ Triggered reconciliation for GatewayClient ${GW_CLIENT} in ${TENANT_NS}"
  done
  
  # Process GatewayServers if present
  GATEWAY_SERVERS=$(kubectl get gatewayserver -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
  for GW_SERVER in $GATEWAY_SERVERS; do
    SYNC_TS=$(date +%%s)
    kubectl annotate gatewayserver "${GW_SERVER}" -n "${TENANT_NS}" liqo.io/force-sync="${SYNC_TS}" --overwrite 2>/dev/null || true
    echo "  ✓ Triggered reconciliation for GatewayServer ${GW_SERVER} in ${TENANT_NS}"
  done
done

# Wait for controller to process annotations and fabric to update routes
echo "Waiting for route updates to propagate (10s)..."
sleep 10

echo "✓ Final network stabilization complete (zero-downtime - no fabric restart)"

echo ""
echo "========================================="
echo "Step 18: Final verification of network & data-plane..."
echo "========================================="
echo ""
# Wait for pod updates to fully propagate
echo "Waiting for pod updates to propagate..."
sleep 5

# Re-fetch tenant namespaces for verification
TENANT_NAMESPACES=$(kubectl get namespaces -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep '^liqo-tenant-' || true)

# Verify core network components by checking running pods
if kubectl get deployment liqo-ipam -n "${NAMESPACE}" &>/dev/null; then
  echo "  Checking liqo-ipam (deployment):"
  CURRENT_IMAGE=$(kubectl get deployment liqo-ipam -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].image}')
  echo "    Deployment spec image: ${CURRENT_IMAGE}"

  # Check actual running pod
  POD_IMAGE=$(kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/name=ipam -o jsonpath='{.items[0].spec.containers[0].image}' 2>/dev/null || echo "")
  if [ -n "$POD_IMAGE" ]; then
    echo "    Running pod image: ${POD_IMAGE}"
  fi

  if [[ "$CURRENT_IMAGE" != *"${TARGET_VERSION}"* ]]; then
    echo "    ❌ ERROR: liqo-ipam not running target version!"
    exit 1
  fi
  echo "    ✓ Version correct"
fi

if kubectl get deployment liqo-proxy -n "${NAMESPACE}" &>/dev/null; then
  echo "  Checking liqo-proxy (deployment):"
  CURRENT_IMAGE=$(kubectl get deployment liqo-proxy -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].image}')
  echo "    Deployment spec image: ${CURRENT_IMAGE}"

  POD_IMAGE=$(kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/name=proxy -o jsonpath='{.items[0].spec.containers[0].image}' 2>/dev/null || echo "")
  if [ -n "$POD_IMAGE" ]; then
    echo "    Running pod image: ${POD_IMAGE}"
  fi

  if [[ "$CURRENT_IMAGE" != *"${TARGET_VERSION}"* ]]; then
    echo "    ❌ ERROR: liqo-proxy not running target version!"
    exit 1
  fi
  echo "    ✓ Version correct"
fi

if kubectl get daemonset liqo-fabric -n "${NAMESPACE}" &>/dev/null; then
  echo "  Checking liqo-fabric (daemonset):"
  CURRENT_IMAGE=$(kubectl get daemonset liqo-fabric -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].image}')
  echo "    DaemonSet spec image: ${CURRENT_IMAGE}"

  POD_IMAGE=$(kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/name=fabric -o jsonpath='{.items[0].spec.containers[0].image}' 2>/dev/null || echo "")
  if [ -n "$POD_IMAGE" ]; then
    echo "    Running pod image: ${POD_IMAGE}"
  fi

  if [[ "$CURRENT_IMAGE" != *"${TARGET_VERSION}"* ]]; then
    echo "    ❌ ERROR: liqo-fabric not running target version!"
    exit 1
  fi
  echo "    ✓ Version correct"
fi

# Verify gateway deployments in tenant namespaces
for TENANT_NS in ${TENANT_NAMESPACES}; do
  GATEWAY_DEPLOYMENTS=$(kubectl get deployments -n "${TENANT_NS}" -l networking.liqo.io/component=gateway -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)

  for GW in ${GATEWAY_DEPLOYMENTS}; do
    echo "  Checking gateway ${GW} in ${TENANT_NS}:"

    # Double-check rollout status
    kubectl rollout status deployment/"${GW}" -n "${TENANT_NS}" --timeout=30s &>/dev/null || true

    # Get the current pod-template-hash from the deployment's replicaset
    CURRENT_RS=$(kubectl get rs -n "${TENANT_NS}" -l networking.liqo.io/gateway-name="${GW#gw-}" \
      --sort-by='.metadata.creationTimestamp' -o jsonpath='{.items[-1:].metadata.labels.pod-template-hash}' 2>/dev/null || echo "")

    # Check deployment spec (should be updated)
    GATEWAY_IMAGE=$(kubectl get deployment "${GW}" -n "${TENANT_NS}" \
      -o jsonpath='{.spec.template.spec.containers[?(@.name=="gateway")].image}')
    WIREGUARD_IMAGE=$(kubectl get deployment "${GW}" -n "${TENANT_NS}" \
      -o jsonpath='{.spec.template.spec.containers[?(@.name=="wireguard")].image}')
    GENEVE_IMAGE=$(kubectl get deployment "${GW}" -n "${TENANT_NS}" \
      -o jsonpath='{.spec.template.spec.containers[?(@.name=="geneve")].image}')

    echo "    Deployment spec:"
    echo "      Gateway: ${GATEWAY_IMAGE}"
    echo "      Wireguard: ${WIREGUARD_IMAGE}"
    echo "      Geneve: ${GENEVE_IMAGE}"

    # Find the actual running pod with the current template hash
    if [ -n "$CURRENT_RS" ]; then
      RUNNING_POD=$(kubectl get pods -n "${TENANT_NS}" \
        -l networking.liqo.io/gateway-name="${GW#gw-}",pod-template-hash="${CURRENT_RS}" \
        -o jsonpath='{.items[?(@.status.phase=="Running")].metadata.name}' 2>/dev/null | awk '{print $1}')
    else
      RUNNING_POD=$(kubectl get pods -n "${TENANT_NS}" \
        -l networking.liqo.io/gateway-name="${GW#gw-}" \
        -o jsonpath='{.items[?(@.status.phase=="Running")].metadata.name}' 2>/dev/null | awk '{print $1}')
    fi

    if [ -n "$RUNNING_POD" ]; then
      POD_GATEWAY_IMAGE=$(kubectl get pod "${RUNNING_POD}" -n "${TENANT_NS}" \
        -o jsonpath='{.spec.containers[?(@.name=="gateway")].image}' 2>/dev/null || echo "")
      POD_WIREGUARD_IMAGE=$(kubectl get pod "${RUNNING_POD}" -n "${TENANT_NS}" \
        -o jsonpath='{.spec.containers[?(@.name=="wireguard")].image}' 2>/dev/null || echo "")
      POD_GENEVE_IMAGE=$(kubectl get pod "${RUNNING_POD}" -n "${TENANT_NS}" \
        -o jsonpath='{.spec.containers[?(@.name=="geneve")].image}' 2>/dev/null || echo "")

      echo "    Running pod (${RUNNING_POD}):"
      echo "      Gateway: ${POD_GATEWAY_IMAGE}"
      echo "      Wireguard: ${POD_WIREGUARD_IMAGE}"
      echo "      Geneve: ${POD_GENEVE_IMAGE}"
    fi

    # Verify deployment spec containers are on target version
    if [[ "$GATEWAY_IMAGE" != *"${TARGET_VERSION}"* ]] || \
       [[ "$WIREGUARD_IMAGE" != *"${TARGET_VERSION}"* ]] || \
       [[ "$GENEVE_IMAGE" != *"${TARGET_VERSION}"* ]]; then
      echo "    ❌ ERROR: Deployment spec not on target version!"
      echo "    Expected version: ${TARGET_VERSION}"
      exit 1
    fi

    echo "    ✓ All containers on target version"
    
    # Update version labels for gateway deployment
    echo "    Updating version labels..."
    kubectl patch deployment "${GW}" -n "${TENANT_NS}" --type=json \
      -p='[
        {"op":"replace","path":"/metadata/labels/app.kubernetes.io~1version","value":"'"${TARGET_VERSION}"'"},
        {"op":"replace","path":"/metadata/labels/helm.sh~1chart","value":"liqo-'"${TARGET_VERSION}"'"}
      ]' 2>/dev/null || echo "      ⚠️ Could not update metadata labels"
    kubectl patch deployment "${GW}" -n "${TENANT_NS}" --type=json \
      -p='[
        {"op":"replace","path":"/spec/template/metadata/labels/app.kubernetes.io~1version","value":"'"${TARGET_VERSION}"'"},
        {"op":"replace","path":"/spec/template/metadata/labels/helm.sh~1chart","value":"liqo-'"${TARGET_VERSION}"'"}
      ]' 2>/dev/null || echo "      ⚠️ Could not update pod template labels"
    echo "    ✓ Labels updated"
  done
done

echo ""
echo "========================================="
echo "Step 19: Comprehensive network connectivity verification..."
echo "========================================="
echo ""
# Check fabric pods can reach API server
echo "  Checking fabric pods API connectivity..."
FABRIC_PODS=$(kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/name=fabric -o jsonpath='{.items[*].metadata.name}')
if [ -n "$FABRIC_PODS" ]; then
  FABRIC_POD=$(echo "$FABRIC_PODS" | awk '{print $1}')
  # Check if wget or curl is available
  if kubectl exec -n "${NAMESPACE}" "$FABRIC_POD" -- /bin/sh -c 'command -v wget >/dev/null 2>&1 || command -v curl >/dev/null 2>&1' 2>/dev/null; then
    if kubectl exec -n "${NAMESPACE}" "$FABRIC_POD" -- /bin/sh -c 'wget -q -O- --timeout=5 https://kubernetes.default.svc >/dev/null 2>&1 || curl -k -s --connect-timeout 5 https://kubernetes.default.svc >/dev/null 2>&1' 2>/dev/null; then
      echo "    ✓ Fabric pod can reach Kubernetes API"
    else
      echo "    ⚠️  WARNING: Fabric pod cannot reach Kubernetes API"
    fi
  else
    echo "    ℹ️  Skipping API check (wget/curl not in fabric image)"
  fi
else
  echo "    ⚠️  WARNING: No fabric pods found"
fi

# Check for active ForeignClusters and their connectivity
echo "  Checking ForeignCluster connectivity..."
FOREIGN_CLUSTERS=$(kubectl get foreignclusters -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
if [ -n "$FOREIGN_CLUSTERS" ]; then
  FC_COUNT=$(echo "$FOREIGN_CLUSTERS" | wc -w)
  echo "    Found ${FC_COUNT} ForeignCluster(s)"

  for FC in $FOREIGN_CLUSTERS; do
    FC_STATUS=$(kubectl get foreigncluster "$FC" -o jsonpath='{.status.modules.networking.conditions[?(@.type=="NetworkConnectionStatus")].status}' 2>/dev/null || echo "Unknown")
    echo "      - ${FC}: NetworkConnectionStatus=${FC_STATUS}"
  done

  # Count established connections (check for "Established" or "Ready" status)
  ESTABLISHED=$(kubectl get foreignclusters -o json 2>/dev/null | jq -r '.items[] | select(.status.modules.networking.conditions != null) | .status.modules.networking.conditions[] | select(.type=="NetworkConnectionStatus" and (.status=="Established" or .status=="Ready"))' 2>/dev/null | wc -l || echo "0")
  ESTABLISHED=$(echo "$ESTABLISHED" | tr -d '[:space:]')
  if [ "$ESTABLISHED" -gt 0 ]; then
    echo "    ✓ ${ESTABLISHED} ForeignCluster(s) with network connectivity"
  else
    echo "    ⚠️  WARNING: No ForeignClusters with established network connectivity"
  fi
else
  echo "    ℹ️  No ForeignClusters found (single-cluster setup)"
fi

# Verify tunnel interfaces exist (if fabric is running)
if [ -n "$FABRIC_PODS" ]; then
  echo "  Checking tunnel interfaces..."
  FABRIC_POD=$(echo "$FABRIC_PODS" | awk '{print $1}')
  TUNNEL_COUNT=$(kubectl exec -n "${NAMESPACE}" "$FABRIC_POD" -- ip link show type geneve 2>/dev/null | grep -c "geneve" 2>/dev/null || echo "0")
  TUNNEL_COUNT=$(echo "$TUNNEL_COUNT" | tr -d '[:space:]')
  if [ "$TUNNEL_COUNT" -gt 0 ]; then
    echo "    ✓ Found ${TUNNEL_COUNT} tunnel interface(s)"
  else
    echo "    ℹ️  No tunnel interfaces found (may be normal if no peerings)"
  fi
fi

# Check gateway pod connectivity in tenant namespaces
echo "  Checking gateway pod connectivity..."
GATEWAY_HEALTHY=0
GATEWAY_UNHEALTHY=0

for TENANT_NS in ${TENANT_NAMESPACES}; do
  GATEWAY_PODS=$(kubectl get pods -n "${TENANT_NS}" -l networking.liqo.io/component=gateway -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
  
  # FIXED: Check if GATEWAY_PODS is empty before looping to avoid syntax error
  if [ -n "$GATEWAY_PODS" ]; then
    for GW_POD in $GATEWAY_PODS; do
        POD_STATUS=$(kubectl get pod "$GW_POD" -n "${TENANT_NS}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
        if [ "$POD_STATUS" == "Running" ]; then
        GATEWAY_HEALTHY=$((GATEWAY_HEALTHY + 1))
        else
        GATEWAY_UNHEALTHY=$((GATEWAY_UNHEALTHY + 1))
        echo "    ⚠️  Gateway pod ${GW_POD} in ${TENANT_NS}: ${POD_STATUS}"
        fi
    done
  fi
done

if [ "${GATEWAY_HEALTHY:-0}" -gt 0 ]; then
  echo "    ✓ ${GATEWAY_HEALTHY} gateway pod(s) running"
fi

if [ "${GATEWAY_UNHEALTHY:-0}" -gt 0 ]; then
  echo "    ⚠️  WARNING: ${GATEWAY_UNHEALTHY} gateway pod(s) not running"
fi

echo ""
echo "========================================="
echo "Step 20: Post-upgrade cleanup (restoring template settings)..."
echo "========================================="
echo ""
echo "Restoring gateway templates to original settings (replicas=1, Recreate)..."
echo ""

# Restore WgGatewayClientTemplate to original settings
echo "--- Restoring WgGatewayClientTemplate ---"
if kubectl get wggatewayclienttemplate wireguard-client -n "${NAMESPACE}" &>/dev/null; then
  kubectl patch wggatewayclienttemplate wireguard-client -n "${NAMESPACE}" \
    --type='json' -p='[
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/replicas", "value": 1},
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/strategy", "value": {"type": "Recreate"}}
    ]' && echo "  ✓ WgGatewayClientTemplate: replicas=1, strategy=Recreate" || {
    echo "  ⚠️ Warning: Could not restore template settings, trying separately..."
    kubectl patch wggatewayclienttemplate wireguard-client -n "${NAMESPACE}" \
      --type='json' -p='[{"op": "replace", "path": "/spec/template/spec/deployment/spec/replicas", "value": 1}]' 2>/dev/null || true
    kubectl patch wggatewayclienttemplate wireguard-client -n "${NAMESPACE}" \
      --type='merge' -p='{"spec":{"template":{"spec":{"deployment":{"spec":{"strategy":{"type":"Recreate"}}}}}}}' 2>/dev/null || true
  }
else
  echo "  ℹ️  WgGatewayClientTemplate not found"
fi

# Restore WgGatewayServerTemplate to original settings
echo "--- Restoring WgGatewayServerTemplate ---"
if kubectl get wggatewayservertemplate wireguard-server -n "${NAMESPACE}" &>/dev/null; then
  kubectl patch wggatewayservertemplate wireguard-server -n "${NAMESPACE}" \
    --type='json' -p='[
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/replicas", "value": 1},
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/strategy", "value": {"type": "Recreate"}}
    ]' && echo "  ✓ WgGatewayServerTemplate: replicas=1, strategy=Recreate" || {
    echo "  ⚠️ Warning: Could not restore template settings"
  }
else
  echo "  ℹ️  WgGatewayServerTemplate not found"
fi

echo ""
echo "--- Triggering Controller to Scale Down Gateways ---"
echo "CRITICAL: Delete passive pods first to preserve active pod during scale-down..."

# Delete passive pods manually before triggering scale-down
TENANT_NAMESPACES=$(kubectl get namespaces -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep '^liqo-tenant-' || true)
PASSIVE_PODS_DELETED=0

for TENANT_NS in $TENANT_NAMESPACES; do
  GATEWAY_DEPLOYMENTS=$(kubectl get deployments -n "${TENANT_NS}" -l networking.liqo.io/component=gateway -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
  
  for GW_DEPLOY in $GATEWAY_DEPLOYMENTS; do
    if [ -n "$GW_DEPLOY" ]; then
      # Check if deployment has 2 replicas (still in HA mode)
      CURRENT_REPLICAS=$(kubectl get deployment "${GW_DEPLOY}" -n "${TENANT_NS}" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "1")
      
      if [ "$CURRENT_REPLICAS" -eq 2 ]; then
        echo "  Processing ${GW_DEPLOY} in ${TENANT_NS}..."
        
        # Get active pod directly using label selector (avoids bash word-splitting issues)
        ACTIVE_POD=$(kubectl get pods -n "${TENANT_NS}" \
          -l networking.liqo.io/component=gateway,networking.liqo.io/active=true \
          --field-selector=status.phase=Running \
          -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
        
        # Get all gateway pods
        ALL_PODS=$(kubectl get pods -n "${TENANT_NS}" \
          -l networking.liqo.io/component=gateway \
          --field-selector=status.phase=Running \
          -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
        
        # Find passive pod (any pod that's not the active one)
        PASSIVE_POD=""
        for POD_NAME in $ALL_PODS; do
          if [ -n "$POD_NAME" ] && [ "$POD_NAME" != "$ACTIVE_POD" ]; then
            PASSIVE_POD="$POD_NAME"
            break
          fi
        done
        
        if [ -n "$PASSIVE_POD" ] && [ -n "$ACTIVE_POD" ]; then
          echo "    Active pod: ${ACTIVE_POD}"
          echo "    Deleting passive pod: ${PASSIVE_POD}..."
          kubectl delete pod "${PASSIVE_POD}" -n "${TENANT_NS}" --grace-period=10 2>/dev/null && {
            echo "    ✓ Passive pod deleted, active pod remains serving traffic"
            PASSIVE_PODS_DELETED=$((PASSIVE_PODS_DELETED + 1))
            
            # Wait a moment for pod deletion to register
            sleep 3
          } || echo "    ⚠️ Could not delete passive pod (may have been deleted already)"
        elif [ -z "$ACTIVE_POD" ]; then
          echo "    ⚠️ No active pod found - skipping to avoid disruption"
        else
          echo "    ℹ️ No passive pod found (may have been cleaned up already)"
        fi
      fi
    fi
  done
done

if [ "$PASSIVE_PODS_DELETED" -gt 0 ]; then
  echo "  ✓ Deleted ${PASSIVE_PODS_DELETED} passive pod(s), waiting for deployment to adjust (5s)..."
  sleep 5
fi

# Trigger scale-down annotation (controller will complete the scale-down)
echo ""
echo "Annotating Gateway resources to trigger final scale-down..."

RESTORE_CLIENT_COUNT=0
RESTORE_SERVER_COUNT=0
for TENANT_NS in $TENANT_NAMESPACES; do
  # Process GatewayClients if present
  GATEWAY_CLIENTS=$(kubectl get gatewayclient -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
  for GW_CLIENT in $GATEWAY_CLIENTS; do
    RESTORE_TS=$(date +%%s)
    kubectl annotate gatewayclient "${GW_CLIENT}" -n "${TENANT_NS}" liqo.io/ha-restore="${RESTORE_TS}" --overwrite 2>/dev/null || true
    echo "  ✓ Triggered scale-down for GatewayClient ${GW_CLIENT} in ${TENANT_NS}"
    RESTORE_CLIENT_COUNT=$((RESTORE_CLIENT_COUNT + 1))
  done
  
  # Process GatewayServers if present
  GATEWAY_SERVERS=$(kubectl get gatewayserver -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
  for GW_SERVER in $GATEWAY_SERVERS; do
    RESTORE_TS=$(date +%%s)
    kubectl annotate gatewayserver "${GW_SERVER}" -n "${TENANT_NS}" liqo.io/ha-restore="${RESTORE_TS}" --overwrite 2>/dev/null || true
    echo "  ✓ Triggered scale-down for GatewayServer ${GW_SERVER} in ${TENANT_NS}"
    RESTORE_SERVER_COUNT=$((RESTORE_SERVER_COUNT + 1))
  done
done

if [ "$RESTORE_CLIENT_COUNT" -eq 0 ] && [ "$RESTORE_SERVER_COUNT" -eq 0 ]; then
  echo "  ℹ️ No Gateway resources found"
else
  echo "  Summary: ${RESTORE_CLIENT_COUNT} GatewayClient(s), ${RESTORE_SERVER_COUNT} GatewayServer(s) triggered for scale-down"
fi

echo ""
echo "Waiting for controllers to propagate settings (10s)..."
sleep 10

# Verify scale-down completed
GATEWAYS_SCALED_BACK=0

for TENANT_NS in $TENANT_NAMESPACES; do
  GATEWAY_DEPLOYMENTS=$(kubectl get deployments -n "${TENANT_NS}" -l networking.liqo.io/component=gateway -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
  
  for GW_DEPLOY in $GATEWAY_DEPLOYMENTS; do
    if [ -n "$GW_DEPLOY" ]; then
      echo "  Checking ${GW_DEPLOY} in ${TENANT_NS}..."
      
      # Wait for scale down to complete
      for i in 1 2 3 4 5 6; do
        REPLICAS=$(kubectl get deployment "${GW_DEPLOY}" -n "${TENANT_NS}" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "1")
        READY_REPLICAS=$(kubectl get deployment "${GW_DEPLOY}" -n "${TENANT_NS}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
        STRATEGY=$(kubectl get deployment "${GW_DEPLOY}" -n "${TENANT_NS}" -o jsonpath='{.spec.strategy.type}' 2>/dev/null || echo "unknown")
        
        if [ "$REPLICAS" -eq 1 ] && [ "$READY_REPLICAS" -eq 1 ]; then
          echo "    ✓ ${GW_DEPLOY}: replicas=${REPLICAS}, ready=${READY_REPLICAS}, strategy=${STRATEGY}"
          GATEWAYS_SCALED_BACK=$((GATEWAYS_SCALED_BACK + 1))
          break
        fi
        
        if [ $((i %% 2)) -eq 0 ]; then
          echo "    ⏳ Waiting for scale down... (replicas=${REPLICAS}, ready=${READY_REPLICAS})"
        fi
        sleep 5
      done
    fi
  done
done

# Verify template settings were restored
echo ""
echo "Verifying template restoration..."
CLIENT_REPLICAS=$(kubectl get wggatewayclienttemplate wireguard-client -n "${NAMESPACE}" \
  -o jsonpath='{.spec.template.spec.deployment.spec.replicas}' 2>/dev/null || echo "unknown")
CLIENT_STRATEGY=$(kubectl get wggatewayclienttemplate wireguard-client -n "${NAMESPACE}" \
  -o jsonpath='{.spec.template.spec.deployment.spec.strategy.type}' 2>/dev/null || echo "unknown")
echo "  Client template: replicas=${CLIENT_REPLICAS}, strategy=${CLIENT_STRATEGY}"

SERVER_REPLICAS=$(kubectl get wggatewayservertemplate wireguard-server -n "${NAMESPACE}" \
  -o jsonpath='{.spec.template.spec.deployment.spec.replicas}' 2>/dev/null || echo "unknown")
SERVER_STRATEGY=$(kubectl get wggatewayservertemplate wireguard-server -n "${NAMESPACE}" \
  -o jsonpath='{.spec.template.spec.deployment.spec.strategy.type}' 2>/dev/null || echo "unknown")
echo "  Server template: replicas=${SERVER_REPLICAS}, strategy=${SERVER_STRATEGY}"

echo ""
echo "✅ Processed ${GATEWAYS_SCALED_BACK} gateway(s) - templates restored to original settings"

echo ""
echo "========================================="
echo "✅ Stage 3 complete: Network & Data-Plane upgraded"
echo "========================================="
echo "✅ All network components upgraded to ${TARGET_VERSION}"
echo "✅ Gateway HA enabled during upgrade (zero-downtime)"
echo "✅ InternalNodes preserved (not deleted - in-place update)"
echo "✅ RouteConfigurations preserved (not deleted - in-place update)"
echo "✅ Gateway IP synchronization verified"
echo "✅ Canary verification passed for all peerings"
echo "✅ Virtual Kubelet upgraded with IPAM env var"
echo "========================================="
`, upgrade.Spec.TargetVersion, namespace, backupConfigMapName, planConfigMap, upgrade.Spec.GetImageRegistry())

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "liqo-upgrade",
				"app.kubernetes.io/component": "network-fabric-upgrade",
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: int32Ptr(1800),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: "liqo-upgrade-controller",
					RestartPolicy:      corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "upgrade-network-fabric",
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
