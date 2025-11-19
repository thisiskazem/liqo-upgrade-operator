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
		namespace = "liqo"
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

# Load upgrade plan
echo "Loading upgrade plan from ConfigMap ${PLAN_CONFIGMAP}..."
PLAN_JSON=$(kubectl get configmap "${PLAN_CONFIGMAP}" -n "${NAMESPACE}" -o jsonpath='{.data.plan\.json}')

if [ -z "$PLAN_JSON" ]; then
  echo "⚠️  WARNING: Failed to load upgrade plan, will use fallback image upgrades"
  PLAN_JSON='{}'
fi

echo "Upgrade plan loaded"
echo ""

echo "Step 1: Backing up network fabric deployments..."
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
echo "Step 2: Validating WireGuard/Geneve Prerequisites..."

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
echo "Step 3: Upgrading Gateway Templates (MUST happen before gateway instance recreation)..."

# Upgrade WgGatewayClientTemplate
echo "--- Upgrading WgGatewayClientTemplate ---"
if kubectl get wggatewayclienttemplate wireguard-client -n "${NAMESPACE}" &>/dev/null; then
  echo "Updating WgGatewayClientTemplate to version ${TARGET_VERSION}..."

  # Patch the template to update container images
  kubectl patch wggatewayclienttemplate wireguard-client -n "${NAMESPACE}" \
    --type='json' -p='[
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/0/image", "value": "ghcr.io/liqotech/gateway:'"${TARGET_VERSION}"'"},
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/1/image", "value": "ghcr.io/liqotech/gateway/wireguard:'"${TARGET_VERSION}"'"},
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/2/image", "value": "ghcr.io/liqotech/gateway/geneve:'"${TARGET_VERSION}"'"}
    ]' && echo "  ✓ WgGatewayClientTemplate updated" || echo "  ⚠️  Warning: Could not update WgGatewayClientTemplate"
else
  echo "  ℹ️  WgGatewayClientTemplate not found, skipping"
fi

# Upgrade WgGatewayServerTemplate
echo "--- Upgrading WgGatewayServerTemplate ---"
if kubectl get wggatewayservertemplate wireguard-server -n "${NAMESPACE}" &>/dev/null; then
  echo "Updating WgGatewayServerTemplate to version ${TARGET_VERSION}..."

  kubectl patch wggatewayservertemplate wireguard-server -n "${NAMESPACE}" \
    --type='json' -p='[
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/0/image", "value": "ghcr.io/liqotech/gateway:'"${TARGET_VERSION}"'"},
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/1/image", "value": "ghcr.io/liqotech/gateway/wireguard:'"${TARGET_VERSION}"'"},
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/2/image", "value": "ghcr.io/liqotech/gateway/geneve:'"${TARGET_VERSION}"'"}
    ]' && echo "  ✓ WgGatewayServerTemplate updated" || echo "  ⚠️  Warning: Could not update WgGatewayServerTemplate"
else
  echo "  ℹ️  WgGatewayServerTemplate not found, skipping"
fi

# Verify templates were updated
sleep 2
echo "Verifying template updates..."
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
echo "Step 3.5: Upgrading Virtual Kubelet Template and VirtualNodes..."

# Upgrade VkOptionsTemplate
echo "--- Upgrading VkOptionsTemplate ---"
if kubectl get vkoptionstemplate virtual-kubelet-default -n "${NAMESPACE}" &>/dev/null; then
  echo "Updating VkOptionsTemplate to version ${TARGET_VERSION}..."

  # Patch the template to update the virtual-kubelet container image
  kubectl patch vkoptionstemplate virtual-kubelet-default -n "${NAMESPACE}" \
    --type='json' -p='[
      {"op": "replace", "path": "/spec/containerImage", "value": "ghcr.io/liqotech/virtual-kubelet:'"${TARGET_VERSION}"'"}
    ]' && echo "  ✓ VkOptionsTemplate updated" || echo "  ⚠️  Warning: Could not update VkOptionsTemplate"
else
  echo "  ℹ️  VkOptionsTemplate not found, skipping"
fi

# Verify template was updated
sleep 2
echo "Verifying VkOptionsTemplate update..."
VK_TEMPLATE_IMAGE=$(kubectl get vkoptionstemplate virtual-kubelet-default -n "${NAMESPACE}" \
  -o jsonpath='{.spec.containerImage}' 2>/dev/null || echo "not found")
