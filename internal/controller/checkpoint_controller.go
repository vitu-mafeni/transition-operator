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
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/pointer"
	capiv1beta1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/robfig/cron/v3"
	transitionv1 "github.com/vitu1234/transition-operator/api/v1"
	capictrl "github.com/vitu1234/transition-operator/reconcilers/capi"
)

const (
	CheckpointFinalizer = "checkpoint.transition.dcnlab.ssu.ac.kr/finalizer"
	CheckpointBasePath  = "/var/lib/kubelet/checkpoints"
	ServiceAccountPath  = "/var/run/secrets/kubernetes.io/serviceaccount"
)

const (
	KubernetesHost         = "https://kubernetes.default.svc"
	checkpoint_saName      = "checkpoint-sa"
	checkpoint_saNamespace = "default"
)

// CheckpointResponse represents the response from kubelet checkpoint API
// The actual response format contains an "items" array with checkpoint file paths
type CheckpointResponse struct {
	Items []string `json:"items"`
}

// CheckpointReconciler reconciles a Checkpoint object
type CheckpointReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	NodeName       string
	KubeletClient  *KubeletClient
	RegistryClient *RegistryClient
	Scheduler      *cron.Cron
	scheduledJobs  map[string]cron.EntryID // Track scheduled jobs
}

// KubeletClient handles communication with kubelet API
type KubeletClient struct {
	httpClient *http.Client
	token      string
	kubeletURL string
	baseURL    string
	nodeName   string
}

// RegistryClient handles container registry operations
type RegistryClient struct {
	username string
	password string
	registry string
}

// // CheckpointReconciler reconciles a Checkpoint object
// type CheckpointReconciler struct {
// 	client.Client
// 	Scheme *runtime.Scheme
// }

// +kubebuilder:rbac:groups=transition.dcnlab.ssu.ac.kr,resources=checkpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=transition.dcnlab.ssu.ac.kr,resources=checkpoints/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=transition.dcnlab.ssu.ac.kr,resources=checkpoints/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/checkpoints,verbs=patch;create;update;proxy
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Checkpoint object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *CheckpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	log := logf.FromContext(ctx)

	// Fetch the Checkpoint instance
	var Checkpoint transitionv1.Checkpoint
	if err := r.Get(ctx, req.NamespacedName, &Checkpoint); err != nil {
		if errors.IsNotFound(err) {
			log.Info("Checkpoint resource not found. Ignoring since object must be deleted")
			// Clean up any scheduled job
			if r.scheduledJobs != nil {
				if entryID, exists := r.scheduledJobs[req.NamespacedName.String()]; exists {
					r.Scheduler.Remove(entryID)
					delete(r.scheduledJobs, req.NamespacedName.String())
				}
			}
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get Checkpoint")
		return ctrl.Result{}, err
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(&Checkpoint, CheckpointFinalizer) {
		controllerutil.AddFinalizer(&Checkpoint, CheckpointFinalizer)
		if err := r.Update(ctx, &Checkpoint); err != nil {
			log.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
	}

	// Handle deletion
	if Checkpoint.GetDeletionTimestamp() != nil {
		return r.reconcileDelete(ctx, &Checkpoint)
	}

	// Get workload cluster client
	_, workloadClient, restConfig, err := r.getWorkloadClient(ctx, Checkpoint)
	if err != nil {
		log.Error(err, "Failed to get workload cluster client")
		return ctrl.Result{}, err
	}

	// Get all nodes in workload cluster
	var nodeList corev1.NodeList
	if err := workloadClient.List(ctx, &nodeList); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list nodes in workload cluster: %w", err)
	}

	// Get the Pod
	var pod corev1.Pod
	if err := workloadClient.Get(ctx, types.NamespacedName{
		Name:      Checkpoint.Spec.PodRef.Name,
		Namespace: Checkpoint.Spec.PodRef.Namespace,
	}, &pod); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get pod %s/%s: %w", Checkpoint.Spec.PodRef.Namespace, Checkpoint.Spec.PodRef.Name, err)
	}

	nodeKubeletURL := ""
	nodeKubeletIP := ""
	nodeName := pod.Spec.NodeName

	// Find the node that pod is scheduled on
	for _, node := range nodeList.Items {
		if pod.Spec.NodeName == node.Name {
			for _, addr := range node.Status.Addresses {
				if addr.Type == corev1.NodeInternalIP {
					nodeKubeletIP := addr.Address
					nodeKubeletURL = fmt.Sprintf("https://%s:10250", nodeKubeletIP)
					NewKubeletClient(ctx, nodeKubeletIP, workloadClient)
				}
			}
		}
	}

	// Initialize clients if not already done
	if err := r.initializeClients(ctx, &Checkpoint, nodeKubeletIP, workloadClient); err != nil {
		log.Error(err, "Failed to initialize clients")
		return ctrl.Result{}, err
	}

	// // Check if the pod is on this node
	// isOnThisNode, err := r.isPodOnThisNode(ctx, &Checkpoint)
	// if err != nil {
	// 	log.Error(err, "Failed to check if pod is on this node")
	// 	return ctrl.Result{}, err
	// }

	// if !isOnThisNode {
	// 	log.Info("Pod is not on this node, skipping", "pod", Checkpoint.Spec.PodRef.Name, "node", r.NodeName)
	// 	return ctrl.Result{}, nil
	// }

	// Handle normal reconciliation
	return r.reconcileNormal(ctx, workloadClient, &Checkpoint, nodeKubeletURL, nodeName, restConfig)

}

