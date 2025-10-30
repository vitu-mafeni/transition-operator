package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ControlPlaneStatus string

const (
	ControlPlaneReady       ControlPlaneStatus = "Ready"
	ControlPlaneNotReady    ControlPlaneStatus = "NotReady"
	ControlPlaneUnreachable ControlPlaneStatus = "Unreachable"
	ControlPlaneError       ControlPlaneStatus = "Error"
)

// CheckControlPlaneStatus checks if control plane nodes and critical pods are healthy,
// and whether the API server is reachable.
func CheckControlPlaneStatus(ctx context.Context, c client.Client) (ControlPlaneStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// --- 1. Try to list control-plane nodes
	var nodeList v1.NodeList
	if err := c.List(ctx, &nodeList, client.MatchingLabels{
		"node-role.kubernetes.io/control-plane": "",
	}); err != nil {
		// Detect API server unreachable conditions
		if isNetworkOrTimeoutError(err) {
			return ControlPlaneUnreachable, fmt.Errorf("API server unreachable: %w", err)
		}
		return ControlPlaneError, fmt.Errorf("failed to list control-plane nodes: %w", err)
	}

	if len(nodeList.Items) == 0 {
		return ControlPlaneError, fmt.Errorf("no control-plane nodes found")
	}

	// --- 2. Verify Node readiness
	for _, node := range nodeList.Items {
		for _, cond := range node.Status.Conditions {
			if cond.Type == v1.NodeReady && cond.Status != v1.ConditionTrue {
				return ControlPlaneNotReady, fmt.Errorf("control-plane node %s not ready (%s: %s)",
					node.Name, cond.Reason, cond.Message)
			}
		}
	}

	// --- 3. Optionally check kube-apiserver pods
	var podList v1.PodList
	if err := c.List(ctx, &podList, client.InNamespace("kube-system"),
		client.MatchingLabels{"component": "kube-apiserver"}); err == nil {
		for _, pod := range podList.Items {
			for _, cond := range pod.Status.Conditions {
				if cond.Type == v1.PodReady && cond.Status != v1.ConditionTrue {
					return ControlPlaneNotReady, fmt.Errorf("kube-apiserver pod %s not ready", pod.Name)
				}
			}
		}
	}

	return ControlPlaneReady, nil
}

// isNetworkOrTimeoutError checks if the error indicates the API server is unreachable.
func isNetworkOrTimeoutError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || !netErr.Temporary()) {
		return true
	}

	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "server is currently unable to handle the request") {
		return true
	}

	return false
}
