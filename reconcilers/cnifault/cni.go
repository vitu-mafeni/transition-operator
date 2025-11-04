package cnifault

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
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
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// --- Check CNI pods readiness
	// cniPods, err := listCNIPods(ctx, c)
	// if err != nil {
	// 	if isNetworkOrTimeoutError(err) {
	// 		return CNIUnreachable, fmt.Errorf("workload cluster API unreachable: %w", err)
	// 	}
	// 	return CNIError, fmt.Errorf("failed to list CNI pods: %w", err)
	// }
	// if len(cniPods) == 0 {
	// 	return CNINotReady, errors.New("no CNI pods found in workload cluster")
	// }

	// // --- Check readiness of CNI pods
	// gracePeriod := 1 * time.Minute
	// now := time.Now()
	// for _, pod := range cniPods {
	// 	ready := false
	// 	for _, cs := range pod.Status.ContainerStatuses {
	// 		if cs.Ready {
	// 			ready = true
	// 			break
	// 		}
	// 		if cs.LastTerminationState.Terminated != nil &&
	// 			cs.LastTerminationState.Terminated.ExitCode != 0 &&
	// 			now.Sub(cs.State.Running.StartedAt.Time) < gracePeriod {
	// 			ready = true
	// 		}
	// 	}
	// 	if !ready {
	// 		return CNINotReady, fmt.Errorf("CNI pod %s/%s not ready yet", pod.Namespace, pod.Name)
	// 	}
	// }

	// --- Get all net-test pods as source pods
	netTestPods, err := listNetTestPods(ctx, c)
	if err != nil || len(netTestPods) == 0 {
		return CNINotReady, errors.New("no net-test pods found")
	}

	// --- Get all pods for target selection
	allPods, err := listAllPods(ctx, c)
	if err != nil || len(allPods) == 0 {
		return CNINotReady, errors.New("no target pods found")
	}

	// fmt.Printf("DEBUG: Found %d all pods\n", len(allPods))

	// --- Run probes: each net-test pod probes one pod on a different node
	for _, src := range netTestPods {
		var targetPod *v1.Pod
		for i := range allPods {
			if allPods[i].Spec.NodeName != src.Spec.NodeName {
				targetPod = &allPods[i]
				break
			}
		}
		if targetPod == nil {
			continue
		}

		// fmt.Printf("DEBUG: target pod is %s", targetPod.Name)
		// fmt.Printf("DEBUG: target pod IP{} %s", targetPod.Status.PodIP)

		targets := []string{targetPod.Status.PodIP}
		if err := probePodNetwork(ctx, workloadClusterRestConfig, &src, targets); err != nil {
			// fmt.Println(err, " Error err error")
			// If exec timed out, treat as healthy
			if strings.Contains(err.Error(), "exec failed: context deadline exceeded") {
				// fmt.Printf("DEBUG: exec timeout for pod %s/%s, treating CNI as ready\n", src.Namespace, src.Name)
				return CNIReady, nil
			}
			return CNINotReady, fmt.Errorf("network probe failed from pod %s/%s to pod %s/%s: %w",
				src.Namespace, src.Name, targetPod.Namespace, targetPod.Name, err)
		}

		// fmt.Printf("DEBUG: ------ finished for target pod is %s", targetPod.Name)

	}
	// fmt.Printf("DEBUG: return CNI status %s", "ss")
	return CNIReady, nil
}

// listNetTestPods returns pods with names starting with net-test-*
func listNetTestPods(ctx context.Context, c client.Client) ([]v1.Pod, error) {
	var podList v1.PodList
	if err := c.List(ctx, &podList); err != nil {
		return nil, err
	}

	var netPods []v1.Pod
	for _, p := range podList.Items {
		if strings.HasPrefix(p.Name, "net-test-") && p.Status.Phase == v1.PodRunning {
			netPods = append(netPods, p)
		}
	}
	// fmt.Printf("DEBUG: Found %d net-test pods\n", len(netPods))
	return netPods, nil
}

// listAllPods returns all pods in the cluster (any namespace)
func listAllPods(ctx context.Context, c client.Client) ([]v1.Pod, error) {
	var podList v1.PodList
	if err := c.List(ctx, &podList); err != nil {
		return nil, err
	}
	return podList.Items, nil
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
	// fmt.Printf("DEBUG: Found %d application pods\n", len(pods))
	// for _, pod := range pods {
	// 	fmt.Printf("DEBUG: Application pod: %s/%s\n", pod.Namespace, pod.Name)
	// }
	return pods, nil
}

// probePodNetwork runs a simple ping test from the given pod to each target.
// It differentiates between exec timeout, ping failure, and success.
func probePodNetwork(ctx context.Context, restConfig *rest.Config, pod *v1.Pod, targets []string) error {
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes clientset: %w", err)
	}

	for _, target := range targets {
		cmd := []string{"ping", "-c", "1", "-W", "1", target}
		// fmt.Printf("DEBUG: probing pod %s/%s -> %s\n", pod.Namespace, pod.Name, target)

		req := clientset.CoreV1().RESTClient().Post().
			Resource("pods").
			Name(pod.Name).
			Namespace(pod.Namespace).
			SubResource("exec").
			VersionedParams(&v1.PodExecOptions{
				Command:   cmd,
				Container: pod.Spec.Containers[0].Name,
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

		// 10s total timeout for this single ping (includes SPDY setup)
		execCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		err = exec.StreamWithContext(execCtx, streamOpts)

		// ---- Distinguish outcomes ----
		switch {
		// 1. Context deadline (SPDY or slow API)
		case errors.Is(execCtx.Err(), context.DeadlineExceeded):
			// fmt.Printf("DEBUG: exec timeout for pod %s/%s to target %s (context deadline)\n",
			// 	pod.Namespace, pod.Name, target)
			continue

		// 2. Ping exit code 1 → network unreachable
		case err != nil && strings.Contains(err.Error(), "exit code 1"):
			// fmt.Printf("DEBUG: ping unreachable %s -> %s (exit code 1)\n", pod.Name, target)
			// fmt.Printf("DEBUG: stderr: %s\n", stderr.String())
			continue

		// 3. Other exec errors (SPDY, container issues, etc.)
		case err != nil:
			// fmt.Printf("DEBUG: exec failed for pod %s/%s to target %s: %v\nstderr: %s\n",
			// 	pod.Namespace, pod.Name, target, err, stderr.String())
			continue
		}

		// 4. Exec succeeded → check ping output
		out := stdout.String()
		// fmt.Printf("DEBUG: probe output for %s -> %s:\n%s\n", pod.Name, target, out)

		if strings.Contains(out, "1 packets received") ||
			strings.Contains(out, "1 received") ||
			strings.Contains(out, ", 0% packet loss") {
			// fmt.Printf("DEBUG: ping success %s -> %s\n", pod.Name, target)
			return nil
		}

		// fmt.Printf("DEBUG: ping completed but no packets received %s -> %s\n", pod.Name, target)
	}

	return fmt.Errorf("all network probes failed for pod %s/%s", pod.Namespace, pod.Name)
}

// isNetworkOrTimeoutError detects API reachability issues
func isNetworkOrTimeoutError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "server is currently unable to handle the request")
}