// getWorkloadClient is a helper function to get the workload cluster client
func (r *CheckpointReconciler) getWorkloadClient(
	ctx context.Context,
	checkpoint transitionv1.Checkpoint,
) (ctrl.Result, client.Client, *rest.Config, error) {
	log := logf.FromContext(ctx)
	clusterList := &capiv1beta1.ClusterList{}
	if err := r.Client.List(ctx, clusterList); err != nil {
		return ctrl.Result{}, nil, nil, err
	}

	var capiCluster *capictrl.Capi
	for _, workload_cluster := range clusterList.Items {
		if checkpoint.Spec.ClusterRef.Name == workload_cluster.Name {
			var err error
			capiCluster, err = capictrl.GetCapiClusterFromName(
				ctx, checkpoint.Spec.ClusterRef.Name, "default", r.Client,
			)
			if err != nil {
				log.Error(err, "Failed to get CAPI cluster")
				return ctrl.Result{}, nil, nil, err
			}
			break
		}
	}

	if capiCluster == nil {
		return ctrl.Result{}, nil, nil, fmt.Errorf(
			"no matching cluster found for %s",
			checkpoint.Spec.ClusterRef.Name,
		)
	}

	fmt.Printf("DEBUG: Found CAPI cluster: %s\n", capiCluster.GetClusterName())

	clusterClient, restConfig, _, err := capiCluster.GetClusterClient(ctx)
	if err != nil {
		log.Error(err, "Failed to get workload cluster client", "cluster", capiCluster.GetClusterName())
		return ctrl.Result{}, nil, nil, err
	}

	return ctrl.Result{}, clusterClient, restConfig, nil
}

// get kubelet token
func getKubeletToken(ctx context.Context, workloadClient client.Client, saName, saNamespace string) ([]byte, error) {
	// Try TokenRequest API first
	tr := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{"https://kubernetes.default.svc"},
			ExpirationSeconds: pointer.Int64(3600),
		},
	}

	sa := &corev1.ServiceAccount{}
	if err := workloadClient.Get(ctx, types.NamespacedName{
		Name:      saName,
		Namespace: saNamespace,
	}, sa); err != nil {
		return nil, fmt.Errorf("failed to get service account: %w", err)
	}

	// Use Token subresource if supported
	if err := workloadClient.SubResource("token").Create(ctx, sa, tr); err == nil {
		return []byte(tr.Status.Token), nil
	}

	// Fallback: find a Secret with the right annotation
	secretList := &corev1.SecretList{}
	if err := workloadClient.List(ctx, secretList, client.InNamespace(saNamespace)); err != nil {
		return nil, fmt.Errorf("failed to list secrets: %w", err)
	}

	for _, s := range secretList.Items {
		if s.Type == corev1.SecretTypeServiceAccountToken &&
			s.Annotations["kubernetes.io/service-account.name"] == saName {
			if token, ok := s.Data["token"]; ok {
				return token, nil
			}
		}
	}

	return nil, fmt.Errorf("no token secret found for service account %s", saName)
}

