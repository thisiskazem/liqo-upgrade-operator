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

// Rollback - Cumulative rollback strategy:
// - CRD phase fails → Rollback CRDs only
// - Core Control Plane fails → Rollback Core + CRDs
// - Network Fabric fails → Rollback Network + Core + CRDs
func (r *LiqoUpgradeReconciler) startRollback(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade, reason string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Starting cumulative rollback", "reason", reason, "failedPhase", upgrade.Status.Phase)

	// Check if autoRollback is disabled
	if upgrade.Spec.AutoRollback != nil && !*upgrade.Spec.AutoRollback {
		logger.Info("AutoRollback disabled, not rolling back")
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed,
			fmt.Sprintf("Upgrade failed (AutoRollback disabled): %s", reason), nil)
	}

	job := r.buildRollbackJob(upgrade)
	if err := controllerutil.SetControllerReference(upgrade, job, r.Scheme); err != nil {
		return r.fail(ctx, upgrade, fmt.Sprintf("Rollback preparation failed: %v", err))
	}

	if err := r.Create(ctx, job); err != nil {
		if !errors.IsAlreadyExists(err) {
			logger.Error(err, "Failed to create rollback job")
			return r.fail(ctx, upgrade, fmt.Sprintf("Rollback failed to start: %v", err))
		}
	}

	condition := metav1.Condition{
		Type:               string(upgradev1alpha1.ConditionRollbackRequired),
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "UpgradeFailed",
		Message:            reason,
	}

	statusUpdates := map[string]interface{}{
		"conditions": append(upgrade.Status.Conditions, condition),
	}

	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseRollingBack, fmt.Sprintf("Rolling back: %s", reason), statusUpdates)
}

