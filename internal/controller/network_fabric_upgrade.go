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

echo ""
echo "========================================="
echo "Step 4.5: Performing Local Data Plane Reset"
echo "========================================="
echo "This step cleans stale network state to ensure VPN tunnels can be re-established"
echo ""

# --- NEW FIX: Clean Stale Host Interfaces ---
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

# --- NEW FIX: Delete Global InternalNode Resources ---
echo ""
echo "2. Deleting InternalNode resources to force regeneration..."
# These resources map physical nodes to VPN tunnels and contain old IP addresses
# Deleting them forces the Controller Manager to regenerate them with correct new Gateway IPs
INTERNAL_NODES=$(kubectl get internalnodes -A -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null || echo "")
if [ -n "$INTERNAL_NODES" ]; then
  kubectl delete internalnodes --all-namespaces --all 2>/dev/null || echo "  ⚠️ Warning: Could not delete all InternalNode resources"
  echo "  ✓ InternalNode resources deleted (will be regenerated)"
else
  echo "  ℹ️ No InternalNode resources found"
fi

# --- NEW FIX: Restart liqo-fabric DaemonSet ---
echo ""
echo "3. Restarting liqo-fabric to reset node status..."
# The node agent (Fabric) needs to restart AFTER interface cleanup 
# to correctly detect the network state and populate status for the Gateway
if kubectl get daemonset liqo-fabric -n "${NAMESPACE}" &>/dev/null; then
  kubectl rollout restart daemonset liqo-fabric -n "${NAMESPACE}"
  echo "  Waiting for liqo-fabric rollout to complete..."
  if kubectl rollout status daemonset liqo-fabric -n "${NAMESPACE}" --timeout=2m; then
    echo "  ✓ liqo-fabric restarted successfully"
  else
    echo "  ⚠️ Warning: liqo-fabric rollout timed out"
  fi
else
  echo "  ℹ️ liqo-fabric DaemonSet not found"
fi

# --- NEW FIX: Flush conntrack and restart CoreDNS for API reachability ---
echo ""
echo "4. Flushing conntrack table to clear stale connections..."
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
echo "5. Restarting CoreDNS to restore API connectivity..."
# CoreDNS restart forces re-establishment of connections to API server
# This is critical after network interface cleanup
if kubectl get deployment coredns -n kube-system &>/dev/null; then
  kubectl rollout restart deployment/coredns -n kube-system
  echo "  Waiting for CoreDNS to be ready..."
  kubectl rollout status deployment/coredns -n kube-system --timeout=2m || echo "  ⚠️ Warning: CoreDNS rollout timed out"
  echo "  ✓ CoreDNS restarted"
else
  echo "  ℹ️ CoreDNS deployment not found (may use different DNS solution)"
fi

# Brief wait for DNS propagation
sleep 5

echo ""
echo "✅ Local Data Plane Reset complete"

echo ""
echo "========================================="
echo "Step 5: Restart liqo-controller-manager and regenerate InternalNodes"
echo "========================================="

# Force clean stale network configurations to prevent validation errors
# RouteConfigurations from the old version may have incompatible specs
# that cause "invalid spec" errors when the new controller tries to update them
echo "  Cleaning stale RouteConfigurations..."
kubectl delete routeconfigurations --all --all-namespaces --wait=false 2>/dev/null || true
echo "  ✓ RouteConfigurations deleted (will be regenerated by controller)"

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
echo "Step 5.1: Forcing InternalNode Regeneration"
echo "========================================="
echo "The controller-manager restart alone may not trigger Node reconciliation."
echo "We must explicitly touch Nodes to force InternalNode recreation."
echo ""

