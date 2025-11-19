#!/bin/bash

# Setup script for testing liqo-upgrade-operator in a local cluster
# This script creates the necessary ConfigMaps and resources for testing

set -e

NAMESPACE=${LIQO_NAMESPACE:-liqo}

echo "========================================="
echo "Liqo Upgrade Operator - Test Environment Setup"
echo "========================================="
echo ""
echo "Namespace: ${NAMESPACE}"
echo ""

# Check if liqo-controller-manager deployment exists
echo "Step 1: Checking if Liqo is installed..."
if ! kubectl get deployment liqo-controller-manager -n "${NAMESPACE}" > /dev/null 2>&1; then
    echo "❌ ERROR: liqo-controller-manager deployment not found in namespace '${NAMESPACE}'"
    echo ""
    echo "Please install Liqo first before running this setup script."
    echo "Visit https://docs.liqo.io for installation instructions."
    exit 1
fi
echo "✓ Liqo is installed"
echo ""

# Extract CLUSTER_ID from liqo-controller-manager deployment
echo "Step 2: Extracting CLUSTER_ID from liqo-controller-manager..."
CLUSTER_ID=$(kubectl get deployment liqo-controller-manager -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="CLUSTER_ID")].value}' 2>/dev/null || echo "")

# If not found as a direct value, try to get it from a ConfigMap reference
if [ -z "$CLUSTER_ID" ]; then
    echo "  CLUSTER_ID not found as direct value, checking for ConfigMap reference..."
    CONFIGMAP_NAME=$(kubectl get deployment liqo-controller-manager -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="CLUSTER_ID")].valueFrom.configMapKeyRef.name}' 2>/dev/null || echo "")
    CONFIGMAP_KEY=$(kubectl get deployment liqo-controller-manager -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="CLUSTER_ID")].valueFrom.configMapKeyRef.key}' 2>/dev/null || echo "")

    if [ -n "$CONFIGMAP_NAME" ] && [ -n "$CONFIGMAP_KEY" ]; then
        echo "  Found ConfigMap reference: ${CONFIGMAP_NAME}.${CONFIGMAP_KEY}"
        CLUSTER_ID=$(kubectl get configmap "${CONFIGMAP_NAME}" -n "${NAMESPACE}" -o jsonpath="{.data.${CONFIGMAP_KEY}}" 2>/dev/null || echo "")
    fi
fi

# If still not found, try to extract from args
if [ -z "$CLUSTER_ID" ]; then
    echo "  CLUSTER_ID not found in env vars, checking args..."
    ARGS=$(kubectl get deployment liqo-controller-manager -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].args[*]}')
    for arg in $ARGS; do
        if [[ "$arg" == --cluster-id=* ]]; then
            CLUSTER_ID=$(echo "$arg" | cut -d= -f2)
            # Remove $(...) if present
            CLUSTER_ID=$(echo "$CLUSTER_ID" | sed 's/\$(\(.*\))/\1/')
            break
        fi
    done
fi

# If still not found, generate a default
if [ -z "$CLUSTER_ID" ]; then
    echo "  CLUSTER_ID not found, generating default value..."
    CLUSTER_ID="default-cluster-$(openssl rand -hex 4)"
    echo "  ⚠️  WARNING: Could not extract CLUSTER_ID, using generated value: ${CLUSTER_ID}"
else
    echo "  ✓ Found CLUSTER_ID: ${CLUSTER_ID}"
fi
echo ""

# Check if liqo-cluster-id ConfigMap already exists
echo "Step 3: Checking for existing liqo-cluster-id ConfigMap..."
if kubectl get configmap liqo-cluster-id -n "${NAMESPACE}" > /dev/null 2>&1; then
    EXISTING_ID=$(kubectl get configmap liqo-cluster-id -n "${NAMESPACE}" -o jsonpath='{.data.CLUSTER_ID}')
    echo "  ConfigMap already exists with CLUSTER_ID: ${EXISTING_ID}"

    if [ "$EXISTING_ID" != "$CLUSTER_ID" ]; then
        echo "  ⚠️  WARNING: Existing CLUSTER_ID differs from detected value!"
        echo "  Existing: ${EXISTING_ID}"
        echo "  Detected: ${CLUSTER_ID}"
        echo ""
        read -p "  Do you want to update it? (y/N): " -n 1 -r
        echo
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            echo "  Skipping ConfigMap update"
            CLUSTER_ID=$EXISTING_ID
        else
            kubectl patch configmap liqo-cluster-id -n "${NAMESPACE}" --type merge -p "{\"data\":{\"CLUSTER_ID\":\"${CLUSTER_ID}\"}}"
            echo "  ✓ ConfigMap updated"
        fi
    else
        echo "  ✓ ConfigMap already has correct value"
    fi
else
    echo "  ConfigMap does not exist, creating..."
    kubectl create configmap liqo-cluster-id -n "${NAMESPACE}" --from-literal=CLUSTER_ID="${CLUSTER_ID}"
    echo "  ✓ ConfigMap created"
fi
echo ""

# Verify compatibility ConfigMap exists
echo "Step 4: Verifying compatibility ConfigMap..."
if ! kubectl get configmap liqo-compatibility -n "${NAMESPACE}" > /dev/null 2>&1; then
    echo "  ⚠️  WARNING: liqo-compatibility ConfigMap not found"
    echo "  Creating from config/default/compatibility-configmap.yaml..."
    if [ -f config/default/compatibility-configmap.yaml ]; then
        kubectl apply -f config/default/compatibility-configmap.yaml
        echo "  ✓ Compatibility ConfigMap created"
    else
        echo "  ❌ ERROR: config/default/compatibility-configmap.yaml not found"
        echo "  Please ensure you are running this from the liqo-upgrade-operator directory"
        exit 1
    fi
else
    echo "  ✓ Compatibility ConfigMap exists"
fi
echo ""

# Verify target descriptors ConfigMap exists
echo "Step 5: Verifying target descriptors ConfigMap..."
if ! kubectl get configmap liqo-target-descriptors -n "${NAMESPACE}" > /dev/null 2>&1; then
    echo "  ⚠️  WARNING: liqo-target-descriptors ConfigMap not found"
    echo "  This should have been created by 'make deploy'"
    echo "  Please run: kubectl apply -f config/default/target-descriptors-configmap.yaml"
    exit 1
else
    echo "  ✓ Target descriptors ConfigMap exists"
fi
echo ""

# Summary
echo "========================================="
echo "Setup Complete!"
echo "========================================="
echo ""
echo "Your test environment is ready. You can now:"
echo "  1. Apply a test upgrade:"
echo "     kubectl apply -f examples/test-upgrade.yaml"
echo ""
echo "  2. Monitor the upgrade:"
echo "     kubectl get liqoupgrades.upgrade.liqo.io -A -w"
echo ""
echo "  3. Check upgrade status:"
echo "     kubectl describe liqoupgrades.upgrade.liqo.io liqo-upgrade-test -n ${NAMESPACE}"
echo ""
echo "Configuration:"
echo "  Namespace: ${NAMESPACE}"
echo "  Cluster ID: ${CLUSTER_ID}"
echo ""
