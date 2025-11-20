#!/bin/bash
# Debug script to check the failing pod error

echo "=== Checking failing pod details ==="
echo ""

POD_NAME=$(kubectl get pods -n liqo -l app.kubernetes.io/name=controller-manager --field-selector status.phase!=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)

if [ -z "$POD_NAME" ]; then
  echo "No failing controller-manager pod found"
  exit 1
fi

echo "Pod Name: $POD_NAME"
echo ""
echo "=== Pod Events ==="
kubectl describe pod -n liqo "$POD_NAME" | grep -A 20 "Events:"
echo ""

echo "=== Container Status ==="
kubectl get pod -n liqo "$POD_NAME" -o jsonpath='{range .status.containerStatuses[*]}{.name}{": "}{.state}{"\n"}{end}'
echo ""

echo "=== Checking if ConfigMap exists ==="
kubectl get configmap -n liqo liqo-cluster-id -o yaml 2>&1
echo ""

echo "=== Current Deployment Environment Variables ==="
kubectl get deployment -n liqo liqo-controller-manager -o jsonpath='{.spec.template.spec.containers[0].env}' | jq '.'