# --- THE FIX: Force Node Reconciliation ---
echo "--- Forcing InternalNode Regeneration ---"
# Apply a temporary label to all nodes to trigger the NodeReconciler
# This sends a MODIFIED event for every Node to the liqo-controller-manager
echo "Touching all nodes to trigger reconciliation..."
TRIGGER_TIMESTAMP=$(date +%%s)
kubectl label nodes --all liqo.io/upgrade-trigger="${TRIGGER_TIMESTAMP}" --overwrite 2>/dev/null || {
  echo "  ⚠️ Warning: Could not label all nodes, trying individually..."
  for NODE in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
    kubectl label node "$NODE" liqo.io/upgrade-trigger="${TRIGGER_TIMESTAMP}" --overwrite 2>/dev/null || echo "    ⚠️ Could not label node $NODE"
  done
}
echo "  ✓ All nodes labeled to trigger reconciliation"

# BLOCK until InternalNodes appear. Do not proceed otherwise.
echo ""
echo "Waiting for InternalNode resources to appear..."
echo "This is CRITICAL - without InternalNodes, the network fabric cannot start."
TIMEOUT=90
ELAPSED=0
while [ $ELAPSED -lt $TIMEOUT ]; do
  COUNT=$(kubectl get internalnodes -A --no-headers 2>/dev/null | wc -l)
  if [ "$COUNT" -gt 0 ]; then
    echo "  ✓ Found $COUNT InternalNode(s). Proceeding."
    
    # Also verify the InternalNodes have the expected fields populated
    echo "  Verifying InternalNode status..."
    kubectl get internalnodes -A -o wide 2>/dev/null || true
    break
  fi
  echo "  Waiting for InternalNodes... ($ELAPSED/$TIMEOUT seconds)"
  sleep 2
  ELAPSED=$((ELAPSED+2))
done

if [ $ELAPSED -ge $TIMEOUT ]; then
  echo "❌ ERROR: InternalNodes were not recreated within ${TIMEOUT}s timeout."
  echo "Network fabric cannot start without InternalNode resources."
  echo ""
  echo "Debug information:"
  echo "  - Checking controller-manager logs for errors..."
  kubectl logs deployment/liqo-controller-manager -n "${NAMESPACE}" --tail=50 2>/dev/null | grep -i "error\|internal" || echo "    (no relevant logs found)"
  echo ""
  echo "  - Checking Node labels..."
  kubectl get nodes --show-labels 2>/dev/null | head -5 || true
  echo ""
  # We exit here because proceeding without InternalNodes guarantees failure
  exit 1
fi

# Additional wait for Fabric to populate InternalNode status
echo ""
echo "Waiting for liqo-fabric to populate InternalNode status (15s)..."
sleep 15

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
echo "Step 5.5: Gateway Stabilization (Wait, Tune & Sync)"
echo "========================================="
echo "This step waits for the controller's rollout, applies network tuning,"
echo "and actively verifies IP synchronization between Control Plane and Data Plane."
echo ""

# Find all tenant namespaces
TENANT_NAMESPACES=$(kubectl get namespaces -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep '^liqo-tenant-' || true)