echo "  VkOptionsTemplate image: ${VK_TEMPLATE_IMAGE}"

if [[ "$VK_TEMPLATE_IMAGE" != *"${TARGET_VERSION}"* ]] && [[ "$VK_TEMPLATE_IMAGE" != "not found" ]]; then
  echo "❌ ERROR: VkOptionsTemplate not updated to ${TARGET_VERSION}!"
  exit 1
fi

echo "✅ VkOptionsTemplate upgraded successfully"

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
        # FIX: Corrected path to include the full nested structure of Deployment -> Pod -> Container
        echo "    Patching VirtualNode to update image to ${TARGET_VERSION}..."
        kubectl patch virtualnode "$VN_NAME" -n "$VN_NAMESPACE" \
          --type='json' -p='[
            {"op": "replace", "path": "/spec/template/spec/template/spec/containers/0/image", "value": "ghcr.io/liqotech/virtual-kubelet:'"${TARGET_VERSION}"'"}
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
echo "Step 4: Upgrading network components sequentially with health checks..."

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

# Upgrade liqo-ipam first (less critical, manages IP allocation)
if kubectl get deployment liqo-ipam -n "${NAMESPACE}" &>/dev/null; then
  echo ""
  echo "--- Upgrading liqo-ipam ---"
  echo "Extracting environment variables..."

  # Get current environment variables
  ENV_JSON=$(kubectl get deployment liqo-ipam -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].env}')
  echo "  Current environment variables preserved in deployment spec"

  NEW_IMAGE="ghcr.io/liqotech/ipam:${TARGET_VERSION}"
  echo "New image: ${NEW_IMAGE}"

  CONTAINER_NAME=$(kubectl get deployment liqo-ipam -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].name}')
  kubectl set image deployment/liqo-ipam \
    "${CONTAINER_NAME}=${NEW_IMAGE}" \
    -n "${NAMESPACE}"

  echo "Waiting for rollout..."
  if ! kubectl rollout status deployment/liqo-ipam -n "${NAMESPACE}" --timeout=5m; then
    echo "❌ ERROR: liqo-ipam rollout failed!"
    exit 1
  fi

  echo "Verifying health..."
  if ! kubectl wait --for=condition=available --timeout=2m deployment/liqo-ipam -n "${NAMESPACE}"; then
    echo "❌ ERROR: liqo-ipam not healthy!"
    exit 1
  fi

  echo "✅ liqo-ipam upgraded successfully"

  # Health check after ipam upgrade
  check_component_health "liqo-ipam" "Deployment" "${NAMESPACE}" || {
    echo "❌ ERROR: liqo-ipam health check failed!"
    exit 1
  }
fi

# Upgrade liqo-proxy (Deployment)
if kubectl get deployment liqo-proxy -n "${NAMESPACE}" &>/dev/null; then
  echo ""
  echo "--- Upgrading liqo-proxy ---"
  echo "Extracting environment variables..."

  # Get current environment variables
  ENV_JSON=$(kubectl get deployment liqo-proxy -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].env}')
  echo "  Current environment variables preserved in deployment spec"

  NEW_IMAGE="ghcr.io/liqotech/proxy:${TARGET_VERSION}"
  echo "New image: ${NEW_IMAGE}"

  CONTAINER_NAME=$(kubectl get deployment liqo-proxy -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].name}')
  kubectl set image deployment/liqo-proxy \
    "${CONTAINER_NAME}=${NEW_IMAGE}" \
    -n "${NAMESPACE}"

  echo "Waiting for rollout..."
  if ! kubectl rollout status deployment/liqo-proxy -n "${NAMESPACE}" --timeout=5m; then
    echo "❌ ERROR: liqo-proxy rollout failed!"
    exit 1
  fi

  echo "Verifying health..."
  if ! kubectl wait --for=condition=available --timeout=2m deployment/liqo-proxy -n "${NAMESPACE}"; then
    echo "❌ ERROR: liqo-proxy not healthy!"
    exit 1
  fi

  echo "✅ liqo-proxy upgraded successfully"

  # Health check after proxy upgrade
  check_component_health "liqo-proxy" "Deployment" "${NAMESPACE}" || {
    echo "❌ ERROR: liqo-proxy health check failed!"
    exit 1
  }
fi