// initializeClients initializes the kubelet and registry clients
func (r *CheckpointReconciler) initializeClients(ctx context.Context, backup *transitionv1.Checkpoint, nodeKubeletIP string, workloadClient client.Client) error {
	if r.KubeletClient == nil {
		kubeletClient, err := NewKubeletClient(ctx, nodeKubeletIP, workloadClient)
		if err != nil {
			return fmt.Errorf("failed to create kubelet client: %w", err)
		}
		r.KubeletClient = kubeletClient
	}

	if r.RegistryClient == nil {
		registryClient, err := r.NewRegistryClient(ctx, workloadClient, backup.Spec.Registry)
		if err != nil {
			return fmt.Errorf("failed to create registry client: %w", err)
		}
		r.RegistryClient = registryClient
	}

	if r.Scheduler == nil {
		r.Scheduler = cron.New()
		r.Scheduler.Start()
		r.scheduledJobs = make(map[string]cron.EntryID)
	}

	return nil
}

// NewKubeletClient creates a new kubelet client
// NewKubeletClient creates a new kubelet client
func NewKubeletClient(ctx context.Context, nodeIP string, workloadClient client.Client) (*KubeletClient, error) {
	// 1. Get token via TokenRequest (or fallback)
	tokenBytes, err := getKubeletToken(ctx, workloadClient, checkpoint_saName, checkpoint_saNamespace)
	if err != nil {
		return nil, fmt.Errorf("failed to get kubelet token: %w", err)
	}

	// 2. Build kubelet URL
	kubeletURL := "https://localhost:10250"
	if nodeIP != "" {
		kubeletURL = fmt.Sprintf("https://%s:10250", nodeIP)
	}

	// 3. Create HTTP client
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 30 * time.Second,
	}

	return &KubeletClient{
		httpClient: httpClient,
		token:      string(tokenBytes),
		kubeletURL: kubeletURL,
	}, nil
}

// NewRegistryClient creates a new registry client using the registry configuration from Checkpoint
func (r *CheckpointReconciler) NewRegistryClient(
	ctx context.Context,
	workloadClient client.Client,
	registryConfig transitionv1.Registry,
) (*RegistryClient, error) {
	// Determine secret name and namespace
	secretName := "reg-credentials" // default fallback
	secretNamespace := "default"    // default fallback

	if registryConfig.SecretRef != nil {
		secretName = registryConfig.SecretRef.Name
		if registryConfig.SecretRef.Namespace != "" {
			secretNamespace = registryConfig.SecretRef.Namespace
		}
	}

	// Get registry credentials from workload cluster secret
	var secret corev1.Secret
	if err := workloadClient.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: secretNamespace,
	}, &secret); err != nil {
		return nil, fmt.Errorf("failed to get registry credentials secret %s/%s: %w", secretNamespace, secretName, err)
	}

	username := string(secret.Data["username"])
	password := string(secret.Data["password"])
	registry := string(secret.Data["registry"])

	if username == "" || password == "" {
		return nil, fmt.Errorf("registry credentials are empty in secret %s/%s", secretNamespace, secretName)
	}

	// Use registry URL from config → secret → fallback
	registryURL := registryConfig.URL
	if registryURL == "" && registry != "" {
		registryURL = registry
	}
	if registryURL == "" {
		registryURL = "docker.io"
	}

	return &RegistryClient{
		username: username,
		password: password,
		registry: registryURL,
	}, nil
}

// isPodOnThisNode checks if the pod referenced in Checkpoint is on this node
func (r *CheckpointReconciler) isPodOnThisNode(ctx context.Context, backup *transitionv1.Checkpoint) (bool, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{
		Name:      backup.Spec.PodRef.Name,
		Namespace: backup.Spec.PodRef.Namespace,
	}, &pod); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	return pod.Spec.NodeName == r.NodeName, nil
}

