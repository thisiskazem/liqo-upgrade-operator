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
	"sigs.k8s.io/controller-runtime/pkg/log"

	upgradev1alpha1 "github.com/thisiskazem/liqo-upgrade-controller/api/v1alpha1"
)

const (
	targetDescriptorGeneratorPrefix = "liqo-td-generator"
	// Using targetDescriptorsConfigMap from liqoupgrade_controller.go
)

// ensureTargetDescriptors ensures that target descriptors exist for both current and target versions.
// If they don't exist, it creates a Job to generate them from Helm charts.
func (r *LiqoUpgradeReconciler) ensureTargetDescriptors(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade, currentVersion, targetVersion, namespace string) error {
	logger := log.FromContext(ctx)
	logger.Info("Ensuring target descriptors exist", "currentVersion", currentVersion, "targetVersion", targetVersion)

	// Check if ConfigMap exists
	configMap := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      targetDescriptorsConfigMap,
		Namespace: namespace,
	}, configMap)

	if errors.IsNotFound(err) {
		// Create empty ConfigMap first
		configMap = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      targetDescriptorsConfigMap,
				Namespace: namespace,
				Labels: map[string]string{
					"app.kubernetes.io/name":      "liqo-upgrade",
					"app.kubernetes.io/component": "target-descriptor",
				},
			},
			Data: map[string]string{},
		}
		if err := r.Create(ctx, configMap); err != nil {
			return fmt.Errorf("failed to create target descriptors ConfigMap: %w", err)
		}
		logger.Info("Created target descriptors ConfigMap")
	} else if err != nil {
		return fmt.Errorf("failed to get target descriptors ConfigMap: %w", err)
	}

	// Check which versions are missing
	versionsToGenerate := []string{}

	if _, exists := configMap.Data[currentVersion+".json"]; !exists {
		logger.Info("Current version descriptor missing", "version", currentVersion)
		versionsToGenerate = append(versionsToGenerate, currentVersion)
	}

	if _, exists := configMap.Data[targetVersion+".json"]; !exists {
		logger.Info("Target version descriptor missing", "version", targetVersion)
		versionsToGenerate = append(versionsToGenerate, targetVersion)
	}

	if len(versionsToGenerate) == 0 {
		logger.Info("All required target descriptors already exist")
		return nil
	}

	// Generate missing descriptors
	for _, version := range versionsToGenerate {
		if err := r.generateAndStoreTargetDescriptor(ctx, upgrade, version, namespace); err != nil {
			return fmt.Errorf("failed to generate target descriptor for %s: %w", version, err)
		}
	}

	return nil
}

// generateAndStoreTargetDescriptor creates a Job to generate the target descriptor for a specific version
func (r *LiqoUpgradeReconciler) generateAndStoreTargetDescriptor(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade, version, namespace string) error {
	logger := log.FromContext(ctx)
	logger.Info("Generating target descriptor", "version", version)

	jobName := fmt.Sprintf("%s-%s-%s", targetDescriptorGeneratorPrefix, upgrade.Name, version)
	// Clean up job name (remove 'v' and dots for k8s naming)
	jobName = sanitizeJobName(jobName)

	// Check if job already exists and completed
	existingJob := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: namespace}, existingJob)
	if err == nil {
		// Job exists
		if existingJob.Status.Succeeded > 0 {
			logger.Info("Target descriptor generator job already completed", "version", version)
			return nil
		}
		if existingJob.Status.Failed > 0 {
			// Delete failed job and retry
			logger.Info("Deleting failed target descriptor generator job", "version", version)
			if err := r.Delete(ctx, existingJob); err != nil {
				return fmt.Errorf("failed to delete failed job: %w", err)
			}
			time.Sleep(2 * time.Second)
		} else {
			// Job still running, wait
			logger.Info("Target descriptor generator job still running, waiting...", "version", version)
			return r.waitForGeneratorJob(ctx, jobName, namespace, version)
		}
	}

	// Create the generator job
	job := r.buildTargetDescriptorGeneratorJob(upgrade, version, namespace, jobName)
	if err := r.Create(ctx, job); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create generator job: %w", err)
		}
	}

	logger.Info("Created target descriptor generator job", "jobName", jobName, "version", version)

	// Wait for job to complete
	return r.waitForGeneratorJob(ctx, jobName, namespace, version)
}