# Upgrade liqo-fabric (DaemonSet) - Data plane component
if kubectl get daemonset liqo-fabric -n "${NAMESPACE}" &>/dev/null; then
  echo ""
  echo "--- Upgrading liqo-fabric (DaemonSet - Data Plane) ---"
  echo "⚠️  WARNING: This may cause temporary network disruption"
  echo "Extracting environment variables..."

  # Get current environment variables
  ENV_JSON=$(kubectl get daemonset liqo-fabric -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].env}')
  echo "  Current environment variables preserved in daemonset spec"

  NEW_IMAGE="ghcr.io/liqotech/fabric:${TARGET_VERSION}"
  echo "New image: ${NEW_IMAGE}"

  CONTAINER_NAME=$(kubectl get daemonset liqo-fabric -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].name}')
  kubectl set image daemonset/liqo-fabric \
    "${CONTAINER_NAME}=${NEW_IMAGE}" \
    -n "${NAMESPACE}"

  echo "Waiting for DaemonSet rollout (this may take several minutes)..."
  if ! kubectl rollout status daemonset/liqo-fabric -n "${NAMESPACE}" --timeout=10m; then
    echo "❌ ERROR: liqo-fabric rollout failed!"
    exit 1
  fi

  echo "Verifying all fabric pods..."
  DESIRED=$(kubectl get daemonset liqo-fabric -n "${NAMESPACE}" -o jsonpath='{.status.desiredNumberScheduled}')
  READY=$(kubectl get daemonset liqo-fabric -n "${NAMESPACE}" -o jsonpath='{.status.numberReady}')

  if [ "${DESIRED}" != "${READY}" ]; then
    echo "❌ ERROR: Not all fabric pods ready! Desired: ${DESIRED}, Ready: ${READY}"
    exit 1
  fi

  echo "✅ liqo-fabric upgraded successfully (${READY}/${DESIRED} pods ready)"

  # Health check after fabric upgrade
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
fi

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

    NEW_IMAGE="ghcr.io/liqotech/virtual-kubelet:${TARGET_VERSION}"
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

    NEW_IMAGE="ghcr.io/liqotech/virtual-kubelet:${TARGET_VERSION}"
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

# Upgrade gateway deployments in tenant namespaces with role detection
echo ""
echo "--- Upgrading liqo-gateway deployments with role-based logic ---"