// reconcileNormal handles the normal reconciliation logic
func (r *CheckpointReconciler) reconcileNormal(ctx context.Context, workloadClient client.Client, backup *transitionv1.Checkpoint, kubeletURL, nodeName string, restConfig *rest.Config) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Schedule checkpoint creation based on the schedule
	backupKey := types.NamespacedName{
		Name:      backup.Name,
		Namespace: backup.Namespace,
	}.String()

	// Remove existing job if schedule changed
	if entryID, exists := r.scheduledJobs[backupKey]; exists {
		r.Scheduler.Remove(entryID)
		delete(r.scheduledJobs, backupKey)
	}

	// Add new scheduled job
	entryID, err := r.Scheduler.AddFunc(backup.Spec.Schedule, func() {
		if err := r.performCheckpoint(context.Background(), workloadClient, backup, kubeletURL, nodeName, restConfig); err != nil {
			log.Error(err, "Failed to perform checkpoint", "backup", backup.Name)
		}
	})
	if err != nil {
		log.Error(err, "Failed to schedule checkpoint job")
		return ctrl.Result{}, err
	}

	r.scheduledJobs[backupKey] = entryID
	log.Info("Scheduled checkpoint job", "backup", backup.Name, "schedule", backup.Spec.Schedule)

	// Also perform immediate checkpoint on first reconcile
	if backup.Status.LastCheckpointTime == nil {
		if err := r.performCheckpoint(ctx, workloadClient, backup, kubeletURL, nodeName, restConfig); err != nil {
			log.Error(err, "Failed to perform initial checkpoint")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: time.Hour}, nil
}

// reconcileDelete handles the deletion logic
func (r *CheckpointReconciler) reconcileDelete(ctx context.Context, backup *transitionv1.Checkpoint) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Remove scheduled job
	backupKey := types.NamespacedName{
		Name:      backup.Name,
		Namespace: backup.Namespace,
	}.String()

	if entryID, exists := r.scheduledJobs[backupKey]; exists {
		r.Scheduler.Remove(entryID)
		delete(r.scheduledJobs, backupKey)
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(backup, CheckpointFinalizer)
	if err := r.Update(ctx, backup); err != nil {
		log.Error(err, "Failed to remove finalizer")
		return ctrl.Result{}, err
	}

	log.Info("Successfully deleted Checkpoint", "name", backup.Name)
	return ctrl.Result{}, nil
}

// performCheckpoint performs the actual checkpoint operation
func (r *CheckpointReconciler) performCheckpoint(ctx context.Context, workloadClient client.Client, backup *transitionv1.Checkpoint, kubeletURL, nodeName string, restConfig *rest.Config) error {
	log := logf.FromContext(ctx)
	log.Info("Starting checkpoint operation", "backup", backup.Name, "pod", backup.Spec.PodRef.Name)

	// Get pod to ensure it exists and is ready
	var pod corev1.Pod
	if err := workloadClient.Get(ctx, types.NamespacedName{
		Name:      backup.Spec.PodRef.Name,
		Namespace: backup.Spec.PodRef.Namespace,
	}, &pod); err != nil {
		return fmt.Errorf("failed to get pod: %w", err)
	}

	if pod.Status.Phase != corev1.PodRunning {
		log.Info("Pod is not running, skipping checkpoint", "pod", pod.Name, "phase", pod.Status.Phase)
		return nil
	}

	// Process each container
	for _, container := range pod.Spec.Containers {
		if err := r.checkpointContainer(ctx, backup, &pod, container, kubeletURL, nodeName, restConfig); err != nil {
			log.Error(err, "Failed to checkpoint container", "container", container.Name)
			return err
		}
	}

	// Update status
	now := metav1.Now()
	backup.Status.LastCheckpointTime = &now
	backup.Status.Phase = "Completed"
	if err := r.Status().Update(ctx, backup); err != nil {
		log.Error(err, "Failed to update backup status")
		return err
	}

	log.Info("Successfully completed checkpoint operation", "backup", backup.Name)
	return nil
}