// waitForGeneratorJob waits for the generator job to complete
func (r *LiqoUpgradeReconciler) waitForGeneratorJob(ctx context.Context, jobName, namespace, version string) error {
	logger := log.FromContext(ctx)

	timeout := time.After(5 * time.Minute)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for generator job for version %s", version)
		case <-ticker.C:
			job := &batchv1.Job{}
			if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: namespace}, job); err != nil {
				if errors.IsNotFound(err) {
					continue
				}
				return err
			}

			if job.Status.Succeeded > 0 {
				logger.Info("Target descriptor generator job completed successfully", "version", version)
				return nil
			}
			if job.Status.Failed > 0 {
				return fmt.Errorf("target descriptor generator job failed for version %s", version)
			}
			logger.Info("Waiting for generator job...", "version", version, "active", job.Status.Active)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// buildTargetDescriptorGeneratorJob creates the Job that generates target descriptors using Helm
func (r *LiqoUpgradeReconciler) buildTargetDescriptorGeneratorJob(upgrade *upgradev1alpha1.LiqoUpgrade, version, namespace, jobName string) *batchv1.Job {
	backoffLimit := int32(3)
	ttlSeconds := int32(1800) // Clean up after 30 minutes (same as other upgrade jobs)

	script := fmt.Sprintf(`#!/bin/bash
set -e

VERSION="%s"
NAMESPACE="%s"
CONFIGMAP_NAME="%s"

echo "========================================="
echo "Generating Target Descriptor for ${VERSION}"
echo "========================================="

# Create work directory
WORK_DIR="/tmp/liqo-helm"
mkdir -p "${WORK_DIR}"
cd "${WORK_DIR}"

# Verify required tools
echo "Verifying required tools..."
if ! command -v helm &> /dev/null; then
  echo "❌ ERROR: helm is required but not found"
  exit 1
fi
if ! command -v jq &> /dev/null; then
  echo "❌ ERROR: jq is required but not found"
  exit 1
fi
if ! command -v kubectl &> /dev/null; then
  echo "❌ ERROR: kubectl is required but not found"
  exit 1
fi
echo "✓ All required tools are available"

# Clone only deployments/liqo folder using sparse checkout
echo ""
echo "Fetching Liqo Helm chart for ${VERSION} (sparse checkout)..."
git clone --filter=blob:none --sparse --depth 1 --branch "${VERSION}" https://github.com/liqotech/liqo.git 2>/dev/null && {
  cd liqo
  git sparse-checkout set deployments/liqo
} || {
  echo "Sparse clone failed, trying manual sparse checkout..."
  rm -rf liqo 2>/dev/null
  mkdir -p liqo && cd liqo
  git init -q
  git remote add origin https://github.com/liqotech/liqo.git
  git sparse-checkout init --cone
  git sparse-checkout set deployments/liqo
  git fetch --depth 1 origin "${VERSION}"
  git checkout FETCH_HEAD -q
}
cd "${WORK_DIR}/liqo"

echo "✓ Liqo repository cloned at ${VERSION}"

# Render Helm templates
echo ""
echo "Rendering Helm templates..."
helm template liqo ./deployments/liqo \
  --set tag="${VERSION}" \
  --namespace "${NAMESPACE}" \
  --set networking.enabled=true \
  --set authentication.enabled=true \
  --set offloading.enabled=true \
  --set storage.enabled=false \
  --set proxy.enabled=true \
  > "${WORK_DIR}/rendered.yaml" 2>/dev/null || {
    # Try with minimal values if default fails
    helm template liqo ./deployments/liqo \
  --set tag="${VERSION}" \
      --namespace "${NAMESPACE}" \
      > "${WORK_DIR}/rendered.yaml"
  }

echo "✓ Helm templates rendered"

# Parse rendered YAML and generate target descriptor
echo ""
echo "Parsing rendered manifests..."

# Create Python parser script
cat > "${WORK_DIR}/parse.py" << 'PYEOF'
import yaml
import json
import sys
import os

version = sys.argv[1] if len(sys.argv) > 1 else "unknown"
input_file = sys.argv[2] if len(sys.argv) > 2 else "/tmp/liqo-helm/rendered.yaml"

# Read rendered yaml (multi-document)
with open(input_file, 'r') as f:
    content = f.read()

docs = list(yaml.safe_load_all(content))

components = []
templates = []

# Component mapping (core components with static deployments)
# Note: liqo-gateway is NOT a direct Deployment - gateways are dynamically created from templates
# Container names match the Helm template $config.name values (not the resource names with liqo- prefix)
component_info = {
    'liqo-controller-manager': {'kind': 'Deployment', 'containerName': 'controller-manager'},
    'liqo-crd-replicator': {'kind': 'Deployment', 'containerName': 'crd-replicator'},
    'liqo-webhook': {'kind': 'Deployment', 'containerName': 'webhook'},
    'liqo-ipam': {'kind': 'Deployment', 'containerName': 'ipam'},
    'liqo-proxy': {'kind': 'Deployment', 'containerName': 'proxy'},
    'liqo-fabric': {'kind': 'DaemonSet', 'containerName': 'fabric'},
    'liqo-metric-agent': {'kind': 'Deployment', 'containerName': 'metric-agent'},
    'liqo-telemetry': {'kind': 'CronJob', 'containerName': 'telemetry'},
}

# Template CRD mapping
template_info = {
    'VkOptionsTemplate': ['virtual-kubelet-default'],
    'WgGatewayClientTemplate': ['wireguard-client'],
    'WgGatewayServerTemplate': ['wireguard-server'],
}

def parse_env(env_list):
    """Convert k8s env format to target descriptor format"""
    result = []
    if not env_list:
        return result
    for e in env_list:
        name = e.get('name', '')
        if 'valueFrom' in e:
            vf = e['valueFrom']
            if 'fieldRef' in vf:
                result.append({
                    'name': name,
                    'type': 'fieldRef',
                    'value': vf['fieldRef']['fieldPath']
                })
            elif 'configMapKeyRef' in vf:
                result.append({
                    'name': name,
                    'type': 'configMapKeyRef',
                    'configMapName': vf['configMapKeyRef']['name'],
                    'key': vf['configMapKeyRef']['key']
                })
            elif 'secretKeyRef' in vf:
                result.append({
                    'name': name,
                    'type': 'secretKeyRef',
                    'secretName': vf['secretKeyRef']['name'],
                    'key': vf['secretKeyRef']['key']
                })
        elif 'value' in e:
            result.append({
                'name': name,
                'type': 'value',
                'value': e['value']
            })
    return result

def parse_image(image_str, default_tag):
    """Parse image string into repository and tag"""
    if ':' in image_str:
        repo, tag = image_str.rsplit(':', 1)
    else:
        repo, tag = image_str, default_tag
    return {'repository': repo, 'tag': tag}

def parse_vkoptions_template(doc):
    """Parse VkOptionsTemplate CRD"""
    name = doc.get('metadata', {}).get('name', '')
    spec = doc.get('spec', {})
    
    container_image = spec.get('containerImage', '')
    extra_args = spec.get('extraArgs', [])
    # VkOptionsTemplate doesn't have a command field - VK uses default entrypoint
    
    container = {
        'name': 'virtual-kubelet',
        'image': parse_image(container_image, version),
        'command': [],  # VK uses image entrypoint
        'args': extra_args if extra_args else [],
        'env': []
    }
    
    return {
        'name': name,
        'kind': 'VkOptionsTemplate',
        'namespace': 'liqo',
        'containers': [container]
    }

def parse_wg_gateway_template(doc, kind):
    """Parse WgGatewayClientTemplate or WgGatewayServerTemplate CRD"""
    name = doc.get('metadata', {}).get('name', '')
    
    # Navigate to spec.template.spec.deployment.spec.template.spec.containers
    try:
        containers_list = doc.get('spec', {}).get('template', {}).get('spec', {}).get('deployment', {}).get('spec', {}).get('template', {}).get('spec', {}).get('containers', [])
    except:
        containers_list = []
    
    parsed_containers = []
    for c in containers_list:
        container_name = c.get('name', '')
        image = c.get('image', '')
        command = c.get('command', [])
        args = c.get('args', [])
        env = parse_env(c.get('env', []))
        
        parsed_containers.append({
            'name': container_name,
            'image': parse_image(image, version),
            'command': command if command else [],
            'args': args if args else [],
            'env': env if env else []
        })
    
    return {
        'name': name,
        'kind': kind,
        'namespace': 'liqo',
        'containers': parsed_containers
    }

for doc in docs:
    if not doc:
        continue
    
    kind = doc.get('kind', '')
    name = doc.get('metadata', {}).get('name', '')
    
    # Parse core components
    if name in component_info:
        info = component_info[name]
        if kind != info['kind']:
            continue
        
        # Get container spec
        if kind == 'CronJob':
            containers = doc.get('spec', {}).get('jobTemplate', {}).get('spec', {}).get('template', {}).get('spec', {}).get('containers', [])
        else:
            containers = doc.get('spec', {}).get('template', {}).get('spec', {}).get('containers', [])
        
        if not containers:
            continue
        
        # Find the right container
        container = None
        for c in containers:
            if c.get('name') == info['containerName']:
                container = c
                break
        
        if not container:
            container = containers[0]
        
        image = container.get('image', '')
        command = container.get('command', [])
        args = container.get('args', [])
        env = parse_env(container.get('env', []))
        
        comp = {
            'name': name,
            'kind': kind,
            'namespace': 'liqo',
            'containerName': info['containerName'],
            'image': parse_image(image, version),
            'command': command if command else [],
            'args': args if args else [],
            'env': env if env else []
        }
        
        components.append(comp)
        continue
    
    # Parse template CRDs
    if kind == 'VkOptionsTemplate':
        expected_names = template_info.get('VkOptionsTemplate', [])
        if name in expected_names:
            templates.append(parse_vkoptions_template(doc))
    elif kind == 'WgGatewayClientTemplate':
        expected_names = template_info.get('WgGatewayClientTemplate', [])
        if name in expected_names:
            templates.append(parse_wg_gateway_template(doc, kind))
    elif kind == 'WgGatewayServerTemplate':
        expected_names = template_info.get('WgGatewayServerTemplate', [])
        if name in expected_names:
            templates.append(parse_wg_gateway_template(doc, kind))

# Sort components in a logical order
order = ['liqo-controller-manager', 'liqo-crd-replicator', 'liqo-webhook', 'liqo-ipam', 
         'liqo-proxy', 'liqo-fabric', 'liqo-metric-agent', 'liqo-telemetry']
components.sort(key=lambda x: order.index(x['name']) if x['name'] in order else 999)

# Sort templates
template_order = ['virtual-kubelet-default', 'wireguard-client', 'wireguard-server']
templates.sort(key=lambda x: template_order.index(x['name']) if x['name'] in template_order else 999)

result = {
    'version': version,
    'components': components,
    'templates': templates
}

# Output formatted JSON
print(json.dumps(result, indent=2))
PYEOF

# Run parser
python3 "${WORK_DIR}/parse.py" "${VERSION}" "${WORK_DIR}/rendered.yaml" > "${WORK_DIR}/descriptor.json"

echo "✓ Target descriptor generated"

# Validate JSON
if ! jq . "${WORK_DIR}/descriptor.json" > /dev/null 2>&1; then
  echo "❌ ERROR: Generated descriptor is not valid JSON"
  cat "${WORK_DIR}/descriptor.json"
  exit 1
fi

COMPONENT_COUNT=$(jq '.components | length' "${WORK_DIR}/descriptor.json")
TEMPLATE_COUNT=$(jq '.templates | length' "${WORK_DIR}/descriptor.json")
echo "  Components found: ${COMPONENT_COUNT}"
echo "  Templates found: ${TEMPLATE_COUNT}"

if [ "${COMPONENT_COUNT}" -lt 3 ]; then
  echo "❌ ERROR: Too few components found (expected at least 3)"
  exit 1
fi

# Update ConfigMap with the generated descriptor
echo ""
echo "Updating ConfigMap ${CONFIGMAP_NAME}..."

DESCRIPTOR_JSON=$(cat "${WORK_DIR}/descriptor.json")

# Use kubectl patch to add/update the version key
kubectl get configmap "${CONFIGMAP_NAME}" -n "${NAMESPACE}" -o json | \
  jq --arg key "${VERSION}.json" --arg value "${DESCRIPTOR_JSON}" '.data[$key] = $value' | \
  kubectl apply -f -

echo "✓ ConfigMap updated with ${VERSION} descriptor"

# Verify using jq (jsonpath has issues with dots in version like v1.0.0)
echo ""
echo "Verifying..."
VERSION_KEY="${VERSION}.json"
STORED=$(kubectl get configmap "${CONFIGMAP_NAME}" -n "${NAMESPACE}" -o json 2>/dev/null | jq -r --arg key "$VERSION_KEY" '.data[$key]' 2>/dev/null | jq -r '.version' 2>/dev/null || echo "")
if [ "${STORED}" == "${VERSION}" ]; then
  echo "✓ Verification passed: ${VERSION} descriptor stored successfully"
else
  echo "⚠️ Verification check failed but ConfigMap was updated - continuing anyway"
  # Don't fail - the data was stored, just verification had issues
fi

echo ""
echo "========================================="
echo "✅ Target Descriptor Generation Complete"
echo "========================================="
`, version, namespace, targetDescriptorsConfigMap)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "liqo-upgrade",
				"app.kubernetes.io/component": "target-descriptor-generator",
				"upgrade.liqo.io/upgrade":     upgrade.Name,
				"upgrade.liqo.io/version":     version,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttlSeconds,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyOnFailure,
					ServiceAccountName: "liqo-upgrade-controller",
					Containers: []corev1.Container{
						{
							Name:    "generator",
							Image:   "alpine/k8s:1.28.4", // Image with kubectl, helm, git, python, jq
							Command: []string{"/bin/sh", "-c"},
							Args:    []string{script},
							Env: []corev1.EnvVar{
								{
									Name:  "HOME",
									Value: "/tmp",
								},
							},
						},
					},
				},
			},
		},
	}
}

// sanitizeJobName ensures the job name is valid for Kubernetes
func sanitizeJobName(name string) string {
	// Replace dots and 'v' prefix in version
	result := ""
	for _, c := range name {
		if c == '.' {
			result += "-"
		} else if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' {
			result += string(c)
		} else if c >= 'A' && c <= 'Z' {
			result += string(c + 32) // lowercase
		}
	}
	// Ensure it starts with a letter
	if len(result) > 0 && (result[0] >= '0' && result[0] <= '9') {
		result = "v" + result
	}
	// Truncate if too long
	if len(result) > 63 {
		result = result[:63]
	}
	// Remove trailing hyphens
	for len(result) > 0 && result[len(result)-1] == '-' {
		result = result[:len(result)-1]
	}
	return result
}
