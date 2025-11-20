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

# Upgrade gateway deployments in tenant namespaces
# The hierarchy is: GatewayClient -> WgGatewayClient -> Deployment
# When templates are upgraded, we need to trigger GatewayClient to recreate WgGatewayClient
echo ""
echo "--- Upgrading liqo-gateway deployments in tenant namespaces ---"

# Re-fetch tenant namespaces for gateway upgrade
TENANT_NAMESPACES=$(kubectl get namespaces -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep '^liqo-tenant-' || true)

GATEWAY_COUNT=0
for TENANT_NS in ${TENANT_NAMESPACES}; do
  echo ""
  echo "Processing tenant namespace: ${TENANT_NS}"

  # Find GatewayClient resources (these reference the templates)
  GATEWAY_CLIENTS=$(kubectl get gatewayclient -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
  GATEWAY_SERVERS=$(kubectl get gatewayserver -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")

  # Process GatewayClient resources - trigger reconciliation by adding annotation
  for GW_CLIENT in ${GATEWAY_CLIENTS}; do
    echo "  Found GatewayClient: ${GW_CLIENT}"
    GATEWAY_COUNT=$((GATEWAY_COUNT + 1))

    # Get the template reference
    TEMPLATE_REF=$(kubectl get gatewayclient "${GW_CLIENT}" -n "${TENANT_NS}" \
      -o jsonpath='{.spec.clientTemplateRef.name}' 2>/dev/null || echo "")
    echo "    Template: ${TEMPLATE_REF}"

    # Trigger reconciliation by adding/updating annotation
    # This will cause the controller to recreate WgGatewayClient from the updated template
    TIMESTAMP=$(date +%%s)
    echo "    Triggering reconciliation with timestamp: ${TIMESTAMP}"
    kubectl annotate gatewayclient "${GW_CLIENT}" -n "${TENANT_NS}" \
      liqo.io/force-recreate="${TIMESTAMP}" \
      --overwrite

    echo "    ✓ GatewayClient annotated to trigger recreation"
  done

  # Process GatewayServer resources
  for GW_SERVER in ${GATEWAY_SERVERS}; do
    echo "  Found GatewayServer: ${GW_SERVER}"
    GATEWAY_COUNT=$((GATEWAY_COUNT + 1))

    # Get the template reference
    TEMPLATE_REF=$(kubectl get gatewayserver "${GW_SERVER}" -n "${TENANT_NS}" \
      -o jsonpath='{.spec.serverTemplateRef.name}' 2>/dev/null || echo "")
    echo "    Template: ${TEMPLATE_REF}"

    # Trigger reconciliation
    TIMESTAMP=$(date +%%s)
    echo "    Triggering reconciliation with timestamp: ${TIMESTAMP}"
    kubectl annotate gatewayserver "${GW_SERVER}" -n "${TENANT_NS}" \
      liqo.io/force-recreate="${TIMESTAMP}" \
      --overwrite

    echo "    ✓ GatewayServer annotated to trigger recreation"
  done

  # If annotation doesn't work, delete and recreate WgGatewayClient resources
  # This forces them to be regenerated from the updated templates
  WGGW_CLIENTS=$(kubectl get wggatewayclient -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
  WGGW_SERVERS=$(kubectl get wggatewayserver -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")

  if [ -n "$WGGW_CLIENTS" ] || [ -n "$WGGW_SERVERS" ]; then
    echo "  Deleting WgGateway resources to trigger recreation from updated templates..."

    for WGGW_CLIENT in ${WGGW_CLIENTS}; do
      echo "    Deleting wggatewayclient: ${WGGW_CLIENT}"
      kubectl delete wggatewayclient "${WGGW_CLIENT}" -n "${TENANT_NS}" --wait=false 2>/dev/null || true
    done

    for WGGW_SERVER in ${WGGW_SERVERS}; do
      echo "    Deleting wggatewayserver: ${WGGW_SERVER}"
      kubectl delete wggatewayserver "${WGGW_SERVER}" -n "${TENANT_NS}" --wait=false 2>/dev/null || true
    done

    echo "  Waiting for controller to recreate WgGateway resources from updated templates..."
    sleep 10

    # Wait for WgGatewayClient/Server to be recreated
    TIMEOUT=60
    ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
      RECREATED_CLIENTS=$(kubectl get wggatewayclient -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
      RECREATED_SERVERS=$(kubectl get wggatewayserver -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")

      if [ -n "$RECREATED_CLIENTS" ] || [ -n "$RECREATED_SERVERS" ]; then
        echo "  ✓ WgGateway resources recreated"
        break
      fi

      sleep 2
      ELAPSED=$((ELAPSED + 2))
    done

    if [ $ELAPSED -ge $TIMEOUT ]; then
      echo "  ⚠️  Warning: WgGateway resources not recreated within timeout"
    fi
  fi

  # Wait for gateway deployments to roll out with new images
  echo "  Waiting for gateway deployments to update..."
  sleep 5

  GATEWAY_DEPLOYMENTS=$(kubectl get deployments -n "${TENANT_NS}" -l networking.liqo.io/component=gateway -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")

  for GW in ${GATEWAY_DEPLOYMENTS}; do
    echo "  Monitoring gateway deployment: ${GW}"

    # Wait for deployment to start updating
    TIMEOUT=60
    ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
      CURRENT_IMAGE=$(kubectl get deployment "${GW}" -n "${TENANT_NS}" \
        -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "")
      if [[ "$CURRENT_IMAGE" == *"${TARGET_VERSION}"* ]]; then
        echo "    ✓ Deployment spec updated to ${TARGET_VERSION}"
        break
      fi
      sleep 2
      ELAPSED=$((ELAPSED + 2))
    done

    if [ $ELAPSED -ge $TIMEOUT ]; then
      echo "    ⚠️  Warning: Deployment spec not updated after ${TIMEOUT}s"
      echo "    Current image: ${CURRENT_IMAGE}"
    fi

    # Wait for rollout to complete
    if kubectl rollout status deployment/"${GW}" -n "${TENANT_NS}" --timeout=5m 2>/dev/null; then
      echo "    ✓ Rollout completed"
    else
      echo "    ⚠️  Warning: Rollout did not complete"
    fi

    # Verify final state
    FINAL_IMAGE=$(kubectl get deployment "${GW}" -n "${TENANT_NS}" \
      -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "")
    echo "    Final image: ${FINAL_IMAGE}"
  done
done

if [ ${GATEWAY_COUNT} -eq 0 ]; then
  echo "  ℹ️  No GatewayClient/GatewayServer resources found in tenant namespaces"
else
  echo ""
  echo "✅ ${GATEWAY_COUNT} gateway resource(s) processed"
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

  for GW_POD in $GATEWAY_PODS; do
    POD_STATUS=$(kubectl get pod "$GW_POD" -n "${TENANT_NS}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
    if [ "$POD_STATUS" == "Running" ]; then
      GATEWAY_HEALTHY=$((GATEWAY_HEALTHY + 1))
    else
      GATEWAY_UNHEALTHY=$((GATEWAY_UNHEALTHY + 1))
      echo "    ⚠️  Gateway pod ${GW_POD} in ${TENANT_NS}: ${POD_STATUS}"
    fi
  done
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
