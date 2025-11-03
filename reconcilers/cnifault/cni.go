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
	// 10-second overall timeout
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
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

	gracePeriod := 1 * time.Minute
	now := time.Now()

	for _, pod := range cniPods {
		ready := false
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Ready {
				ready = true
				break
			}
			// ignore transient recent crashes
			if cs.LastTerminationState.Terminated != nil &&
				cs.LastTerminationState.Terminated.ExitCode != 0 &&
				now.Sub(cs.State.Running.StartedAt.Time) < gracePeriod {
				ready = true
			}
		}
		if !ready {
			return CNINotReady, fmt.Errorf("CNI pod %s/%s not ready yet", pod.Namespace, pod.Name)
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
	targets := []string{"kubernetes.default.svc", "8.8.8.8"}
	for _, pod := range samplePods {
		if err := probePodNetwork(ctx, workloadClusterRestConfig, &pod, targets); err != nil {
			return CNINotReady, fmt.Errorf("network probe failed for pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}

	return CNIReady, nil
}

// listCNIPods returns CNI pods by common namespaces/labels
func listCNIPods(ctx context.Context, c client.Client) ([]v1.Pod, error) {
	var pods []v1.Pod
	cniNamespaces := []string{"kube-system", "kube-flannel", "cilium-system", "calico-system"}
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
	fmt.Printf("DEBUG: Found %d CNI pods\n", len(pods))
	fmt.Printf("DEBUG: CNI pods: ")
	for _, pod := range pods {
		fmt.Printf("%s/%s ", pod.Namespace, pod.Name)
	}
	fmt.Println()
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
	fmt.Printf("DEBUG: Found %d application pods\n", len(pods))
	for _, pod := range pods {
		fmt.Printf("DEBUG: Application pod: %s/%s\n", pod.Namespace, pod.Name)
	}
	return pods, nil
}

// probePodNetwork execs a lightweight ping inside the pod
func probePodNetwork(ctx context.Context, restConfig *rest.Config, pod *v1.Pod, targets []string) error {
	for _, target := range targets {
		cmd := []string{"ping", "-c", "1", "-W", "1", target}

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

		fmt.Printf("DEBUG: probing %s/%s\n", pod.Namespace, pod.Name)

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

		if strings.Contains(stdout.String(), "1 received") {
			// success
			return nil
		}
	}

	fmt.Printf("DEBUG: Pod %s/%s network probe failed to all targets\n", pod.Namespace, pod.Name)

	return fmt.Errorf("all network probes failed")
}

// isNetworkOrTimeoutError detects API reachability issues
func isNetworkOrTimeoutError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "server is currently unable to handle the request")
}