// checkpointContainer performs checkpoint operation for a single container
func (r *CheckpointReconciler) checkpointContainer(ctx context.Context, backup *transitionv1.Checkpoint, pod *corev1.Pod, container corev1.Container, kubeletURL, nodeName string, restConfig *rest.Config) error {
	log := logf.FromContext(ctx)
	log.Info("Checkpointing container", "container", container.Name, "pod", pod.Name)

	// Step 1: Call kubelet checkpoint API
	checkpointPath, err := r.KubeletClient.CreateCheckpoint(ctx, restConfig, nodeName, backup.Spec.PodRef.Namespace, backup.Spec.PodRef.Name, container.Name, kubeletURL)
	if err != nil {
		return fmt.Errorf("failed to create checkpoint via kubelet API: %w", err)
	}

	// Step 1.5: Verify the checkpoint file exists (kubelet API should have returned the exact path)
	fullCheckpointPath := filepath.Join(CheckpointBasePath, checkpointPath)
	if _, err := os.Stat(fullCheckpointPath); os.IsNotExist(err) {
		// If the file doesn't exist, fall back to file search
		log.Info("Checkpoint file from API response not found, searching for alternative", "expectedPath", checkpointPath)
		actualCheckpointPath, err := r.findCheckpointFile(backup.Spec.PodRef.Namespace, backup.Spec.PodRef.Name, container.Name, checkpointPath)
		if err != nil {
			return fmt.Errorf("failed to find checkpoint file after creation: %w", err)
		}
		log.Info("Found alternative checkpoint file", "actualPath", actualCheckpointPath, "originalExpected", checkpointPath)
		checkpointPath = actualCheckpointPath
	} else {
		log.Info("Checkpoint file found as expected", "path", checkpointPath)
	}

	// Step 2: Get the original container image
	var baseImage string
	for _, c := range pod.Spec.Containers {
		if c.Name == container.Name {
			baseImage = c.Image
			break
		}
	}
	if baseImage == "" {
		return fmt.Errorf("could not find base image for container %s", container.Name)
	}

	// Step 3: Build checkpoint image using buildah
	if err := r.buildCheckpointImage(checkpointPath, container.Image, baseImage, container.Name); err != nil {
		return fmt.Errorf("failed to build checkpoint image: %w", err)
	}

	// Step 4: Push image to registry
	if err := r.RegistryClient.PushImage(container.Image); err != nil {
		return fmt.Errorf("failed to push checkpoint image: %w", err)
	}

	log.Info("Successfully checkpointed and pushed container image", "container", container.Name, "image", container.Image)
	return nil
}

