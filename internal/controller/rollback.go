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

// Rollback
func (r *LiqoUpgradeReconciler) startRollback(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade, reason string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Starting rollback", "reason", reason)

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
		namespace = "liqo"
	}

	script := fmt.Sprintf(`#!/bin/bash
set -e

echo "========================================="
echo "Rolling back to version %s"
echo "========================================="

PREVIOUS_VERSION="%s"
NAMESPACE="%s"

echo "Step 1: Rolling back liqo-controller-manager image..."
PREVIOUS_IMAGE="ghcr.io/liqotech/liqo-controller-manager:${PREVIOUS_VERSION}"
echo "Previous image: ${PREVIOUS_IMAGE}"

kubectl set image deployment/liqo-controller-manager \
  controller-manager="${PREVIOUS_IMAGE}" \
  -n "${NAMESPACE}"

echo ""
echo "Step 2: Waiting for rollback rollout..."
if ! kubectl rollout status deployment/liqo-controller-manager -n "${NAMESPACE}" --timeout=5m; then
  echo "❌ ERROR: Rollback rollout failed!"
  exit 1
fi

echo ""
echo "Step 3: Verifying rollback health..."
if ! kubectl wait --for=condition=available --timeout=2m deployment/liqo-controller-manager -n "${NAMESPACE}"; then
  echo "❌ ERROR: Deployment not healthy after rollback!"
  exit 1
fi

echo ""
echo "Step 4: Verifying version rollback..."
DEPLOYED_VERSION=$(kubectl get deployment liqo-controller-manager -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].image}' | cut -d: -f2)
echo "Deployed version after rollback: ${DEPLOYED_VERSION}"

if [ "${DEPLOYED_VERSION}" != "${PREVIOUS_VERSION}" ]; then
  echo "❌ ERROR: Version mismatch after rollback!"
  exit 1
fi

# Rollback CRDs if needed (based on lastSuccessfulPhase)
LAST_PHASE="%s"

if [ "$LAST_PHASE" = "UpgradingNetworkFabric" ]; then
  echo ""
  echo "Step 5: Rolling back network fabric components..."

  # Find all liqo-tenant-* namespaces for gateway deployments
  TENANT_NAMESPACES=$(kubectl get namespaces -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep '^liqo-tenant-' || true)
  echo "  Found tenant namespaces: ${TENANT_NAMESPACES}"

  # Rollback gateway resources in tenant namespaces first
  echo "  Rolling back gateway resources in tenant namespaces..."
  GATEWAY_COUNT=0

  for TENANT_NS in ${TENANT_NAMESPACES}; do
    echo "    Processing tenant namespace: ${TENANT_NS}"

    # Rollback wggatewayclient resources
    WGGW_CLIENTS=$(kubectl get wggatewayclient -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
    for WGGW_CLIENT in ${WGGW_CLIENTS}; do
      echo "      Rolling back wggatewayclient: ${WGGW_CLIENT}"
      GATEWAY_COUNT=$((GATEWAY_COUNT + 1))

      kubectl patch wggatewayclient "${WGGW_CLIENT}" -n "${TENANT_NS}" --type='json' -p='[
        {"op": "replace", "path": "/spec/deployment/spec/template/spec/containers/0/image", "value": "ghcr.io/liqotech/gateway:'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/deployment/spec/template/spec/containers/1/image", "value": "ghcr.io/liqotech/gateway/wireguard:'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/deployment/spec/template/spec/containers/2/image", "value": "ghcr.io/liqotech/gateway/geneve:'"${PREVIOUS_VERSION}"'"}
      ]' 2>/dev/null || echo "        Warning: Could not patch wggatewayclient"
    done

    # Rollback wggatewayserver resources
    WGGW_SERVERS=$(kubectl get wggatewayserver -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
    for WGGW_SERVER in ${WGGW_SERVERS}; do
      echo "      Rolling back wggatewayserver: ${WGGW_SERVER}"
      GATEWAY_COUNT=$((GATEWAY_COUNT + 1))

      kubectl patch wggatewayserver "${WGGW_SERVER}" -n "${TENANT_NS}" --type='json' -p='[
        {"op": "replace", "path": "/spec/deployment/spec/template/spec/containers/0/image", "value": "ghcr.io/liqotech/gateway:'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/deployment/spec/template/spec/containers/1/image", "value": "ghcr.io/liqotech/gateway/wireguard:'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/deployment/spec/template/spec/containers/2/image", "value": "ghcr.io/liqotech/gateway/geneve:'"${PREVIOUS_VERSION}"'"}
      ]' 2>/dev/null || echo "        Warning: Could not patch wggatewayserver"
    done

    # Also rollback deployments directly as fallback
    GATEWAY_DEPLOYMENTS=$(kubectl get deployments -n "${TENANT_NS}" -l networking.liqo.io/component=gateway -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)

    for GW in ${GATEWAY_DEPLOYMENTS}; do
      echo "      Rolling back gateway deployment: ${GW}"

      kubectl set image deployment/"${GW}" \
        gateway=ghcr.io/liqotech/gateway:${PREVIOUS_VERSION} \
        wireguard=ghcr.io/liqotech/gateway/wireguard:${PREVIOUS_VERSION} \
        geneve=ghcr.io/liqotech/gateway/geneve:${PREVIOUS_VERSION} \
        -n "${TENANT_NS}" 2>/dev/null || echo "        Warning: Could not update deployment"

      kubectl rollout status deployment/"${GW}" -n "${TENANT_NS}" --timeout=5m 2>/dev/null || echo "        Warning: Rollout did not complete"
    done
  done

  if [ ${GATEWAY_COUNT} -gt 0 ]; then
    echo "    ✅ ${GATEWAY_COUNT} gateway resource(s) rolled back"
  fi

  # Rollback core network components
  if kubectl get deployment liqo-ipam -n "${NAMESPACE}" &>/dev/null; then
    echo "  Rolling back liqo-ipam (deployment)..."
    CONTAINER_NAME=$(kubectl get deployment liqo-ipam -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].name}')
    ROLLBACK_IMAGE="ghcr.io/liqotech/ipam:${PREVIOUS_VERSION}"
    kubectl set image deployment/liqo-ipam \
      "${CONTAINER_NAME}=${ROLLBACK_IMAGE}" \
      -n "${NAMESPACE}"
    kubectl rollout status deployment/liqo-ipam -n "${NAMESPACE}" --timeout=3m
    echo "    ✓ liqo-ipam rolled back"
  fi

  if kubectl get deployment liqo-proxy -n "${NAMESPACE}" &>/dev/null; then
    echo "  Rolling back liqo-proxy (deployment)..."
    CONTAINER_NAME=$(kubectl get deployment liqo-proxy -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].name}')
    ROLLBACK_IMAGE="ghcr.io/liqotech/proxy:${PREVIOUS_VERSION}"
    kubectl set image deployment/liqo-proxy \
      "${CONTAINER_NAME}=${ROLLBACK_IMAGE}" \
      -n "${NAMESPACE}"
    kubectl rollout status deployment/liqo-proxy -n "${NAMESPACE}" --timeout=3m
    echo "    ✓ liqo-proxy rolled back"
  fi

  if kubectl get daemonset liqo-fabric -n "${NAMESPACE}" &>/dev/null; then
    echo "  Rolling back liqo-fabric (daemonset)..."
    CONTAINER_NAME=$(kubectl get daemonset liqo-fabric -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].name}')
    ROLLBACK_IMAGE="ghcr.io/liqotech/fabric:${PREVIOUS_VERSION}"
    kubectl set image daemonset/liqo-fabric \
      "${CONTAINER_NAME}=${ROLLBACK_IMAGE}" \
      -n "${NAMESPACE}"
    kubectl rollout status daemonset/liqo-fabric -n "${NAMESPACE}" --timeout=5m
    echo "    ✓ liqo-fabric rolled back"
  fi

  echo "  ✅ Network fabric components rolled back"
fi

if [ "$LAST_PHASE" = "UpgradingControllerManager" ]; then
  echo ""
  echo "Note: Controller manager already rolled back in previous steps"
fi

if [ "$LAST_PHASE" = "UpgradingCRDs" ] || [ "$LAST_PHASE" = "UpgradingControllerManager" ] || [ "$LAST_PHASE" = "UpgradingNetworkFabric" ]; then
  echo ""
  echo "Note: CRD rollback may be needed but is not implemented in this simplified rollback"
  echo "Manual intervention may be required if CRDs changed"
fi

echo ""
echo "✅ Rollback complete"
echo "✅ Controller-manager restored to ${PREVIOUS_VERSION}"
`, upgrade.Status.PreviousVersion, upgrade.Status.PreviousVersion, namespace, upgrade.Status.LastSuccessfulPhase)

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
			TTLSecondsAfterFinished: int32Ptr(300),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: "liqo-upgrade-controller",
					RestartPolicy:      corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "rollback",
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
