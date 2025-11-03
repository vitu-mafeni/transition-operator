package cnifault

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CNIStatus represents the health of CNI in a workload cluster
type CNIStatus string

const (
	CNIReady       CNIStatus = "Ready"
	CNINotReady    CNIStatus = "NotReady"
	CNIUnreachable CNIStatus = "Unreachable"
	CNIError       CNIStatus = "Error"
)

// CheckCNIStatus probes CNI pods and cluster networking using a workload cluster client.
func CheckCNIStatus(ctx context.Context, c client.Client, workloadClusterRestConfig *rest.Config) (CNIStatus, error) {
	// 3-second overall timeout
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	// --- Check CNI pods readiness
	cniPods, err := listCNIPods(ctx, c)
	if err != nil {
		if isNetworkOrTimeoutError(err) {
			return CNIUnreachable, fmt.Errorf("workload cluster API unreachable: %w", err)
		}
		return CNIError, fmt.Errorf("failed to list CNI pods: %w", err)
	}

	if len(cniPods) == 0 {
		return CNINotReady, errors.New("no CNI pods found in workload cluster")
	}

	for _, pod := range cniPods {
		for _, cs := range pod.Status.ContainerStatuses {
			if !cs.Ready {
				return CNINotReady, fmt.Errorf("CNI pod %s/%s container %s not ready", pod.Namespace, pod.Name, cs.Name)
			}
		}
	}

	// --- Pick a few sample application pods to check networking
	samplePods, err := sampleAppPods(ctx, c, 2)
	if err != nil {
		return CNIError, fmt.Errorf("failed to list sample pods: %w", err)
	}

	if len(samplePods) == 0 {
		// fallback: no pods to probe
		return CNIReady, nil
	}

	// --- Run lightweight network probes
	for _, pod := range samplePods {
		if err := probePodNetwork(ctx, c, workloadClusterRestConfig, &pod); err != nil {
			return CNINotReady, fmt.Errorf("network probe failed for pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}

	return CNIReady, nil
}

// listCNIPods returns CNI pods by common namespaces/labels
func listCNIPods(ctx context.Context, c client.Client) ([]v1.Pod, error) {
	var pods []v1.Pod
	cniNamespaces := []string{"kube-system", "cilium-system", "calico-system"}
	cniLabels := []map[string]string{
		{"app": "calico-node"},
		{"k8s-app": "cilium"},
		{"app": "flannel"},
		{"app": "weave"},
	}

	for _, ns := range cniNamespaces {
		for _, label := range cniLabels {
			var podList v1.PodList
			if err := c.List(ctx, &podList, client.InNamespace(ns), client.MatchingLabels(label)); err != nil {
				continue
			}
			pods = append(pods, podList.Items...)
		}
	}
	return pods, nil
}

// sampleAppPods returns a small number of running pods (not host-network) for probing
func sampleAppPods(ctx context.Context, c client.Client, limit int) ([]v1.Pod, error) {
	var podList v1.PodList
	if err := c.List(ctx, &podList, client.Limit(int64(limit))); err != nil {
		return nil, err
	}

	var pods []v1.Pod
	for _, p := range podList.Items {
		if p.Status.Phase == v1.PodRunning && !p.Spec.HostNetwork && p.Status.PodIP != "" {
			pods = append(pods, p)
			if len(pods) >= limit {
				break
			}
		}
	}
	return pods, nil
}

// probePodNetwork execs a lightweight ping inside the pod
func probePodNetwork(ctx context.Context, c client.Client, restConfig *rest.Config, pod *v1.Pod) error {
	cmd := []string{"ping", "-c", "1", "-W", "1", "kubernetes.default.svc"}

	// Create REST client for exec
	restClient, err := rest.RESTClientFor(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create REST client: %w", err)
	}

	req := restClient.Post().Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec").
		VersionedParams(&v1.PodExecOptions{
			Command:   cmd,
			Container: pod.Spec.Containers[0].Name,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("failed to create SPDY executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	streamOpts := remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}
	if err := exec.StreamWithContext(ctx, streamOpts); err != nil {
		return fmt.Errorf("exec failed: %w, stderr: %s", err, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "1 received") {
		return fmt.Errorf("ping failed, output: %s", output)
	}

	return nil
}

// isNetworkOrTimeoutError detects API reachability issues
func isNetworkOrTimeoutError(err error) bool {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "server is currently unable to handle the request") {
		return true
	}
	return false
}