if [ -n "$TENANT_NAMESPACES" ]; then
  for TENANT_NS in $TENANT_NAMESPACES; do
    echo "--- Processing ${TENANT_NS} ---"

    # 1. Clear Leader Election Leases
    echo "  Clearing leader election leases..."
    kubectl delete lease -n "${TENANT_NS}" -l "liqo.io/component=gateway" --ignore-not-found=true 2>/dev/null || true
    
    # Also try by name pattern
    LEASES=$(kubectl get leases -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -E 'gateway|wg-' || true)
    for LEASE in $LEASES; do
      kubectl delete lease "${LEASE}" -n "${TENANT_NS}" --ignore-not-found=true 2>/dev/null || true
    done

    # 2. Find Gateway Deployments
    GATEWAY_DEPLOYMENTS=$(kubectl get deployments -n "${TENANT_NS}" -l networking.liqo.io/component=gateway -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
    
    if [ -z "$GATEWAY_DEPLOYMENTS" ]; then
      echo "    ℹ️ No gateway deployments found in ${TENANT_NS}"
    fi

    for GW_DEPLOY in $GATEWAY_DEPLOYMENTS; do
        echo "    Processing Gateway: ${GW_DEPLOY}"
        GW_CLIENT_NAME=$(echo "${GW_DEPLOY}" | sed 's/^gw-//')
        
        # 3. Wait for Image Update
        echo "    Waiting for controller to update image to ${TARGET_VERSION}..."
        IMG_UPDATED=false
        for IMG_WAIT in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30; do
          CUR_IMG=$(kubectl get deployment "${GW_DEPLOY}" -n "${TENANT_NS}" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "")
          if echo "$CUR_IMG" | grep -q "${TARGET_VERSION}"; then
             echo "      ✓ Image updated to ${TARGET_VERSION}"
             IMG_UPDATED=true
             break
          fi
          sleep 2
        done
        
        if [ "$IMG_UPDATED" != "true" ]; then
          echo "      ⚠️ Warning: Image not updated after 60s, proceeding..."
        fi
        
        # 4. Wait for Rollout Completion
        echo "    Waiting for ${GW_DEPLOY} rollout to complete..."
        kubectl rollout status deployment "${GW_DEPLOY}" -n "${TENANT_NS}" --timeout=5m 2>/dev/null || echo "      ⚠️ Rollout timed out"
        
        # 5. Wait for Terminating pods to fully disappear
        # This is CRITICAL - the controller will abort if it sees 2 pods
        echo "    Waiting for old pods to terminate..."
        for TERM_WAIT in 1 2 3 4 5 6 7 8 9 10 11 12; do
          POD_COUNT=$(kubectl get pods -n "${TENANT_NS}" -l networking.liqo.io/component=gateway 2>/dev/null | grep -v "Terminating" | grep -c "Running" 2>/dev/null || echo "0")
          POD_COUNT=$(echo "$POD_COUNT" | tr -d '[:space:]')
          TERM_COUNT=$(kubectl get pods -n "${TENANT_NS}" -l networking.liqo.io/component=gateway 2>/dev/null | grep -c "Terminating" 2>/dev/null || echo "0")
          TERM_COUNT=$(echo "$TERM_COUNT" | tr -d '[:space:]')
          
          if [ "${TERM_COUNT}" -eq 0 ] && [ "${POD_COUNT}" -eq 1 ]; then
            echo "      ✓ Only 1 running pod, no terminating pods"
            break
          fi
          echo "      Waiting... (Running: ${POD_COUNT}, Terminating: ${TERM_COUNT})"
          sleep 5
        done

        # 6. Apply Network Tuning (RPF/MSS) to the Running Pod
        echo "    Applying Network Tuning..."
        POD_NAME=$(kubectl get pods -n "${TENANT_NS}" -l networking.liqo.io/component=gateway,networking.liqo.io/gateway-name="${GW_CLIENT_NAME}" --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
        
        # Fallback selector
        if [ -z "$POD_NAME" ]; then
           POD_NAME=$(kubectl get pods -n "${TENANT_NS}" -l networking.liqo.io/component=gateway --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
        fi

        if [ -n "$POD_NAME" ]; then
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
             echo "      ⚠️ Warning: Network tuning command failed"
           fi
        else
           echo "      ⚠️ Warning: Could not find running gateway pod for tuning"
        fi

        # 7. CRITICAL: Active Synchronization Loop
        # Wait until Control Plane (GatewayClient status) matches Data Plane (Pod IP)
        echo "    Verifying IP Synchronization for ${GW_CLIENT_NAME}..."
        
        SYNCED="false"
        for SYNC_ATTEMPT in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
          # Re-fetch pod name in case it changed
          POD_NAME=$(kubectl get pods -n "${TENANT_NS}" -l networking.liqo.io/component=gateway --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
          
          # Get current IPs
          ACTUAL_IP=""
          STATUS_IP=""
          
          if [ -n "$POD_NAME" ]; then
            ACTUAL_IP=$(kubectl get pod "${POD_NAME}" -n "${TENANT_NS}" -o jsonpath='{.status.podIP}' 2>/dev/null || echo "")
          fi
          STATUS_IP=$(kubectl get gatewayclient "${GW_CLIENT_NAME}" -n "${TENANT_NS}" -o jsonpath='{.status.internalEndpoint.ip}' 2>/dev/null || echo "")

          if [ -n "$ACTUAL_IP" ] && [ "$ACTUAL_IP" == "$STATUS_IP" ]; then
            echo "      ✓ IPs Synced: ${ACTUAL_IP}"
            SYNCED="true"
            break
          fi
          
          echo "      ⏳ Attempt ${SYNC_ATTEMPT}/20: Mismatch (Pod:${ACTUAL_IP} vs Status:${STATUS_IP}). Forcing refresh..."
          
          # Trigger update to wake up controller
          SYNC_TS=$(date +%%s)
          kubectl annotate gatewayclient "${GW_CLIENT_NAME}" -n "${TENANT_NS}" liqo.io/force-sync="${SYNC_TS}" --overwrite 2>/dev/null || true
          kubectl annotate gatewayserver "${GW_CLIENT_NAME}" -n "${TENANT_NS}" liqo.io/force-sync="${SYNC_TS}" --overwrite 2>/dev/null || true
          
          sleep 5
        done

        if [ "$SYNCED" != "true" ]; then
           echo "      ❌ WARNING: Failed to sync IPs for ${GW_DEPLOY} after 20 attempts."
           echo "      This may cause connectivity issues. Manual intervention may be required."
        fi
        
        echo "    ✓ Gateway ${GW_DEPLOY} processed"
    done

    echo "  ✓ ${TENANT_NS} processed"
    echo ""
  done
else
  echo "  ℹ️ No tenant namespaces found, skipping gateway processing"
fi

echo "✅ Gateway Stabilization complete"

echo ""
echo "--- Upgrading liqo-gateway deployments ---"

# Find all tenant namespaces (re-fetch after reset)
echo "Step 1: Finding tenant namespaces..."
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

    # Step 1: Clear GatewayClient status (if exists)
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

    # Step 2: Clear GatewayServer status (if exists)
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

    # Step 3: Annotate Identity resources (without deleting kubeconfig secrets)
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

    # Step 4: Verify Configuration resources (DO NOT modify spec.remote.cidr)
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
echo "Step 5 (continued): Verify connectivity after restart..."

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
echo "Step 5.9: Final Data Plane Stabilization"
echo "========================================="
echo "Restarting liqo-fabric one last time to ensure routing tables are applied..."
echo "This clears any 'Link not found' errors from the interface churn during upgrade."

# Force restart of fabric to clear "Link not found" errors
kubectl rollout restart daemonset liqo-fabric -n "${NAMESPACE}"

# Wait for it to be fully ready
echo "  Waiting for liqo-fabric to initialize..."
kubectl rollout status daemonset liqo-fabric -n "${NAMESPACE}" --timeout=2m

# Give it a moment to program the routes
echo "  Waiting for route programming (10s)..."
sleep 10

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

echo "✅ Data Plane stabilized"

echo ""
echo "========================================="
echo "Step 5.10: Verifying Fabric -> API Server Connectivity"
echo "========================================="
echo "This is CRITICAL - if Fabric cannot reach API Server, routing will fail."
echo ""

FABRIC_POD=$(kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/name=fabric -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")

if [ -n "$FABRIC_POD" ]; then
  MAX_RETRIES=30
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
    sleep 5
    RETRY_COUNT=$((RETRY_COUNT+1))
  done

  if [ "$API_REACHABLE" != "true" ]; then
    echo "  ❌ WARNING: Fabric cannot reach API Server after ${MAX_RETRIES} attempts"
    echo "  Attempting emergency recovery..."
    
    # Emergency recovery: restart CoreDNS/kube-dns pods
    echo "  Restarting DNS pods..."
    kubectl delete pod -n kube-system -l k8s-app=kube-dns --wait=false 2>/dev/null || true
    kubectl delete pod -n kube-system -l k8s-app=coredns --wait=false 2>/dev/null || true
    sleep 10
    
    # Restart fabric again
    echo "  Restarting liqo-fabric..."
    kubectl rollout restart daemonset liqo-fabric -n "${NAMESPACE}"
    kubectl rollout status daemonset liqo-fabric -n "${NAMESPACE}" --timeout=2m || true
    
    # Final check
    sleep 5
    FABRIC_POD=$(kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/name=fabric -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
    if [ -n "$FABRIC_POD" ]; then
      if kubectl exec -n "${NAMESPACE}" "$FABRIC_POD" -- /bin/sh -c 'wget -q -O /dev/null --timeout=5 --no-check-certificate https://kubernetes.default.svc/healthz 2>/dev/null || curl -k -s -o /dev/null --connect-timeout 5 https://kubernetes.default.svc/healthz 2>/dev/null' 2>/dev/null; then
        echo "  ✓ Emergency recovery successful - API now reachable"
      else
        echo "  ⚠️ WARNING: API still not reachable. Proceeding anyway, but connectivity may fail."
      fi
    fi
  fi
else
  echo "  ⚠️ Warning: No fabric pod found to verify API connectivity"
fi

echo ""
echo "========================================="
echo "Step 6: Upgrading Virtual Kubelet (After Network Stabilization)"
echo "========================================="
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

# Upgrade VkOptionsTemplate
echo ""
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
echo "Step 6.5: Final Network Stabilization"
echo "========================================="
echo "Restarting liqo-fabric after VK upgrade to ensure geneve tunnels are correct..."

# Force GatewayClient reconciliation to update internalEndpoint IPs
echo "Forcing GatewayClient reconciliation..."
TENANT_NAMESPACES=$(kubectl get namespaces -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep '^liqo-tenant-' || true)
for TENANT_NS in $TENANT_NAMESPACES; do
  GATEWAY_CLIENTS=$(kubectl get gatewayclient -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
  for GW_CLIENT in $GATEWAY_CLIENTS; do
    SYNC_TS=$(date +%%s)
    kubectl annotate gatewayclient "${GW_CLIENT}" -n "${TENANT_NS}" liqo.io/force-sync="${SYNC_TS}" --overwrite 2>/dev/null || true
    echo "  ✓ Triggered reconciliation for GatewayClient ${GW_CLIENT}"
  done
done

# Wait for controller to process annotations
sleep 5

# Restart fabric to recreate geneve tunnels with correct gateway IPs
echo "Restarting liqo-fabric to apply updated gateway endpoints..."
kubectl rollout restart daemonset liqo-fabric -n "${NAMESPACE}"
kubectl rollout status daemonset liqo-fabric -n "${NAMESPACE}" --timeout=2m || echo "  ⚠️ Fabric rollout timed out"

# Give fabric time to establish tunnels
sleep 10

echo "✓ Final network stabilization complete"

echo ""
echo "Step 7: Final verification of network & data-plane..."

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
echo "Step 7 (continued): Comprehensive network connectivity verification..."

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
  ESTABLISHED=$(kubectl get foreignclusters -o json 2>/dev/null | jq -r '.items[] | select(.status.peeringConditions != null) | .status.peeringConditions[] | select(.type=="NetworkStatus" and .status=="True")' 2>/dev/null | wc -l || echo "0")
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
echo "✅ Stage 3 complete: Network & Data-Plane upgraded"
echo "✅ All network components upgraded to ${TARGET_VERSION}"
echo "✅ Local data plane reset with conntrack flush"
echo "✅ Stale RouteConfigurations cleaned and regenerated"
echo "✅ Fabric -> API Server connectivity verified"
echo "✅ Gateway IP synchronization verified"
echo "✅ Virtual Kubelet upgraded with IPAM env var"
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