// CreateCheckpoint calls kubelet checkpoint API
func (kc *KubeletClient) CreateCheckpoint(ctx context.Context, restConfig *rest.Config, nodeName, namespace, podName, containerName, kubeletURL string) (string, error) {
	url := fmt.Sprintf(
		"%s/api/v1/nodes/%s/proxy/checkpoint/%s/%s/%s?timeout=60",
		restConfig.Host,
		nodeName,
		namespace,
		podName,
		containerName,
	)

	// Build transport that reuses kubeconfig auth (token / certs)
	transport, err := rest.TransportFor(restConfig)
	if err != nil {
		return "", fmt.Errorf("failed to create transport: %w", err)
	}

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   60 * time.Second,
	}

	// Send POST request
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to call kubelet checkpoint API via apiserver proxy: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("checkpoint API returned %d: %s", resp.StatusCode, string(body))
	}

	// Parse response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read checkpoint response body: %w", err)
	}

	fmt.Printf("DEBUG: Kubelet checkpoint API response body: %s\n", string(body))

	var checkpointResp CheckpointResponse
	if err := json.Unmarshal(body, &checkpointResp); err != nil {
		return "", fmt.Errorf("failed to parse checkpoint response: %w (raw=%s)", err, string(body))
	}

	if len(checkpointResp.Items) == 0 {
		return "", fmt.Errorf("checkpoint API returned no items")
	}

	return checkpointResp.Items[0], nil

	// req, err := http.NewRequest("POST", url, nil)
	// if err != nil {
	// 	return "", fmt.Errorf("failed to create request: %w", err)
	// }

	// req.Header.Set("Authorization", "Bearer "+kc.token)
	// req.Header.Set("Content-Type", "application/json")

	// resp, err := kc.httpClient.Do(req)
	// if err != nil {
	// 	return "", fmt.Errorf("failed to call kubelet checkpoint API: %w", err)
	// }
	// defer resp.Body.Close()

	// if resp.StatusCode != http.StatusOK {
	// 	body, _ := io.ReadAll(resp.Body)
	// 	return "", fmt.Errorf("kubelet checkpoint API returned status %d: %s", resp.StatusCode, string(body))
	// }

	// // First, try to read the response body to see what we actually get
	// body, err := io.ReadAll(resp.Body)
	// if err != nil {
	// 	return "", fmt.Errorf("failed to read checkpoint response body: %w", err)
	// }

	// // Log the raw response for debugging
	// fmt.Printf("DEBUG: Kubelet checkpoint API response body: %s\n", string(body))

	// var checkpointResp CheckpointResponse
	// if err := json.Unmarshal(body, &checkpointResp); err != nil {
	// 	// If JSON parsing fails, fall back to file search by returning a placeholder
	// 	responseText := strings.TrimSpace(string(body))
	// 	fmt.Printf("DEBUG: Failed to parse JSON response from kubelet: %s\n", responseText)
	// 	return "unknown-checkpoint-file", nil
	// }

	// if len(checkpointResp.Items) == 0 {
	// 	// If JSON response doesn't contain any items, fall back to file search
	// 	fmt.Printf("DEBUG: JSON response has no checkpoint items, falling back to file search\n")
	// 	return "unknown-checkpoint-file", nil
	// }

	// // Use the first (and likely only) checkpoint path from the response
	// checkpointPath := checkpointResp.Items[0]
	// fmt.Printf("DEBUG: Successfully parsed JSON response, checkpoint path: %s\n", checkpointPath)

	// // Convert absolute path to relative path (remove the base path prefix)
	// if strings.HasPrefix(checkpointPath, CheckpointBasePath+"/") {
	// 	relativePath := strings.TrimPrefix(checkpointPath, CheckpointBasePath+"/")
	// 	fmt.Printf("DEBUG: Converted to relative path: %s\n", relativePath)
	// 	return relativePath, nil
	// } else if strings.HasPrefix(checkpointPath, "/var/lib/kubelet/checkpoints/") {
	// 	relativePath := strings.TrimPrefix(checkpointPath, "/var/lib/kubelet/checkpoints/")
	// 	fmt.Printf("DEBUG: Converted to relative path: %s\n", relativePath)
	// 	return relativePath, nil
	// }

	// // If path doesn't have expected prefix, just return the filename
	// relativePath := filepath.Base(checkpointPath)
	// fmt.Printf("DEBUG: Using filename only: %s\n", relativePath)
	// return relativePath, nil
}

// findCheckpointFile finds the most recent checkpoint file for a given pod and container
func (r *CheckpointReconciler) findCheckpointFile(namespace, podName, containerName, expectedPath string) (string, error) {
	// First try the expected path
	fullExpectedPath := filepath.Join(CheckpointBasePath, expectedPath)
	if _, err := os.Stat(fullExpectedPath); err == nil {
		return expectedPath, nil
	}

	// If expected path doesn't exist, search for checkpoint files matching the pattern
	podFullName := fmt.Sprintf("%s_%s", namespace, podName)
	pattern := fmt.Sprintf("checkpoint-%s-%s-*.tar", podFullName, containerName)
	fullPattern := filepath.Join(CheckpointBasePath, pattern)

	fmt.Printf("DEBUG: Searching for checkpoint files with pattern: %s\n", fullPattern)
	matches, err := filepath.Glob(fullPattern)
	if err != nil {
		return "", fmt.Errorf("failed to search for checkpoint files with pattern %s: %w", pattern, err)
	}
	fmt.Printf("DEBUG: Found %d matching files: %v\n", len(matches), matches)

	if len(matches) == 0 {
		// List all files in checkpoint directory for debugging
		files, _ := os.ReadDir(CheckpointBasePath)
		var fileNames []string
		for _, file := range files {
			fileNames = append(fileNames, file.Name())
		}
		return "", fmt.Errorf("no checkpoint files found for pattern %s. Available files: %v", pattern, fileNames)
	}

	// Sort matches to get the most recent file (files are naturally sorted by timestamp)
	// Find the most recent file (look for files created in the last few seconds)
	var mostRecentFile string
	var mostRecentTime time.Time

	now := time.Now()
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil {
			continue
		}

		// Only consider files created in the last 30 seconds (recent checkpoint)
		if now.Sub(info.ModTime()) < 30*time.Second {
			if info.ModTime().After(mostRecentTime) {
				mostRecentTime = info.ModTime()
				mostRecentFile = match
			}
		}
	}

	if mostRecentFile == "" {
		// If no recent file found, use the lexicographically last one (likely most recent by timestamp)
		mostRecentFile = matches[len(matches)-1]
	}

	relativePath, _ := filepath.Rel(CheckpointBasePath, mostRecentFile)
	return relativePath, nil
}