func (r *LiqoUpgradeReconciler) monitorRollback(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	jobName := fmt.Sprintf("%s-%s", rollbackJobPrefix, upgrade.Name)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Spec.Namespace}, job); err != nil {
		logger.Error(err, "Failed to get rollback job")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if job.Status.Succeeded > 0 {
		logger.Info("Rollback completed successfully")
		statusUpdates := map[string]interface{}{
			"rolledBack": true,
		}
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed, "Upgrade failed and rolled back successfully", statusUpdates)
	}

	if job.Status.Failed > 0 {
		statusUpdates := map[string]interface{}{
			"rolledBack": false,
		}
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed, "Upgrade failed AND rollback failed - manual intervention required", statusUpdates)
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *LiqoUpgradeReconciler) buildRollbackJob(upgrade *upgradev1alpha1.LiqoUpgrade) *batchv1.Job {
	jobName := fmt.Sprintf("%s-%s", rollbackJobPrefix, upgrade.Name)
	namespace := upgrade.Spec.Namespace
	if namespace == "" {
		namespace = defaultLiqoNamespace
	}

	// Determine which phase failed to decide rollback scope
	failedPhase := string(upgrade.Status.Phase)

	script := fmt.Sprintf(`#!/bin/bash
set -e

echo "========================================="
echo "CUMULATIVE ROLLBACK TO VERSION %s"
echo "========================================="
echo ""
echo "Failed Phase: %s"
echo "Previous Version: %s"
echo ""

PREVIOUS_VERSION="%s"
NAMESPACE="%s"
FAILED_PHASE="%s"
IMAGE_REGISTRY="%s"
TARGET_DESCRIPTOR_FILE="/etc/liqo-target-descriptors/${PREVIOUS_VERSION}.json"

# Load target descriptor for previous version
if [ -f "$TARGET_DESCRIPTOR_FILE" ]; then
  echo "Loading target descriptor for ${PREVIOUS_VERSION}..."
  TARGET_DESCRIPTOR=$(cat "$TARGET_DESCRIPTOR_FILE")
  echo "  ✓ Target descriptor loaded"
else
  echo "⚠️ WARNING: Target descriptor not found at $TARGET_DESCRIPTOR_FILE"
  echo "  Rollback will only restore images, not env vars or args"
  TARGET_DESCRIPTOR=""
fi

#############################################
# HELPER FUNCTIONS FOR ENV/ARGS RESTORATION
#############################################

# Function to get component info from target descriptor
get_component_info() {
  local COMPONENT_NAME="$1"
  if [ -n "$TARGET_DESCRIPTOR" ]; then
    echo "$TARGET_DESCRIPTOR" | jq -c ".components[] | select(.name==\"$COMPONENT_NAME\")" 2>/dev/null || echo ""
  else
    echo ""
  fi
}

# Function to restore args for a deployment
restore_deployment_args() {
  local COMPONENT_NAME="$1"
  local RESOURCE_KIND="$2"  # deployment or daemonset
  
  local COMPONENT_INFO=$(get_component_info "$COMPONENT_NAME")
  if [ -z "$COMPONENT_INFO" ]; then
    echo "    ℹ️ No descriptor info for $COMPONENT_NAME, skipping args restore"
    return 0
  fi
  
  local CONTAINER_NAME=$(echo "$COMPONENT_INFO" | jq -r '.containerName')
  local TARGET_ARGS=$(echo "$COMPONENT_INFO" | jq -c '.args // []')
  
  if [ "$TARGET_ARGS" != "[]" ] && [ "$TARGET_ARGS" != "null" ]; then
    echo "    Restoring args for $COMPONENT_NAME (container: $CONTAINER_NAME)..."
    
    # Find container index
    local CONTAINER_INDEX=0
    if [ "$RESOURCE_KIND" = "deployment" ]; then
      CONTAINER_INDEX=$(kubectl get deployment "$COMPONENT_NAME" -n "${NAMESPACE}" \
        -o jsonpath='{range .spec.template.spec.containers[*]}{.name}{"\n"}{end}' 2>/dev/null | grep -n "^${CONTAINER_NAME}$" | cut -d: -f1 || echo "1")
      CONTAINER_INDEX=$((CONTAINER_INDEX - 1))
      [ "$CONTAINER_INDEX" -lt 0 ] && CONTAINER_INDEX=0
      
      kubectl patch deployment "$COMPONENT_NAME" -n "${NAMESPACE}" --type=json \
        -p="[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/${CONTAINER_INDEX}/args\",\"value\":${TARGET_ARGS}}]" 2>/dev/null \
        && echo "      ✓ Args restored" \
        || echo "      ⚠️ Could not restore args"
    elif [ "$RESOURCE_KIND" = "daemonset" ]; then
      CONTAINER_INDEX=$(kubectl get daemonset "$COMPONENT_NAME" -n "${NAMESPACE}" \
        -o jsonpath='{range .spec.template.spec.containers[*]}{.name}{"\n"}{end}' 2>/dev/null | grep -n "^${CONTAINER_NAME}$" | cut -d: -f1 || echo "1")
      CONTAINER_INDEX=$((CONTAINER_INDEX - 1))
      [ "$CONTAINER_INDEX" -lt 0 ] && CONTAINER_INDEX=0
      
      kubectl patch daemonset "$COMPONENT_NAME" -n "${NAMESPACE}" --type=json \
        -p="[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/${CONTAINER_INDEX}/args\",\"value\":${TARGET_ARGS}}]" 2>/dev/null \
        && echo "      ✓ Args restored" \
        || echo "      ⚠️ Could not restore args"
    fi
  else
    echo "    ℹ️ No args changes for $COMPONENT_NAME"
  fi
}

# Function to restore env vars for a deployment
restore_deployment_env() {
  local COMPONENT_NAME="$1"
  local RESOURCE_KIND="$2"  # deployment or daemonset
  
  local COMPONENT_INFO=$(get_component_info "$COMPONENT_NAME")
  if [ -z "$COMPONENT_INFO" ]; then
    echo "    ℹ️ No descriptor info for $COMPONENT_NAME, skipping env restore"
    return 0
  fi
  
  local CONTAINER_NAME=$(echo "$COMPONENT_INFO" | jq -r '.containerName')
  local TARGET_ENV=$(echo "$COMPONENT_INFO" | jq -c '.env // []')
  
  if [ "$TARGET_ENV" != "[]" ] && [ "$TARGET_ENV" != "null" ]; then
    echo "    Restoring env vars for $COMPONENT_NAME..."
    
    # Convert our env format to Kubernetes env format
    local K8S_ENV=$(echo "$TARGET_ENV" | jq -c '[.[] | 
      if .type == "value" then
        {name: .name, value: .value}
      elif .type == "fieldRef" then
        {name: .name, valueFrom: {fieldRef: {fieldPath: .value}}}
      elif .type == "configMapKeyRef" then
        {name: .name, valueFrom: {configMapKeyRef: {name: .configMapName, key: .key}}}
      elif .type == "secretKeyRef" then
        {name: .name, valueFrom: {secretKeyRef: {name: .secretName, key: .key}}}
      else
        {name: .name, value: .value}
      end
    ]')
    
    # Find container index
    local CONTAINER_INDEX=0
    if [ "$RESOURCE_KIND" = "deployment" ]; then
      CONTAINER_INDEX=$(kubectl get deployment "$COMPONENT_NAME" -n "${NAMESPACE}" \
        -o jsonpath='{range .spec.template.spec.containers[*]}{.name}{"\n"}{end}' 2>/dev/null | grep -n "^${CONTAINER_NAME}$" | cut -d: -f1 || echo "1")
      CONTAINER_INDEX=$((CONTAINER_INDEX - 1))
      [ "$CONTAINER_INDEX" -lt 0 ] && CONTAINER_INDEX=0
      
      kubectl patch deployment "$COMPONENT_NAME" -n "${NAMESPACE}" --type=json \
        -p="[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/${CONTAINER_INDEX}/env\",\"value\":${K8S_ENV}}]" 2>/dev/null \
        && echo "      ✓ Env vars restored" \
        || echo "      ⚠️ Could not restore env vars"
    elif [ "$RESOURCE_KIND" = "daemonset" ]; then
      CONTAINER_INDEX=$(kubectl get daemonset "$COMPONENT_NAME" -n "${NAMESPACE}" \
        -o jsonpath='{range .spec.template.spec.containers[*]}{.name}{"\n"}{end}' 2>/dev/null | grep -n "^${CONTAINER_NAME}$" | cut -d: -f1 || echo "1")
      CONTAINER_INDEX=$((CONTAINER_INDEX - 1))
      [ "$CONTAINER_INDEX" -lt 0 ] && CONTAINER_INDEX=0
      
      kubectl patch daemonset "$COMPONENT_NAME" -n "${NAMESPACE}" --type=json \
        -p="[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/${CONTAINER_INDEX}/env\",\"value\":${K8S_ENV}}]" 2>/dev/null \
        && echo "      ✓ Env vars restored" \
        || echo "      ⚠️ Could not restore env vars"
    fi
  else
    echo "    ℹ️ No env var changes for $COMPONENT_NAME"
  fi
}

# Function to restore command for a deployment/daemonset
restore_deployment_command() {
  local COMPONENT_NAME="$1"
  local RESOURCE_KIND="$2"  # deployment or daemonset
  
  local COMPONENT_INFO=$(get_component_info "$COMPONENT_NAME")
  if [ -z "$COMPONENT_INFO" ]; then
    echo "    ℹ️ No descriptor info for $COMPONENT_NAME, skipping command restore"
    return 0
  fi
  
  local CONTAINER_NAME=$(echo "$COMPONENT_INFO" | jq -r '.containerName')
  local TARGET_COMMAND=$(echo "$COMPONENT_INFO" | jq -c '.command // []')
  
  if [ "$TARGET_COMMAND" != "[]" ] && [ "$TARGET_COMMAND" != "null" ]; then
    echo "    Restoring command for $COMPONENT_NAME (container: $CONTAINER_NAME)..."
    
    # Find container index
    local CONTAINER_INDEX=0
    if [ "$RESOURCE_KIND" = "deployment" ]; then
      CONTAINER_INDEX=$(kubectl get deployment "$COMPONENT_NAME" -n "${NAMESPACE}" \
        -o jsonpath='{range .spec.template.spec.containers[*]}{.name}{"\n"}{end}' 2>/dev/null | grep -n "^${CONTAINER_NAME}$" | cut -d: -f1 || echo "1")
      CONTAINER_INDEX=$((CONTAINER_INDEX - 1))
      [ "$CONTAINER_INDEX" -lt 0 ] && CONTAINER_INDEX=0
      
      kubectl patch deployment "$COMPONENT_NAME" -n "${NAMESPACE}" --type=json \
        -p="[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/${CONTAINER_INDEX}/command\",\"value\":${TARGET_COMMAND}}]" 2>/dev/null \
        && echo "      ✓ Command restored" \
        || echo "      ⚠️ Could not restore command"
    elif [ "$RESOURCE_KIND" = "daemonset" ]; then
      CONTAINER_INDEX=$(kubectl get daemonset "$COMPONENT_NAME" -n "${NAMESPACE}" \
        -o jsonpath='{range .spec.template.spec.containers[*]}{.name}{"\n"}{end}' 2>/dev/null | grep -n "^${CONTAINER_NAME}$" | cut -d: -f1 || echo "1")
      CONTAINER_INDEX=$((CONTAINER_INDEX - 1))
      [ "$CONTAINER_INDEX" -lt 0 ] && CONTAINER_INDEX=0
      
      kubectl patch daemonset "$COMPONENT_NAME" -n "${NAMESPACE}" --type=json \
        -p="[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/${CONTAINER_INDEX}/command\",\"value\":${TARGET_COMMAND}}]" 2>/dev/null \
        && echo "      ✓ Command restored" \
        || echo "      ⚠️ Could not restore command"
    fi
  else
    echo "    ℹ️ No command changes for $COMPONENT_NAME"
  fi
}

# Function to get template info from target descriptor
get_template_info() {
  local TEMPLATE_NAME="$1"
  if [ -n "$TARGET_DESCRIPTOR" ]; then
    echo "$TARGET_DESCRIPTOR" | jq -c ".templates[] | select(.name==\"$TEMPLATE_NAME\")" 2>/dev/null || echo ""
  else
    echo ""
  fi
}

# Determine rollback scope based on failed phase
# Cumulative rollback: always rollback from the failed phase down to CRDs
ROLLBACK_NETWORK=false
ROLLBACK_CORE=false
ROLLBACK_CRDS=false

case "$FAILED_PHASE" in
  "UpgradingNetworkFabric"|"PhaseNetworkFabric")
    echo "Scope: Network Fabric + Core Control Plane + CRDs"
    ROLLBACK_NETWORK=true
    ROLLBACK_CORE=true
    ROLLBACK_CRDS=true
    ;;
  "UpgradingControllerManager"|"PhaseControllerManager")
    echo "Scope: Core Control Plane + CRDs"
    ROLLBACK_CORE=true
    ROLLBACK_CRDS=true
    ;;
  "UpgradingCRDs"|"PhaseCRDs")
    echo "Scope: CRDs only"
    ROLLBACK_CRDS=true
    ;;
  *)
    echo "Unknown phase: ${FAILED_PHASE}, performing full rollback"
    ROLLBACK_NETWORK=true
    ROLLBACK_CORE=true
    ROLLBACK_CRDS=true
    ;;
esac

echo ""

#############################################
# PHASE 1: ROLLBACK NETWORK FABRIC (if needed)
#############################################
if [ "$ROLLBACK_NETWORK" = "true" ]; then
  echo "========================================="
  echo "PHASE 1: Rolling back Network Fabric"
  echo "========================================="
  echo ""

  # Find all liqo-tenant-* namespaces for gateway deployments
  TENANT_NAMESPACES=$(kubectl get namespaces -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep '^liqo-tenant-' || true)
  echo "Found tenant namespaces: ${TENANT_NAMESPACES:-none}"

  # Rollback gateway resources in tenant namespaces
  GATEWAY_COUNT=0

  for TENANT_NS in ${TENANT_NAMESPACES}; do
    echo ""
    echo "Processing tenant namespace: ${TENANT_NS}"

    # Rollback WgGatewayClientTemplate resources
    WGGW_CLIENT_TEMPLATES=$(kubectl get wggatewayclienttemplate -n "${NAMESPACE}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
    for TEMPLATE in ${WGGW_CLIENT_TEMPLATES}; do
      echo "  Rolling back WgGatewayClientTemplate: ${TEMPLATE}"
      
      # Get template info from target descriptor
      GW_CLIENT_TEMPLATE_INFO=$(get_template_info "wireguard-client")
      
      # Rollback images and labels
      kubectl patch wggatewayclienttemplate "${TEMPLATE}" -n "${NAMESPACE}" --type='json' -p='[
        {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/0/image", "value": "'"${IMAGE_REGISTRY}"'/gateway:'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/1/image", "value": "'"${IMAGE_REGISTRY}"'/gateway/wireguard:'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/2/image", "value": "'"${IMAGE_REGISTRY}"'/gateway/geneve:'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/template/spec/deployment/metadata/labels/app.kubernetes.io~1version", "value": "'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/template/spec/deployment/metadata/labels/helm.sh~1chart", "value": "liqo-'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/metadata/labels/app.kubernetes.io~1version", "value": "'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/metadata/labels/helm.sh~1chart", "value": "liqo-'"${PREVIOUS_VERSION}"'"}
      ]' 2>/dev/null || echo "    Warning: Could not patch template images"
      
      # Restore args, env, and command for each container from target descriptor
      if [ -n "$GW_CLIENT_TEMPLATE_INFO" ]; then
        CONTAINER_COUNT=$(echo "$GW_CLIENT_TEMPLATE_INFO" | jq '.containers | length')
        for i in $(seq 0 $((CONTAINER_COUNT - 1))); do
          CONTAINER_NAME=$(echo "$GW_CLIENT_TEMPLATE_INFO" | jq -r ".containers[$i].name")
          CONTAINER_ARGS=$(echo "$GW_CLIENT_TEMPLATE_INFO" | jq -c ".containers[$i].args // []")
          CONTAINER_ENV=$(echo "$GW_CLIENT_TEMPLATE_INFO" | jq -c ".containers[$i].env // []")
          CONTAINER_COMMAND=$(echo "$GW_CLIENT_TEMPLATE_INFO" | jq -c ".containers[$i].command // []")
          
          echo "    Restoring container $CONTAINER_NAME (index $i)..."
          
          # Restore args
          if [ "$CONTAINER_ARGS" != "[]" ] && [ "$CONTAINER_ARGS" != "null" ]; then
            kubectl patch wggatewayclienttemplate "${TEMPLATE}" -n "${NAMESPACE}" --type='json' \
              -p="[{\"op\": \"replace\", \"path\": \"/spec/template/spec/deployment/spec/template/spec/containers/${i}/args\", \"value\": ${CONTAINER_ARGS}}]" 2>/dev/null \
              && echo "      ✓ Args restored" || echo "      ⚠️ Could not restore args"
          fi
          
          # Restore env (convert to K8s format)
          if [ "$CONTAINER_ENV" != "[]" ] && [ "$CONTAINER_ENV" != "null" ]; then
            K8S_ENV=$(echo "$CONTAINER_ENV" | jq -c '[.[] | 
              if .type == "value" then {name: .name, value: .value}
              elif .type == "fieldRef" then {name: .name, valueFrom: {fieldRef: {fieldPath: .value}}}
              elif .type == "configMapKeyRef" then {name: .name, valueFrom: {configMapKeyRef: {name: .configMapName, key: .key}}}
              else {name: .name, value: .value} end
            ]')
            kubectl patch wggatewayclienttemplate "${TEMPLATE}" -n "${NAMESPACE}" --type='json' \
              -p="[{\"op\": \"replace\", \"path\": \"/spec/template/spec/deployment/spec/template/spec/containers/${i}/env\", \"value\": ${K8S_ENV}}]" 2>/dev/null \
              && echo "      ✓ Env vars restored" || echo "      ⚠️ Could not restore env vars"
          fi
          
          # Restore command
          if [ "$CONTAINER_COMMAND" != "[]" ] && [ "$CONTAINER_COMMAND" != "null" ]; then
            kubectl patch wggatewayclienttemplate "${TEMPLATE}" -n "${NAMESPACE}" --type='json' \
              -p="[{\"op\": \"replace\", \"path\": \"/spec/template/spec/deployment/spec/template/spec/containers/${i}/command\", \"value\": ${CONTAINER_COMMAND}}]" 2>/dev/null \
              && echo "      ✓ Command restored" || echo "      ⚠️ Could not restore command"
          fi
        done
      fi
      echo "    ✓ WgGatewayClientTemplate ${TEMPLATE} rolled back"
    done

    # Rollback WgGatewayServerTemplate resources
    WGGW_SERVER_TEMPLATES=$(kubectl get wggatewayservertemplate -n "${NAMESPACE}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
    for TEMPLATE in ${WGGW_SERVER_TEMPLATES}; do
      echo "  Rolling back WgGatewayServerTemplate: ${TEMPLATE}"
      
      # Get template info from target descriptor
      GW_SERVER_TEMPLATE_INFO=$(get_template_info "wireguard-server")
      
      # Rollback images and labels
      kubectl patch wggatewayservertemplate "${TEMPLATE}" -n "${NAMESPACE}" --type='json' -p='[
        {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/0/image", "value": "'"${IMAGE_REGISTRY}"'/gateway:'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/1/image", "value": "'"${IMAGE_REGISTRY}"'/gateway/wireguard:'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/2/image", "value": "'"${IMAGE_REGISTRY}"'/gateway/geneve:'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/template/spec/deployment/metadata/labels/app.kubernetes.io~1version", "value": "'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/template/spec/deployment/metadata/labels/helm.sh~1chart", "value": "liqo-'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/metadata/labels/app.kubernetes.io~1version", "value": "'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/metadata/labels/helm.sh~1chart", "value": "liqo-'"${PREVIOUS_VERSION}"'"}
      ]' 2>/dev/null || echo "    Warning: Could not patch template images"
      
      # Restore args, env, and command for each container from target descriptor
      if [ -n "$GW_SERVER_TEMPLATE_INFO" ]; then
        CONTAINER_COUNT=$(echo "$GW_SERVER_TEMPLATE_INFO" | jq '.containers | length')
        for i in $(seq 0 $((CONTAINER_COUNT - 1))); do
          CONTAINER_NAME=$(echo "$GW_SERVER_TEMPLATE_INFO" | jq -r ".containers[$i].name")
          CONTAINER_ARGS=$(echo "$GW_SERVER_TEMPLATE_INFO" | jq -c ".containers[$i].args // []")
          CONTAINER_ENV=$(echo "$GW_SERVER_TEMPLATE_INFO" | jq -c ".containers[$i].env // []")
          CONTAINER_COMMAND=$(echo "$GW_SERVER_TEMPLATE_INFO" | jq -c ".containers[$i].command // []")
          
          echo "    Restoring container $CONTAINER_NAME (index $i)..."
          
          # Restore args
          if [ "$CONTAINER_ARGS" != "[]" ] && [ "$CONTAINER_ARGS" != "null" ]; then
            kubectl patch wggatewayservertemplate "${TEMPLATE}" -n "${NAMESPACE}" --type='json' \
              -p="[{\"op\": \"replace\", \"path\": \"/spec/template/spec/deployment/spec/template/spec/containers/${i}/args\", \"value\": ${CONTAINER_ARGS}}]" 2>/dev/null \
              && echo "      ✓ Args restored" || echo "      ⚠️ Could not restore args"
          fi
          
          # Restore env (convert to K8s format)
          if [ "$CONTAINER_ENV" != "[]" ] && [ "$CONTAINER_ENV" != "null" ]; then
            K8S_ENV=$(echo "$CONTAINER_ENV" | jq -c '[.[] | 
              if .type == "value" then {name: .name, value: .value}
              elif .type == "fieldRef" then {name: .name, valueFrom: {fieldRef: {fieldPath: .value}}}
              elif .type == "configMapKeyRef" then {name: .name, valueFrom: {configMapKeyRef: {name: .configMapName, key: .key}}}
              else {name: .name, value: .value} end
            ]')
            kubectl patch wggatewayservertemplate "${TEMPLATE}" -n "${NAMESPACE}" --type='json' \
              -p="[{\"op\": \"replace\", \"path\": \"/spec/template/spec/deployment/spec/template/spec/containers/${i}/env\", \"value\": ${K8S_ENV}}]" 2>/dev/null \
              && echo "      ✓ Env vars restored" || echo "      ⚠️ Could not restore env vars"
          fi
          
          # Restore command
          if [ "$CONTAINER_COMMAND" != "[]" ] && [ "$CONTAINER_COMMAND" != "null" ]; then
            kubectl patch wggatewayservertemplate "${TEMPLATE}" -n "${NAMESPACE}" --type='json' \
              -p="[{\"op\": \"replace\", \"path\": \"/spec/template/spec/deployment/spec/template/spec/containers/${i}/command\", \"value\": ${CONTAINER_COMMAND}}]" 2>/dev/null \
              && echo "      ✓ Command restored" || echo "      ⚠️ Could not restore command"
          fi
        done
      fi
      echo "    ✓ WgGatewayServerTemplate ${TEMPLATE} rolled back"
    done

    # Rollback wggatewayclient resources
    WGGW_CLIENTS=$(kubectl get wggatewayclient -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
    for WGGW_CLIENT in ${WGGW_CLIENTS}; do
      echo "  Rolling back wggatewayclient: ${WGGW_CLIENT}"
      GATEWAY_COUNT=$((GATEWAY_COUNT + 1))

      kubectl patch wggatewayclient "${WGGW_CLIENT}" -n "${TENANT_NS}" --type='json' -p='[
        {"op": "replace", "path": "/spec/deployment/spec/template/spec/containers/0/image", "value": "'"${IMAGE_REGISTRY}"'/gateway:'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/deployment/spec/template/spec/containers/1/image", "value": "'"${IMAGE_REGISTRY}"'/gateway/wireguard:'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/deployment/spec/template/spec/containers/2/image", "value": "'"${IMAGE_REGISTRY}"'/gateway/geneve:'"${PREVIOUS_VERSION}"'"}
      ]' 2>/dev/null || echo "    Warning: Could not patch wggatewayclient"
    done

    # Rollback wggatewayserver resources
    WGGW_SERVERS=$(kubectl get wggatewayserver -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
    for WGGW_SERVER in ${WGGW_SERVERS}; do
      echo "  Rolling back wggatewayserver: ${WGGW_SERVER}"
      GATEWAY_COUNT=$((GATEWAY_COUNT + 1))

      kubectl patch wggatewayserver "${WGGW_SERVER}" -n "${TENANT_NS}" --type='json' -p='[
        {"op": "replace", "path": "/spec/deployment/spec/template/spec/containers/0/image", "value": "'"${IMAGE_REGISTRY}"'/gateway:'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/deployment/spec/template/spec/containers/1/image", "value": "'"${IMAGE_REGISTRY}"'/gateway/wireguard:'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/deployment/spec/template/spec/containers/2/image", "value": "'"${IMAGE_REGISTRY}"'/gateway/geneve:'"${PREVIOUS_VERSION}"'"}
      ]' 2>/dev/null || echo "    Warning: Could not patch wggatewayserver"
    done

    # Rollback gateway deployments directly as fallback
    GATEWAY_DEPLOYMENTS=$(kubectl get deployments -n "${TENANT_NS}" -l networking.liqo.io/component=gateway -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
    for GW in ${GATEWAY_DEPLOYMENTS}; do
      echo "  Rolling back gateway deployment: ${GW}"
      kubectl set image deployment/"${GW}" \
        gateway=${IMAGE_REGISTRY}/gateway:${PREVIOUS_VERSION} \
        wireguard=${IMAGE_REGISTRY}/gateway/wireguard:${PREVIOUS_VERSION} \
        geneve=${IMAGE_REGISTRY}/gateway/geneve:${PREVIOUS_VERSION} \
        -n "${TENANT_NS}" 2>/dev/null || echo "    Warning: Could not update deployment"
      
      # Rollback labels
      kubectl patch deployment "${GW}" -n "${TENANT_NS}" --type=json \
        -p='[{"op":"replace","path":"/metadata/labels/app.kubernetes.io~1version","value":"'"${PREVIOUS_VERSION}"'"},
             {"op":"replace","path":"/metadata/labels/helm.sh~1chart","value":"liqo-'"${PREVIOUS_VERSION}"'"},
             {"op":"replace","path":"/spec/template/metadata/labels/app.kubernetes.io~1version","value":"'"${PREVIOUS_VERSION}"'"},
             {"op":"replace","path":"/spec/template/metadata/labels/helm.sh~1chart","value":"liqo-'"${PREVIOUS_VERSION}"'"}]' 2>/dev/null || echo "    Warning: Could not rollback labels"
      
      kubectl rollout status deployment/"${GW}" -n "${TENANT_NS}" --timeout=5m 2>/dev/null || echo "    Warning: Rollout did not complete"
    done
  done

  echo ""
  echo "Gateway resources rolled back: ${GATEWAY_COUNT}"

  # Rollback liqo-ipam
  if kubectl get deployment liqo-ipam -n "${NAMESPACE}" &>/dev/null; then
    echo ""
    echo "Rolling back liqo-ipam..."
    CONTAINER_NAME=$(kubectl get deployment liqo-ipam -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].name}')
    kubectl set image deployment/liqo-ipam "${CONTAINER_NAME}=${IMAGE_REGISTRY}/ipam:${PREVIOUS_VERSION}" -n "${NAMESPACE}"
    
    # Restore args, env, and command from target descriptor
    restore_deployment_args "liqo-ipam" "deployment"
    restore_deployment_env "liqo-ipam" "deployment"
    restore_deployment_command "liqo-ipam" "deployment"
    
    kubectl patch deployment liqo-ipam -n "${NAMESPACE}" --type=json \
      -p='[{"op":"replace","path":"/metadata/labels/app.kubernetes.io~1version","value":"'"${PREVIOUS_VERSION}"'"},
           {"op":"replace","path":"/metadata/labels/helm.sh~1chart","value":"liqo-'"${PREVIOUS_VERSION}"'"},
           {"op":"replace","path":"/spec/template/metadata/labels/app.kubernetes.io~1version","value":"'"${PREVIOUS_VERSION}"'"},
           {"op":"replace","path":"/spec/template/metadata/labels/helm.sh~1chart","value":"liqo-'"${PREVIOUS_VERSION}"'"}]' 2>/dev/null || true
    kubectl rollout status deployment/liqo-ipam -n "${NAMESPACE}" --timeout=3m
    echo "  ✓ liqo-ipam rolled back"
  fi

  # Rollback liqo-proxy
  if kubectl get deployment liqo-proxy -n "${NAMESPACE}" &>/dev/null; then
    echo ""
    echo "Rolling back liqo-proxy..."
    CONTAINER_NAME=$(kubectl get deployment liqo-proxy -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].name}')
    kubectl set image deployment/liqo-proxy "${CONTAINER_NAME}=${IMAGE_REGISTRY}/proxy:${PREVIOUS_VERSION}" -n "${NAMESPACE}"
    
    # Restore args, env, and command from target descriptor
    restore_deployment_args "liqo-proxy" "deployment"
    restore_deployment_env "liqo-proxy" "deployment"
    restore_deployment_command "liqo-proxy" "deployment"
    
    kubectl patch deployment liqo-proxy -n "${NAMESPACE}" --type=json \
      -p='[{"op":"replace","path":"/metadata/labels/app.kubernetes.io~1version","value":"'"${PREVIOUS_VERSION}"'"},
           {"op":"replace","path":"/metadata/labels/helm.sh~1chart","value":"liqo-'"${PREVIOUS_VERSION}"'"},
           {"op":"replace","path":"/spec/template/metadata/labels/app.kubernetes.io~1version","value":"'"${PREVIOUS_VERSION}"'"},
           {"op":"replace","path":"/spec/template/metadata/labels/helm.sh~1chart","value":"liqo-'"${PREVIOUS_VERSION}"'"}]' 2>/dev/null || true
    kubectl rollout status deployment/liqo-proxy -n "${NAMESPACE}" --timeout=3m
    echo "  ✓ liqo-proxy rolled back"
  fi

  # Rollback liqo-fabric
  if kubectl get daemonset liqo-fabric -n "${NAMESPACE}" &>/dev/null; then
    echo ""
    echo "Rolling back liqo-fabric..."
    CONTAINER_NAME=$(kubectl get daemonset liqo-fabric -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].name}')
    kubectl set image daemonset/liqo-fabric "${CONTAINER_NAME}=${IMAGE_REGISTRY}/fabric:${PREVIOUS_VERSION}" -n "${NAMESPACE}"
    
    # Restore args, env, and command from target descriptor
    restore_deployment_args "liqo-fabric" "daemonset"
    restore_deployment_env "liqo-fabric" "daemonset"
    restore_deployment_command "liqo-fabric" "daemonset"
    
    kubectl patch daemonset liqo-fabric -n "${NAMESPACE}" --type=json \
      -p='[{"op":"replace","path":"/metadata/labels/app.kubernetes.io~1version","value":"'"${PREVIOUS_VERSION}"'"},
           {"op":"replace","path":"/metadata/labels/helm.sh~1chart","value":"liqo-'"${PREVIOUS_VERSION}"'"},
           {"op":"replace","path":"/spec/template/metadata/labels/app.kubernetes.io~1version","value":"'"${PREVIOUS_VERSION}"'"},
           {"op":"replace","path":"/spec/template/metadata/labels/helm.sh~1chart","value":"liqo-'"${PREVIOUS_VERSION}"'"}]' 2>/dev/null || true
    kubectl rollout status daemonset/liqo-fabric -n "${NAMESPACE}" --timeout=5m
    echo "  ✓ liqo-fabric rolled back"
  fi

  echo ""
  echo "✅ Network Fabric rollback complete"
fi

#############################################
# PHASE 2: ROLLBACK CORE CONTROL PLANE (if needed)
#############################################
if [ "$ROLLBACK_CORE" = "true" ]; then
  echo ""
  echo "========================================="
  echo "PHASE 2: Rolling back Core Control Plane"
  echo "========================================="

  # Function to rollback a deployment
  rollback_deployment() {
    local COMPONENT=$1
    local IMAGE_NAME=$2

    if kubectl get deployment "${COMPONENT}" -n "${NAMESPACE}" &>/dev/null; then
      echo ""
      echo "Rolling back ${COMPONENT}..."
      CONTAINER_NAME=$(kubectl get deployment "${COMPONENT}" -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].name}')
      kubectl set image deployment/"${COMPONENT}" "${CONTAINER_NAME}=${IMAGE_REGISTRY}/${IMAGE_NAME}:${PREVIOUS_VERSION}" -n "${NAMESPACE}"
      
      # Restore args, env, and command from target descriptor
      restore_deployment_args "${COMPONENT}" "deployment"
      restore_deployment_env "${COMPONENT}" "deployment"
      restore_deployment_command "${COMPONENT}" "deployment"
      
      # Update labels
      kubectl patch deployment "${COMPONENT}" -n "${NAMESPACE}" --type=json \
        -p='[{"op":"replace","path":"/metadata/labels/app.kubernetes.io~1version","value":"'"${PREVIOUS_VERSION}"'"},
             {"op":"replace","path":"/metadata/labels/helm.sh~1chart","value":"liqo-'"${PREVIOUS_VERSION}"'"},
             {"op":"replace","path":"/spec/template/metadata/labels/app.kubernetes.io~1version","value":"'"${PREVIOUS_VERSION}"'"},
             {"op":"replace","path":"/spec/template/metadata/labels/helm.sh~1chart","value":"liqo-'"${PREVIOUS_VERSION}"'"}]' 2>/dev/null || true
      
      kubectl rollout status deployment/"${COMPONENT}" -n "${NAMESPACE}" --timeout=5m
      echo "  ✓ ${COMPONENT} rolled back"
    else
      echo "  ⚠️ ${COMPONENT} not found, skipping"
    fi
  }

  # Rollback core components
  rollback_deployment "liqo-controller-manager" "liqo-controller-manager"
  rollback_deployment "liqo-crd-replicator" "crd-replicator"
  rollback_deployment "liqo-webhook" "webhook"

  # Rollback liqo-metric-agent (including init container)
  if kubectl get deployment liqo-metric-agent -n "${NAMESPACE}" &>/dev/null; then
    echo ""
    echo "Rolling back liqo-metric-agent..."
    CONTAINER_NAME=$(kubectl get deployment liqo-metric-agent -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].name}')
    kubectl set image deployment/liqo-metric-agent "${CONTAINER_NAME}=${IMAGE_REGISTRY}/metric-agent:${PREVIOUS_VERSION}" -n "${NAMESPACE}"
    
    # Restore args and env from target descriptor
    restore_deployment_args "liqo-metric-agent" "deployment"
    restore_deployment_env "liqo-metric-agent" "deployment"
    
    # Rollback init container (cert-creator)
    INIT_EXISTS=$(kubectl get deployment liqo-metric-agent -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.initContainers[0].name}' 2>/dev/null || echo "")
    if [ -n "$INIT_EXISTS" ]; then
      echo "  Rolling back cert-creator init container..."
      kubectl patch deployment liqo-metric-agent -n "${NAMESPACE}" --type=json \
        -p='[{"op":"replace","path":"/spec/template/spec/initContainers/0/image","value":"'"${IMAGE_REGISTRY}"'/cert-creator:'"${PREVIOUS_VERSION}"'"}]' 2>/dev/null || echo "    Warning: Could not patch init container"
    fi
    
    # Update labels
    kubectl patch deployment liqo-metric-agent -n "${NAMESPACE}" --type=json \
      -p='[{"op":"replace","path":"/metadata/labels/app.kubernetes.io~1version","value":"'"${PREVIOUS_VERSION}"'"},
           {"op":"replace","path":"/metadata/labels/helm.sh~1chart","value":"liqo-'"${PREVIOUS_VERSION}"'"},
           {"op":"replace","path":"/spec/template/metadata/labels/app.kubernetes.io~1version","value":"'"${PREVIOUS_VERSION}"'"},
           {"op":"replace","path":"/spec/template/metadata/labels/helm.sh~1chart","value":"liqo-'"${PREVIOUS_VERSION}"'"}]' 2>/dev/null || true
    
    kubectl rollout status deployment/liqo-metric-agent -n "${NAMESPACE}" --timeout=3m
    echo "  ✓ liqo-metric-agent rolled back"
  fi

  # Rollback liqo-telemetry CronJob
  if kubectl get cronjob liqo-telemetry -n "${NAMESPACE}" &>/dev/null; then
    echo ""
    echo "Rolling back liqo-telemetry CronJob..."
    
    # Update image
    kubectl patch cronjob liqo-telemetry -n "${NAMESPACE}" --type=json \
      -p='[{"op":"replace","path":"/spec/jobTemplate/spec/template/spec/containers/0/image","value":"'"${IMAGE_REGISTRY}"'/telemetry:'"${PREVIOUS_VERSION}"'"}]' 2>/dev/null || echo "  Warning: Could not patch image"
    
    # Restore full args from target descriptor
    TELEMETRY_INFO=$(get_component_info "liqo-telemetry")
    if [ -n "$TELEMETRY_INFO" ]; then
      TELEMETRY_ARGS=$(echo "$TELEMETRY_INFO" | jq -c '.args // []')
      if [ "$TELEMETRY_ARGS" != "[]" ] && [ "$TELEMETRY_ARGS" != "null" ]; then
        echo "    Restoring liqo-telemetry args from target descriptor..."
        kubectl patch cronjob liqo-telemetry -n "${NAMESPACE}" --type=json \
          -p="[{\"op\":\"replace\",\"path\":\"/spec/jobTemplate/spec/template/spec/containers/0/args\",\"value\":${TELEMETRY_ARGS}}]" 2>/dev/null \
          && echo "      ✓ Args restored" \
          || echo "      ⚠️ Could not restore args"
      fi
    else
      # Fallback: just update --liqo-version arg
      for i in 0 1 2 3 4 5; do
        ARG_VAL=$(kubectl get cronjob liqo-telemetry -n "${NAMESPACE}" \
          -o jsonpath="{.spec.jobTemplate.spec.template.spec.containers[0].args[${i}]}" 2>/dev/null || echo "")
        if [[ "${ARG_VAL}" == --liqo-version=* ]]; then
          kubectl patch cronjob liqo-telemetry -n "${NAMESPACE}" --type=json \
            -p='[{"op":"replace","path":"/spec/jobTemplate/spec/template/spec/containers/0/args/'"${i}"'","value":"--liqo-version='"${PREVIOUS_VERSION}"'"}]' 2>/dev/null || true
          break
        fi
      done
    fi
    
    # Update labels
    kubectl patch cronjob liqo-telemetry -n "${NAMESPACE}" --type=json \
      -p='[{"op":"replace","path":"/metadata/labels/app.kubernetes.io~1version","value":"'"${PREVIOUS_VERSION}"'"},
           {"op":"replace","path":"/metadata/labels/helm.sh~1chart","value":"liqo-'"${PREVIOUS_VERSION}"'"},
           {"op":"replace","path":"/spec/jobTemplate/spec/template/metadata/labels/app.kubernetes.io~1version","value":"'"${PREVIOUS_VERSION}"'"},
           {"op":"replace","path":"/spec/jobTemplate/spec/template/metadata/labels/helm.sh~1chart","value":"liqo-'"${PREVIOUS_VERSION}"'"}]' 2>/dev/null || true
    
    echo "  ✓ liqo-telemetry rolled back"
  fi

  echo ""
  echo "✅ Core Control Plane rollback complete"
fi

#############################################
# PHASE 3: ROLLBACK CRDs (if needed)
#############################################
if [ "$ROLLBACK_CRDS" = "true" ]; then
  echo ""
  echo "========================================="
  echo "PHASE 3: Rolling back CRDs"
  echo "========================================="
  echo ""

  GITHUB_API_URL="https://api.github.com/repos/liqotech/liqo/contents/deployments/liqo/charts/liqo-crds/crds?ref=${PREVIOUS_VERSION}"
  RAW_BASE_URL="https://raw.githubusercontent.com/liqotech/liqo/${PREVIOUS_VERSION}/deployments/liqo/charts/liqo-crds/crds"

  echo "Fetching CRD list from GitHub for version ${PREVIOUS_VERSION}..."
  echo "API URL: ${GITHUB_API_URL}"
  echo ""

  # Fetch list of CRD files from GitHub API
  set +o pipefail
  CRD_FILES=$(curl -fsSL "${GITHUB_API_URL}" 2>&1 | grep '"name":' | grep '.yaml"' | cut -d'"' -f4 || true)
  set -o pipefail

  if [ -z "$CRD_FILES" ]; then
    echo "⚠️ WARNING: Failed to fetch CRD list from GitHub for rollback"
    echo "  This could be due to network issues or invalid version tag"
    echo "  CRD rollback will be skipped - manual intervention may be required"
  else
    CRD_COUNT=$(echo "$CRD_FILES" | wc -l)
    echo "Found ${CRD_COUNT} CRD files to restore"
    echo ""

    SUCCESS_COUNT=0
    FAILED_COUNT=0

    for crd_file in $CRD_FILES; do
      echo "Restoring ${crd_file}..."

      set +e
      YAML_CONTENT=$(curl -fsSL "${RAW_BASE_URL}/${crd_file}" 2>&1)
      CURL_EXIT=$?
      set -e

      if [ $CURL_EXIT -ne 0 ]; then
        echo "  ✗ Failed to fetch ${crd_file}"
        FAILED_COUNT=$((FAILED_COUNT + 1))
        continue
      fi

      set +e
      APPLY_OUTPUT=$(echo "$YAML_CONTENT" | kubectl apply --server-side --force-conflicts -f - 2>&1)
      APPLY_EXIT=$?
      set -e

      if [ $APPLY_EXIT -eq 0 ]; then
        SUCCESS_COUNT=$((SUCCESS_COUNT + 1))
        echo "  ✓ ${crd_file} restored"
      else
        FAILED_COUNT=$((FAILED_COUNT + 1))
        echo "  ✗ ${crd_file} failed: ${APPLY_OUTPUT}"
      fi
    done

    echo ""
    echo "CRD Rollback Summary: ${SUCCESS_COUNT} succeeded, ${FAILED_COUNT} failed"

    if [ "$FAILED_COUNT" -gt 0 ]; then
      echo "⚠️ WARNING: Some CRDs failed to rollback"
    fi
  fi

  # Wait for CRDs to be established
  echo ""
  echo "Waiting for CRDs to stabilize..."
  sleep 10

  echo ""
  echo "✅ CRD rollback complete"
fi

#############################################
# PHASE 4: ROLLBACK RBAC CHANGES
#############################################
echo ""
echo "========================================="
echo "PHASE 4: Rolling back RBAC Changes"
echo "========================================="

# Revert liqo-telemetry ClusterRole RBAC fix (remove create/update verbs added during upgrade)
if kubectl get clusterrole liqo-telemetry &>/dev/null; then
  echo "Reverting liqo-telemetry ClusterRole permissions..."
  
  # Restore original verbs (get, list, watch) - remove create/update that were added
  kubectl patch clusterrole liqo-telemetry --type='json' \
    -p='[{"op":"replace","path":"/rules/0/verbs","value":["get","list","watch"]}]' \
    && echo "  ✓ liqo-telemetry ClusterRole reverted to original permissions" \
    || echo "  ⚠️ Warning: Could not revert liqo-telemetry ClusterRole"
else
  echo "  ℹ️ liqo-telemetry ClusterRole not found, skipping"
fi

echo ""
echo "✅ RBAC rollback complete"

#############################################
# PHASE 5: ROLLBACK GATEWAY HA SCALING
#############################################
echo ""
echo "========================================="
echo "PHASE 5: Rolling back Gateway HA Scaling"
echo "========================================="

# If gateways were scaled to 2 replicas during upgrade, scale them back to 1
TENANT_NAMESPACES=$(kubectl get namespaces -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep '^liqo-tenant-' || true)
GATEWAYS_SCALED_BACK=0

for TENANT_NS in $TENANT_NAMESPACES; do
  GATEWAY_DEPLOYMENTS=$(kubectl get deployments -n "${TENANT_NS}" -l networking.liqo.io/component=gateway -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
  
  for GW_DEPLOY in $GATEWAY_DEPLOYMENTS; do
    if [ -n "$GW_DEPLOY" ]; then
      CURRENT_REPLICAS=$(kubectl get deployment "${GW_DEPLOY}" -n "${TENANT_NS}" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "1")
      
      if [ "$CURRENT_REPLICAS" -gt 1 ]; then
        echo "  Scaling ${GW_DEPLOY} in ${TENANT_NS} from ${CURRENT_REPLICAS} to 1 replica..."
        kubectl scale deployment "${GW_DEPLOY}" -n "${TENANT_NS}" --replicas=1 2>/dev/null && {
          echo "    ✓ Scaled back to 1 replica"
          GATEWAYS_SCALED_BACK=$((GATEWAYS_SCALED_BACK + 1))
        } || echo "    ⚠️ Could not scale ${GW_DEPLOY}"
      fi
    fi
  done
done

if [ "$GATEWAYS_SCALED_BACK" -gt 0 ]; then
  echo "✅ Scaled back ${GATEWAYS_SCALED_BACK} gateway(s) to 1 replica"
else
  echo "ℹ️ No gateways needed scaling back"
fi

#############################################
# PHASE 6: ROLLBACK VKOPTIONSTEMPLATE
#############################################
echo ""
echo "========================================="
echo "PHASE 6: Rolling back VkOptionsTemplate"
echo "========================================="

if kubectl get vkoptionstemplate virtual-kubelet-default -n "${NAMESPACE}" &>/dev/null; then
  echo "Reverting VkOptionsTemplate to version ${PREVIOUS_VERSION}..."
  
  # Get template info from target descriptor
  VK_TEMPLATE_INFO=$(get_template_info "virtual-kubelet-default")
  
  # Revert image
  kubectl patch vkoptionstemplate virtual-kubelet-default -n "${NAMESPACE}" \
    --type='json' -p='[
      {"op": "replace", "path": "/spec/containerImage", "value": "'"${IMAGE_REGISTRY}"'/virtual-kubelet:'"${PREVIOUS_VERSION}"'"}
    ]' && echo "  ✓ VkOptionsTemplate image reverted" || echo "  ⚠️ Warning: Could not revert VkOptionsTemplate image"
  
  # Restore extraArgs from target descriptor
  if [ -n "$VK_TEMPLATE_INFO" ]; then
    VK_EXTRA_ARGS=$(echo "$VK_TEMPLATE_INFO" | jq -c '.containers[0].args // []')
    if [ "$VK_EXTRA_ARGS" != "[]" ] && [ "$VK_EXTRA_ARGS" != "null" ]; then
      echo "  Restoring VkOptionsTemplate extraArgs..."
      kubectl patch vkoptionstemplate virtual-kubelet-default -n "${NAMESPACE}" \
        --type='json' -p="[{\"op\": \"replace\", \"path\": \"/spec/extraArgs\", \"value\": ${VK_EXTRA_ARGS}}]" \
        && echo "    ✓ extraArgs restored" \
        || echo "    ⚠️ Could not restore extraArgs"
    else
      echo "  ℹ️ No extraArgs to restore"
    fi
  fi
else
  echo "  ℹ️ VkOptionsTemplate not found, skipping"
fi

#############################################
# PHASE 7: ROLLBACK VIRTUALNODE RESOURCES
#############################################
echo ""
echo "========================================="
echo "PHASE 7: Rolling back VirtualNode Resources"
echo "========================================="

VIRTUALNODES=$(kubectl get virtualnodes -A -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null || echo "")

if [ -n "$VIRTUALNODES" ]; then
  VN_COUNT=0
  while IFS= read -r line; do
    if [ -n "$line" ]; then
      VN_NAMESPACE=$(echo "$line" | awk '{print $1}')
      VN_NAME=$(echo "$line" | awk '{print $2}')

      echo "  Reverting VirtualNode: ${VN_NAME} in namespace ${VN_NAMESPACE}..."
      
      kubectl patch virtualnode "$VN_NAME" -n "$VN_NAMESPACE" \
        --type='json' -p='[
          {"op": "replace", "path": "/spec/template/spec/template/spec/containers/0/image", "value": "'"${IMAGE_REGISTRY}"'/virtual-kubelet:'"${PREVIOUS_VERSION}"'"}
        ]' 2>/dev/null && {
          echo "    ✓ VirtualNode image reverted"
          VN_COUNT=$((VN_COUNT + 1))
        } || echo "    ⚠️ Warning: Could not revert VirtualNode"
    fi
  done <<< "$VIRTUALNODES"
  
  echo "✅ Reverted ${VN_COUNT} VirtualNode(s)"
  
  # Delete VK deployments to force recreation with old image
  if [ "$VN_COUNT" -gt 0 ]; then
    echo "Deleting VK deployments to force recreation..."
    kubectl delete deployments -A -l offloading.liqo.io/component=virtual-kubelet --wait=false 2>/dev/null || true
    echo "  ✓ VK deployments deleted (will be recreated by controller)"
  fi
else
  echo "  ℹ️ No VirtualNode resources found, skipping"
fi

#############################################
# PHASE 8: CLEANUP UPGRADE ANNOTATIONS
#############################################
echo ""
echo "========================================="
echo "PHASE 8: Cleaning up Upgrade Annotations"
echo "========================================="

# Remove upgrade trigger annotations from nodes
echo "Removing upgrade annotations from nodes..."
for NODE in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
  kubectl annotate node "$NODE" liqo.io/upgrade-trigger- 2>/dev/null || true
  kubectl label node "$NODE" liqo.io/upgrade-trigger- 2>/dev/null || true
done
echo "  ✓ Node annotations cleaned"

# Remove upgrade annotations from InternalNodes
echo "Removing upgrade annotations from InternalNodes..."
INTERNAL_NODES=$(kubectl get internalnodes -A -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || echo "")
for INODE in $INTERNAL_NODES; do
  kubectl annotate internalnode "${INODE}" liqo.io/upgrade-trigger- 2>/dev/null || true
done
echo "  ✓ InternalNode annotations cleaned"

# Remove upgrade annotations from RouteConfigurations
echo "Removing upgrade annotations from RouteConfigurations..."
ROUTE_CONFIGS=$(kubectl get routeconfigurations -A -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null || echo "")
if [ -n "$ROUTE_CONFIGS" ]; then
  while IFS= read -r line; do
    if [ -n "$line" ]; then
      RC_NS=$(echo "$line" | awk '{print $1}')
      RC_NAME=$(echo "$line" | awk '{print $2}')
      kubectl annotate routeconfiguration "${RC_NAME}" -n "${RC_NS}" liqo.io/upgrade-trigger- 2>/dev/null || true
    fi
  done <<< "$ROUTE_CONFIGS"
fi
echo "  ✓ RouteConfiguration annotations cleaned"

echo ""
echo "✅ Cleanup complete"

#############################################
# FINAL VERIFICATION
#############################################
echo ""
echo "========================================="
echo "ROLLBACK VERIFICATION"
echo "========================================="
echo ""

echo "Checking control-plane deployments..."
ALL_HEALTHY=true

for component in liqo-controller-manager liqo-crd-replicator liqo-metric-agent liqo-webhook; do
  if kubectl get deployment "${component}" -n "${NAMESPACE}" > /dev/null 2>&1; then
    REPLICAS=$(kubectl get deployment "${component}" -n "${NAMESPACE}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
    DESIRED=$(kubectl get deployment "${component}" -n "${NAMESPACE}" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "1")
    IMAGE=$(kubectl get deployment "${component}" -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "unknown")

    if [ "$REPLICAS" == "$DESIRED" ] && [ "$REPLICAS" != "0" ]; then
      echo "  ✓ ${component}: ${REPLICAS}/${DESIRED} ready, image: ${IMAGE}"
    else
      echo "  ✗ ${component}: ${REPLICAS}/${DESIRED} ready (UNHEALTHY)"
      ALL_HEALTHY=false
    fi
  fi
done

echo ""
echo "Checking network fabric components..."
for component in liqo-ipam liqo-proxy; do
  if kubectl get deployment "${component}" -n "${NAMESPACE}" > /dev/null 2>&1; then
    IMAGE=$(kubectl get deployment "${component}" -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "unknown")
    echo "  ✓ ${component}: ${IMAGE}"
  fi
done

if kubectl get daemonset liqo-fabric -n "${NAMESPACE}" > /dev/null 2>&1; then
  IMAGE=$(kubectl get daemonset liqo-fabric -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "unknown")
  echo "  ✓ liqo-fabric: ${IMAGE}"
fi

echo ""
if [ "$ALL_HEALTHY" = "true" ]; then
  echo "✅ CUMULATIVE ROLLBACK COMPLETED SUCCESSFULLY"
  echo "   All components restored to version ${PREVIOUS_VERSION}"
else
  echo "⚠️ ROLLBACK COMPLETED WITH WARNINGS"
  echo "   Some components may need manual attention"
fi

echo ""
echo "Rollback scope executed:"
echo "  - Network Fabric: ${ROLLBACK_NETWORK}"
echo "  - Core Control Plane: ${ROLLBACK_CORE}"
echo "  - CRDs: ${ROLLBACK_CRDS}"
echo "  - RBAC: true"
echo "  - Gateway HA: true"
echo "  - VkOptionsTemplate: true"
echo "  - VirtualNodes: true"
echo "  - Cleanup annotations: true"
`, upgrade.Status.PreviousVersion, upgrade.Status.Phase, upgrade.Status.PreviousVersion,
		upgrade.Status.PreviousVersion, namespace, failedPhase, upgrade.Spec.GetImageRegistry())

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "liqo-upgrade",
				"app.kubernetes.io/component": "rollback",
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
							Name:    "rollback",
							Image:   "bitnami/kubectl:latest",
							Command: []string{"/bin/bash", "-c", script},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "target-descriptors",
									MountPath: "/etc/liqo-target-descriptors",
									ReadOnly:  true,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "target-descriptors",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: targetDescriptorsConfigMap,
									},
									Optional: func() *bool { b := true; return &b }(),
								},
							},
						},
					},
				},
			},
			BackoffLimit: int32Ptr(0),
		},
	}
}