# Detect ForeignCluster roles to determine upgrade strategy
echo "Step 1: Detecting peering roles from ForeignClusters..."
FOREIGN_CLUSTERS=$(kubectl get foreignclusters -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")

if [ -z "$FOREIGN_CLUSTERS" ]; then
  echo "  ℹ️  No ForeignClusters found, skipping gateway upgrade"
else
  echo "  Found ForeignClusters: ${FOREIGN_CLUSTERS}"
  echo ""

  # Process each ForeignCluster to determine role
  for FC in $FOREIGN_CLUSTERS; do
    echo "--- Processing ForeignCluster: ${FC} ---"

    # Simplified role detection based on tenant namespace existence
    TENANT_NS="liqo-tenant-${FC}"
    IS_CONSUMER="false"
    IS_PROVIDER="false"

    # Check if tenant namespace exists (indicates we're hosting remote resources)
    if kubectl get namespace "${TENANT_NS}" > /dev/null 2>&1; then
      # Check for GatewayClient (indicates CONSUMER role)
      if kubectl get gatewayclient -n "${TENANT_NS}" 2>/dev/null | grep -q "${FC}"; then
        IS_CONSUMER="true"
        echo "  Role: CONSUMER (running VirtualKubelet, connecting to remote provider)"
      fi

      # Check for GatewayServer (indicates PROVIDER role)
      if kubectl get gatewayserver -n "${TENANT_NS}" 2>/dev/null | grep -q "${FC}"; then
        IS_PROVIDER="true"
        echo "  Role: PROVIDER (hosting tenant namespace, accepting connections)"
      fi

      if [ "$IS_CONSUMER" == "true" ] && [ "$IS_PROVIDER" == "true" ]; then
        echo "  Role: BIDIRECTIONAL (both consumer and provider)"
      fi
    else
      echo "  ⚠️  Tenant namespace ${TENANT_NS} not found, skipping"
      continue
    fi

    echo ""

    # Apply CONSUMER role fixes
    if [ "$IS_CONSUMER" == "true" ]; then
      echo "  Applying CONSUMER role fixes for ${FC}..."

      # Find GatewayClient resources
      GATEWAY_CLIENTS=$(kubectl get gatewayclient -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")

      for GW_CLIENT in ${GATEWAY_CLIENTS}; do
        echo "    Processing GatewayClient: ${GW_CLIENT}"

        # Step 1: Clear GatewayClient status using status subresource (FIXED: Use merge patch with nulls)
        echo "      Clearing GatewayClient status (using --subresource=status)..."
        kubectl patch gatewayclient "${GW_CLIENT}" -n "${TENANT_NS}" \
          --subresource=status \
          --type=merge \
          -p='{"status":{"clientRef":null, "endpoint":null, "internalEndpoint":null, "secretRef":null}}' || echo "      ⚠️  Status clear failed"

        # Step 2: Delete WgGatewayClient to force recreation
        # FIX: Only delete if it hasn't been recreated recently to avoid flapping
        WGGW_CLIENTS=$(kubectl get wggatewayclient -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
        for WGGW_CLIENT in ${WGGW_CLIENTS}; do
          # Check age of WgGatewayClient
          # FIXED: Escaped percent sign in date format for Go Sprintf (%%s)
          AGE_SECONDS=$(kubectl get wggatewayclient "${WGGW_CLIENT}" -n "${TENANT_NS}" -o jsonpath='{.metadata.creationTimestamp}' | xargs -I {} date -d {} +%%s | xargs -I {} echo "$(date +%%s) - {}" | bc)
          if [ "$AGE_SECONDS" -gt 300 ]; then
             echo "      Deleting old WgGatewayClient: ${WGGW_CLIENT} (Age: ${AGE_SECONDS}s)"
             kubectl delete wggatewayclient "${WGGW_CLIENT}" -n "${TENANT_NS}" --wait=false 2>/dev/null || true
          else
             echo "      Skipping WgGatewayClient deletion (Created recently: ${AGE_SECONDS}s ago)"
          fi
        done

        # Step 3: Fix VK RBAC - Delete Identity Kubeconfig Secret to trigger regeneration
        echo "      Fixing VirtualKubelet RBAC..."
        
        # Find the Identity in this tenant namespace that corresponds to the remote cluster
        IDENTITY_NAME=$(kubectl get identities -n "${TENANT_NS}" -l liqo.io/remote-cluster-id="${FC}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "${FC}")
        
        if kubectl get identity "${IDENTITY_NAME}" -n "${TENANT_NS}" > /dev/null 2>&1; then
            # FORCE REFRESH: Annotate Identity to wake up identity-controller
            echo "        Touching Identity resource '${IDENTITY_NAME}' to ensure controller watch..."
            # FIXED: Escaped percent sign for Go Sprintf
            kubectl annotate identity "${IDENTITY_NAME}" -n "${TENANT_NS}" \
                liqo.io/force-refresh="$(date +%%s)" --overwrite || echo "        ⚠️  Failed to annotate Identity"
                
            KUBECONFIG_SECRET=$(kubectl get identity "${IDENTITY_NAME}" -n "${TENANT_NS}" \
            -o jsonpath='{.status.kubeconfigSecretRef.name}' 2>/dev/null || echo "")

            if [ -n "$KUBECONFIG_SECRET" ]; then
            echo "        Deleting Kubeconfig Secret: ${KUBECONFIG_SECRET}"
            kubectl delete secret "${KUBECONFIG_SECRET}" -n "${TENANT_NS}" --ignore-not-found=true
            echo "        ✓ Secret deleted (identity-controller will regenerate it)"

            # Delete VK pod to force restart with new secret
            VK_PODS=$(kubectl get pods -n "${TENANT_NS}" -l offloading.liqo.io/component=virtual-kubelet \
                -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
            for VK_POD in ${VK_PODS}; do
                echo "        Deleting VK pod: ${VK_POD}"
                kubectl delete pod "${VK_POD}" -n "${TENANT_NS}" --wait=false || true
            done
            else
            echo "        ℹ️  No Kubeconfig Secret found in Identity status"
            fi
        else
            echo "        ⚠️  Identity resource not found for cluster ${FC} in ${TENANT_NS}"
        fi

        echo "      ✓ CONSUMER fixes applied for ${GW_CLIENT}"
      done

      # Wait for WgGatewayClient recreation
      echo "      Waiting for WgGatewayClient recreation..."
      sleep 10

      TIMEOUT=120
      ELAPSED=0
      while [ $ELAPSED -lt $TIMEOUT ]; do
        RECREATED_CLIENTS=$(kubectl get wggatewayclient -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
        if [ -n "$RECREATED_CLIENTS" ]; then
          echo "      ✓ WgGatewayClient recreated: ${RECREATED_CLIENTS}"
          break
        fi
        sleep 5
        ELAPSED=$((ELAPSED + 5))
      done

      if [ $ELAPSED -ge $TIMEOUT ]; then
        echo "      ⚠️  WARNING: WgGatewayClient not recreated within ${TIMEOUT}s"
      fi

      # Wait for gateway deployment rollout
      echo "      Waiting for gateway deployment rollout..."
      GATEWAY_DEPLOYMENTS=$(kubectl get deployments -n "${TENANT_NS}" -l networking.liqo.io/component=gateway \
        -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")

      for GW_DEPLOY in ${GATEWAY_DEPLOYMENTS}; do
        echo "        Monitoring: ${GW_DEPLOY}"
        kubectl rollout status deployment/"${GW_DEPLOY}" -n "${TENANT_NS}" --timeout=3m 2>/dev/null || \
          echo "        ⚠️  Rollout monitoring timed out"

        # Verify image version
        FINAL_IMAGE=$(kubectl get deployment "${GW_DEPLOY}" -n "${TENANT_NS}" \
          -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "")
        echo "        Final image: ${FINAL_IMAGE}"
      done

      echo "    ✅ CONSUMER role upgrade complete for ${FC}"
    fi

    echo ""

    # Apply PROVIDER role fixes
    if [ "$IS_PROVIDER" == "true" ]; then
      echo "  Applying PROVIDER role fixes for ${FC}..."

      # Find GatewayServer resources
      GATEWAY_SERVERS=$(kubectl get gatewayserver -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")

      for GW_SERVER in ${GATEWAY_SERVERS}; do
        echo "    Processing GatewayServer: ${GW_SERVER}"

        # Step 1: Clear GatewayServer status using status subresource (FIXED: Use merge patch with nulls)
        echo "      Clearing GatewayServer status (using --subresource=status)..."
        kubectl patch gatewayserver "${GW_SERVER}" -n "${TENANT_NS}" \
          --subresource=status \
          --type=merge \
          -p='{"status":{"serverRef":null, "endpoint":null, "internalEndpoint":null, "secretRef":null}}' || echo "      ⚠️  Status clear failed"

        # Step 2: Delete WgGatewayServer to force recreation
        WGGW_SERVERS=$(kubectl get wggatewayserver -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
        for WGGW_SERVER in ${WGGW_SERVERS}; do
          # Check age of WgGatewayServer
          # FIXED: Escaped percent sign in date format for Go Sprintf (%%s)
          AGE_SECONDS=$(kubectl get wggatewayserver "${WGGW_SERVER}" -n "${TENANT_NS}" -o jsonpath='{.metadata.creationTimestamp}' | xargs -I {} date -d {} +%%s | xargs -I {} echo "$(date +%%s) - {}" | bc)
          if [ "$AGE_SECONDS" -gt 300 ]; then
             echo "      Deleting old WgGatewayServer: ${WGGW_SERVER}"
             kubectl delete wggatewayserver "${WGGW_SERVER}" -n "${TENANT_NS}" --wait=false 2>/dev/null || true
          else
             echo "      Skipping WgGatewayServer deletion (Created recently)"
          fi
        done

        # Step 3: Fix VK RBAC - Patch Tenant resource to trigger reconciliation
        echo "      Fixing Tenant RBAC..."
        TENANT_RESOURCES=$(kubectl get tenants -A -o json 2>/dev/null | \
          jq -r '.items[] | select(.spec.clusterID == "'${FC}'") | .metadata.namespace + "/" + .metadata.name' || echo "")

        if [ -n "$TENANT_RESOURCES" ]; then
          for TENANT_REF in ${TENANT_RESOURCES}; do
            TENANT_NAMESPACE=$(echo "$TENANT_REF" | cut -d/ -f1)
            TENANT_NAME=$(echo "$TENANT_REF" | cut -d/ -f2)

            echo "        Patching Tenant: ${TENANT_NAME} in ${TENANT_NAMESPACE}"
            TIMESTAMP=$(date +%%s)
            kubectl annotate tenant "${TENANT_NAME}" -n "${TENANT_NAMESPACE}" \
              liqo.io/force-sync="${TIMESTAMP}" \
              --overwrite || echo "        ⚠️  Failed to annotate Tenant"
            echo "        ✓ Tenant annotated to trigger RBAC reconciliation"
          done
        else
          echo "        ℹ️  No Tenant resources found for cluster ${FC}"
        fi

        echo "      ✓ PROVIDER fixes applied for ${GW_SERVER}"
      done

      # Wait for WgGatewayServer recreation
      echo "      Waiting for WgGatewayServer recreation..."
      sleep 10

      TIMEOUT=120
      ELAPSED=0
      while [ $ELAPSED -lt $TIMEOUT ]; do
        RECREATED_SERVERS=$(kubectl get wggatewayserver -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
        if [ -n "$RECREATED_SERVERS" ]; then
          echo "      ✓ WgGatewayServer recreated: ${RECREATED_SERVERS}"
          break
        fi
        sleep 5
        ELAPSED=$((ELAPSED + 5))
      done

      if [ $ELAPSED -ge $TIMEOUT ]; then
        echo "      ⚠️  WARNING: WgGatewayServer not recreated within ${TIMEOUT}s"
      fi

      # Wait for gateway deployment rollout
      echo "      Waiting for gateway deployment rollout..."
      GATEWAY_DEPLOYMENTS=$(kubectl get deployments -n "${TENANT_NS}" -l networking.liqo.io/component=gateway \
        -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")

      for GW_DEPLOY in ${GATEWAY_DEPLOYMENTS}; do
        echo "        Monitoring: ${GW_DEPLOY}"
        kubectl rollout status deployment/"${GW_DEPLOY}" -n "${TENANT_NS}" --timeout=3m 2>/dev/null || \
          echo "        ⚠️  Rollout monitoring timed out"

        # Verify image version
        FINAL_IMAGE=$(kubectl get deployment "${GW_DEPLOY}" -n "${TENANT_NS}" \
          -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "")
        echo "        Final image: ${FINAL_IMAGE}"
      done

      echo "    ✅ PROVIDER role upgrade complete for ${FC}"
    fi

    echo ""
    echo "✅ ForeignCluster ${FC} gateway upgrade complete"
    echo "========================================="
    echo ""
  done
fi

echo ""
echo "Step 5: Final verification of network & data-plane..."

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
  done
done

echo ""
echo "Step 6: Comprehensive network connectivity verification..."

# Check fabric pods can reach API server
echo "  Checking fabric pods API connectivity..."
FABRIC_PODS=$(kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/name=fabric -o jsonpath='{.items[*].metadata.name}')
if [ -n "$FABRIC_PODS" ]; then
  FABRIC_POD=$(echo "$FABRIC_PODS" | awk '{print $1}')
  if kubectl exec -n "${NAMESPACE}" "$FABRIC_POD" -- wget -q -O- --timeout=5 https://kubernetes.default.svc >/dev/null 2>&1; then
    echo "    ✓ Fabric pod can reach Kubernetes API"
  else
    echo "    ⚠️  WARNING: Fabric pod cannot reach Kubernetes API"
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
    FC_STATUS=$(kubectl get foreigncluster "$FC" -o jsonpath='{.status.peeringConditions[?(@.type=="NetworkStatus")].status}' 2>/dev/null || echo "Unknown")
    echo "      - ${FC}: NetworkStatus=${FC_STATUS}"
  done

  # Count established connections
  ESTABLISHED=$(kubectl get foreignclusters -o json 2>/dev/null | jq -r '.items[].status.peeringConditions[] | select(.type=="NetworkStatus" and .status=="True")' | wc -l || echo "0")
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
  TUNNEL_COUNT=$(kubectl exec -n "${NAMESPACE}" "$FABRIC_POD" -- ip link show type geneve 2>/dev/null | grep -c "geneve" || echo "0")
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

if [ $GATEWAY_HEALTHY -gt 0 ]; then
  echo "    ✓ ${GATEWAY_HEALTHY} gateway pod(s) running"
fi

if [ $GATEWAY_UNHEALTHY -gt 0 ]; then
  echo "    ⚠️  WARNING: ${GATEWAY_UNHEALTHY} gateway pod(s) not running"
fi

echo ""
echo "========================================="
echo "✅ Stage 3 complete: Network & Data-Plane upgraded"
echo "✅ All network components upgraded to ${TARGET_VERSION}"
echo "✅ All critical environment variables and args preserved"
echo "========================================="
`, upgrade.Spec.TargetVersion, namespace, backupConfigMapName, planConfigMap)

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
			TTLSecondsAfterFinished: int32Ptr(300),
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