// buildCheckpointImage builds the checkpoint image using buildah
func (r *CheckpointReconciler) buildCheckpointImage(checkpointPath, imageName, baseImage, containerName string) error {
	log := logf.FromContext(context.Background())

	// Verify the checkpoint file exists (should have been found by findCheckpointFile)
	fullCheckpointPath := filepath.Join(CheckpointBasePath, checkpointPath)
	if _, err := os.Stat(fullCheckpointPath); os.IsNotExist(err) {
		return fmt.Errorf("checkpoint file does not exist: %s (this should not happen after findCheckpointFile)", fullCheckpointPath)
	}

	log.Info("Building checkpoint image", "checkpointFile", fullCheckpointPath, "imageName", imageName, "baseImage", baseImage)

	// Step 1: Create new container from scratch
	cmd := exec.Command("buildah", "from", "scratch")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to create buildah container: %w", err)
	}
	newContainer := strings.TrimSpace(string(out))

	// Ensure cleanup
	defer func() {
		exec.Command("buildah", "rm", newContainer).Run()
	}()

	// Step 2: Add checkpoint tar to root
	if err := exec.Command("buildah", "add", newContainer, fullCheckpointPath, "/").Run(); err != nil {
		return fmt.Errorf("failed to add checkpoint to container (%s): %w", fullCheckpointPath, err)
	}

	// Step 3: Add CRI-O checkpoint annotations
	if err := exec.Command("buildah", "config",
		"--annotation=io.kubernetes.cri-o.annotations.checkpoint.name="+imageName,
		newContainer).Run(); err != nil {
		return fmt.Errorf("failed to add checkpoint name annotation: %w", err)
	}

	if err := exec.Command("buildah", "config",
		"--annotation=io.kubernetes.cri-o.annotations.checkpoint.rootfsImageName="+baseImage,
		newContainer).Run(); err != nil {
		return fmt.Errorf("failed to add rootfs image annotation: %w", err)
	}

	// Step 4: Commit and tag image
	if err := exec.Command("buildah", "commit", newContainer, imageName).Run(); err != nil {
		return fmt.Errorf("failed to commit image: %w", err)
	}

	log.Info("Successfully built checkpoint image", "image", imageName, "baseImage", baseImage)
	return nil
}

// PushImage pushes the image to the registry
func (rc *RegistryClient) PushImage(imageName string) error {
	// Login to registry
	if err := rc.login(imageName); err != nil {
		return fmt.Errorf("failed to login to registry: %w", err)
	}

	// Push image
	cmd := exec.Command("buildah", "push", imageName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to push image %s: %w", imageName, err)
	}

	return nil
}

// login performs registry authentication
func (rc *RegistryClient) login(imageName string) error {
	// Use the registry URL from the secret (not extracted from image name)
	// For Docker Hub, this should be "docker.io" or can be empty

	// Login using buildah
	cmd := exec.Command("buildah", "login", "-u", rc.username, "-p", rc.password, rc.registry)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to login to registry %s: %w", rc.registry, err)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *CheckpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&transitionv1.Checkpoint{}).
		Named("checkpoint").
		Complete(r)
}
