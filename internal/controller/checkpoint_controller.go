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
	"crypto/sha1"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/minio/minio-go/v7"
	"github.com/robfig/cron/v3"
	transitionv1 "github.com/vitu1234/transition-operator/api/v1"
	"github.com/vitu1234/transition-operator/reconcilers/capi"
	capictrl "github.com/vitu1234/transition-operator/reconcilers/capi"
	"github.com/vitu1234/transition-operator/reconcilers/miniohelper"
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
	MinioClient    *MinioClient
}

// KubeletClient handles communication with kubelet API
type KubeletClient struct {
	httpClient *http.Client
	token      string
	kubeletURL string
}

// RegistryClient handles container registry operations
type RegistryClient struct {
	username string
	password string
	registry string
}

type MinioClient struct {
	minioClient *minio.Client
	bucketName  string
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
	clusterResult, workloadClusterClient, restConfig, err := capi.GetWorkloadClusterClient(ctx, r.Client, Checkpoint.Spec.ClusterRef.Name)
	if err != nil {
		log.Error(err, "Failed to get workload cluster client", "checkpoint", Checkpoint.Name)
		return clusterResult, err
	}
	if workloadClusterClient == nil {
		// Cluster not ready or being deleted; just requeue
		log.Info("Cluster client not available yet", "checkpoint", Checkpoint.Name)
		return clusterResult, nil
	}

	// Get all nodes in workload cluster
	var nodeList corev1.NodeList
	if err := workloadClusterClient.List(ctx, &nodeList); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list nodes in workload cluster: %w", err)
	}

	// Get the Pod
	var pod corev1.Pod
	if err := workloadClusterClient.Get(ctx, types.NamespacedName{
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
					NewKubeletClient(ctx, nodeKubeletIP, workloadClusterClient)
				}
			}
		}
	}

	// fmt.Print("node ip address success ssssss: ", nodeKubeletIP)

	// Initialize clients if not already done
	if err := r.initializeClients(ctx, &Checkpoint, nodeKubeletIP, workloadClusterClient); err != nil {
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
	return r.reconcileNormal(ctx, workloadClusterClient, &Checkpoint, nodeKubeletURL, nodeName, restConfig)

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

	//initialize minio client
	minioClient, bucketName, err := NewMinioClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create minio client: %w", err)
	}
	r.MinioClient = &MinioClient{minioClient: minioClient, bucketName: bucketName}

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

func NewMinioClient(ctx context.Context) (client *minio.Client, bucket string, err error) {
	minioClient, bucketName, err := miniohelper.InitializeMinio(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("failed to initialize MinIO client: %w", err)
	}
	return minioClient, bucketName, nil
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
			backup.Status.OriginalImage = container.Image
			backup.Status.LastCheckpointTime = &metav1.Time{Time: time.Now().UTC()}
			backup.Status.Phase = transitionv1.CheckpointPhaseFailed
			backup.Status.Message = fmt.Sprintf("Failed to update backup status with image name: %v", err)

			if err := r.Status().Update(ctx, backup); err != nil {
				log.Error(err, "Failed to update backup status with image name")
				return err
			}
			return err
		}
	}

	// Update status
	// backup.Status.OriginalImage = ""
	// backup.Status.LastCheckpointTime = &metav1.Time{Time: time.Now().UTC()}
	// backup.Status.Phase = transitionv1.CheckpointPhaseCompleted
	// backup.Status.LastCheckpointImage = ""
	// backup.Status.Message = "Checkpoint completed successfully"
	// if err := r.Status().Update(ctx, backup); err != nil {
	// 	log.Error(err, "Failed to update backup status")
	// 	return err
	// }

	log.Info("Successfully completed checkpoint operation", "backup", backup.Name)
	return nil
}

// checkpointContainer performs checkpoint operation for a single container
func (r *CheckpointReconciler) checkpointContainer(ctx context.Context, backup *transitionv1.Checkpoint, pod *corev1.Pod, container corev1.Container, kubeletURL, nodeName string, restConfig *rest.Config) error {
	log := logf.FromContext(ctx)
	log.Info("Checkpointing container", "container", container.Name, "pod", pod.Name)

	backup.Status.OriginalImage = container.Image
	backup.Status.LastCheckpointTime = &metav1.Time{Time: time.Now().UTC()}
	backup.Status.Phase = transitionv1.CheckpointPhaseRunning
	backup.Status.Message = fmt.Sprintf("Starting checkpoint for container %s", container.Name)

	if err := r.Status().Update(ctx, backup); err != nil {
		log.Error(err, "Failed to update backup status with image name")
		return err
	}

	// Step 1: Call kubelet checkpoint API
	checkpointPath, err := r.KubeletClient.CreateCheckpoint(ctx, restConfig, nodeName, backup.Spec.PodRef.Namespace, backup.Spec.PodRef.Name, container.Name, kubeletURL)
	if err != nil {
		backup.Status.OriginalImage = container.Image
		backup.Status.LastCheckpointTime = &metav1.Time{Time: time.Now().UTC()}
		backup.Status.Phase = transitionv1.CheckpointPhaseFailed
		backup.Status.Message = fmt.Sprintf("Failed to update backup status with image name: %v", err)

		if err := r.Status().Update(ctx, backup); err != nil {
			log.Error(err, "Failed to update backup status with image name")
			return err
		}
		return fmt.Errorf("failed to create checkpoint via kubelet API: %w", err)
	}

	// log.Info("Checkpoint created", "checkpointPath", checkpointPath)

	log.Info("Successfully checkpointed and pushed container image", "container", container.Name, "image", container.Image)

	//delay for a few seconds to ensure the file is available
	objectName := filepath.Base(checkpointPath)

	//check if file exists in minio bucket
	// fileExists, err := miniohelper.FileExistsInMinio(ctx, r.MinioClient.minioClient, r.MinioClient.bucketName, checkpointPath)
	// Wait until file is in MinIO before proceeding
	if err := miniohelper.WaitForFileInMinio(ctx, r.MinioClient.minioClient, r.MinioClient.bucketName, checkpointPath, 30*time.Second, 500*time.Millisecond); err != nil {
		log.Info(fmt.Sprintf("File did not appear in MinIO in time: %v", err))
		backup.Status.OriginalImage = container.Image
		backup.Status.LastCheckpointTime = &metav1.Time{Time: time.Now().UTC()}
		backup.Status.Phase = transitionv1.CheckpointPhaseFailed
		backup.Status.Message = fmt.Sprintf("Failed to update backup status with image name: %v", err)

		if err := r.Status().Update(ctx, backup); err != nil {
			log.Error(err, "Failed to update backup status with image name")
			return err
		}
		return err
	}

	// The previous error check is unnecessary because 'err' is always nil here.
	// if !fileExists {
	// 	log.Info("File does not exist in MinIO, skipping upload", "file", checkpointPath)
	// 	return nil
	// }

	//download file from minio
	err = miniohelper.DownloadFileFromMinio(ctx, r.MinioClient.minioClient, r.MinioClient.bucketName, objectName, checkpointPath)
	if err != nil {
		log.Error(err, "Failed to download file from MinIO")
		backup.Status.OriginalImage = container.Image
		backup.Status.LastCheckpointTime = &metav1.Time{Time: time.Now().UTC()}
		backup.Status.Phase = transitionv1.CheckpointPhaseFailed
		backup.Status.Message = fmt.Sprintf("Failed to update backup status with image name: %v", err)

		if err := r.Status().Update(ctx, backup); err != nil {
			log.Error(err, "Failed to update backup status with image name")
			return err
		}
		return err
	}

	// Step 3: Build checkpoint image
	podFullName := fmt.Sprintf("%s_%s", backup.Spec.PodRef.Namespace, backup.Spec.PodRef.Name)
	imageName := fmt.Sprintf("%s/%s/checkpoint-%s-%s:latest", backup.Spec.Registry.URL, backup.Spec.Registry.Repository, podFullName, container.Name)
	baseImage := container.Image
	if err := r.buildCheckpointImage(checkpointPath, imageName, baseImage, container.Name); err != nil {
		log.Error(err, "Failed to build checkpoint image")
		backup.Status.OriginalImage = container.Image
		backup.Status.LastCheckpointTime = &metav1.Time{Time: time.Now().UTC()}
		backup.Status.Phase = transitionv1.CheckpointPhaseFailed
		backup.Status.Message = fmt.Sprintf("Failed to update backup status with image name: %v", err)

		if err := r.Status().Update(ctx, backup); err != nil {
			log.Error(err, "Failed to update backup status with image name")
			return err
		}
		return err
	}

	// Step 4: Push image to registry
	if err := r.RegistryClient.PushImage(imageName); err != nil {
		log.Error(err, "Failed to push checkpoint image to registry")
		backup.Status.OriginalImage = container.Image
		backup.Status.LastCheckpointTime = &metav1.Time{Time: time.Now().UTC()}
		backup.Status.Phase = transitionv1.CheckpointPhaseFailed
		backup.Status.Message = fmt.Sprintf("Failed to update backup status with image name: %v", err)

		if err := r.Status().Update(ctx, backup); err != nil {
			log.Error(err, "Failed to update backup status with image name")
			return err
		}
		return err
	}

	// Step 5: Update status with image name
	backup.Status.LastCheckpointImage = imageName
	backup.Status.OriginalImage = baseImage
	backup.Status.LastCheckpointTime = &metav1.Time{Time: time.Now().UTC()}
	backup.Status.Phase = transitionv1.CheckpointPhaseCompleted
	backup.Status.Message = "Checkpoint completed successfully"

	if err := r.Status().Update(ctx, backup); err != nil {
		log.Error(err, "Failed to update backup status with image name")
		return err
	}

	//predownload images to the target cluster nodes
	err = r.PreDownloadImageToTargetCluster(ctx, imageName, container.Image, *backup)
	if err != nil {
		log.Error(err, "Failed to predownload image to all nodes on the target cluster", "image", imageName)
		// Not a fatal error, just log
	}

	//remove the downloaded checkpoint file to save space
	if err := os.Remove(checkpointPath); err != nil {
		log.Error(err, "Failed to remove downloaded checkpoint file", "file", checkpointPath)
		// Not a fatal error, just log
	}

	//delete local image to save space
	err = r.DeleteBuildImage(imageName)
	if err != nil {
		log.Error(err, "Failed to delete local image", "image", imageName)
		// Not a fatal error, just log
	}

	log.Info("Successfully checkpointed and pushed container image", "container", container.Name, "image", imageName)
	backup.Status.OriginalImage = baseImage
	backup.Status.LastCheckpointTime = &metav1.Time{Time: time.Now().UTC()}
	backup.Status.Phase = transitionv1.CheckpointPhaseCompleted
	backup.Status.LastCheckpointImage = imageName
	backup.Status.Message = "Checkpoint completed successfully"
	if err := r.Status().Update(ctx, backup); err != nil {
		log.Error(err, "Failed to update backup status")
		return err
	}

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

	// fmt.Printf("DEBUG ERROR: Kubelet checkpoint API response body: %s\n", string(body))

	var checkpointResp CheckpointResponse
	if err := json.Unmarshal(body, &checkpointResp); err != nil {
		return "", fmt.Errorf("failed to parse checkpoint response: %w (raw=%s)", err, string(body))
	}

	if len(checkpointResp.Items) == 0 {
		return "", fmt.Errorf("checkpoint API returned no items")
	}

	// fmt.Printf("DEBUG ERROR: FILENAME: %s\n", checkpointResp.Items[0])

	return checkpointResp.Items[0], nil

}

// buildCheckpointImage builds the checkpoint image using buildah
func (r *CheckpointReconciler) buildCheckpointImage(checkpointPath, imageName, baseImage, containerName string) error {
	log := logf.FromContext(context.Background())

	// Verify the checkpoint file exists (should have been found by findCheckpointFile)

	var fullCheckpointPath string
	if filepath.IsAbs(checkpointPath) {
		fullCheckpointPath = checkpointPath
	} else {
		fullCheckpointPath = filepath.Join(CheckpointBasePath, checkpointPath)
	}

	// fullCheckpointPath := filepath.Join(CheckpointBasePath, checkpointPath)
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
		"--annotation=org.criu.checkpoint.container.name="+imageName,
		newContainer).Run(); err != nil {
		return fmt.Errorf("failed to add checkpoint name annotation: %w", err)
	}

	if err := exec.Command("buildah", "config",
		"--annotation=org.criu.checkpoint.rootfsImageName="+baseImage,
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
	if err := rc.login(); err != nil {
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
func (rc *RegistryClient) login() error {
	// Use the registry URL from the secret (not extracted from image name)
	// For Docker Hub, this should be "docker.io" or can be empty

	// Login using buildah
	cmd := exec.Command("buildah", "login", "-u", "vitu1", "-p", "dckr_pat_6dfNrJ77W6R7W4cOVzi_u_FkKec", rc.registry)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to login to registry %s: %w", rc.registry, err)
	}

	return nil
}

func (r *CheckpointReconciler) DeleteBuildImage(imageName string) error {
	// Delete image using buildah
	cmd := exec.Command("buildah", "rmi", imageName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to delete image %s: %w", imageName, err)
	}

	return nil
}

// PreDownloadImageToTargetCluster pre-downloads the image to all nodes in the target cluster to avoid pull delays during recovery
func (r *CheckpointReconciler) PreDownloadImageToTargetCluster(ctx context.Context, checkpointImage, originalImage string, checkpoint transitionv1.Checkpoint) error {
	log := logf.FromContext(ctx)
	log.Info("Pre-downloading image to target cluster nodes", "image", checkpointImage, "targetCluster", checkpoint.Spec.TargetClusterRef.Name)

	// Get target cluster client
	clusterResult, targetClusterClient, _, err := capictrl.GetWorkloadClusterClient(ctx, r.Client, checkpoint.Spec.TargetClusterRef.Name)
	if err != nil {
		log.Error(err, "Failed to get target cluster client", "targetCluster", checkpoint.Spec.TargetClusterRef.Name)
		return err
	}
	if targetClusterClient == nil {
		log.Info("Target cluster client not available yet", "targetCluster", checkpoint.Spec.TargetClusterRef.Name)
		return fmt.Errorf("target cluster client not available yet: %v", clusterResult)
	}

	// List all nodes in target cluster
	var nodeList corev1.NodeList
	if err := targetClusterClient.List(ctx, &nodeList); err != nil {
		return fmt.Errorf("failed to list nodes in target cluster: %w", err)
	}

	// For each node, create a Pod that pulls the image
	for _, node := range nodeList.Items {

		//if control-plane node, skip
		if _, isControlPlane := node.Labels["node-role.kubernetes.io/control-plane"]; isControlPlane {
			log.Info("Skipping control-plane node", "node", node.Name)
			continue
		}

		namePart := sanitizeName(checkpointImage)
		maxLen := 50
		if len(namePart) > maxLen {
			h := sha1.Sum([]byte(namePart))
			namePart = namePart[:maxLen-8] + fmt.Sprintf("-%x", h)[:8]
		}
		podName := fmt.Sprintf("prepull-checkpoint-image-%s-%s", namePart, node.Name)

		var existingPod corev1.Pod
		err := targetClusterClient.Get(ctx, types.NamespacedName{
			Name:      podName,
			Namespace: "default",
		}, &existingPod)
		if err == nil {
			// Pod exists, delete it
			if err := targetClusterClient.Delete(ctx, &existingPod); err != nil {
				log.Error(err, "Failed to delete existing pre-pull pod", "pod", podName, "node", node.Name)
				return fmt.Errorf("failed to delete existing pre-pull pod %s: %w", podName, err)
			}
			log.Info("Deleted existing pre-pull pod", "pod", podName, "node", node.Name)
			// Wait a bit for deletion to complete
			time.Sleep(3 * time.Second)
		} else if !errors.IsNotFound(err) {
			//
			log.Error(err, "Failed to check for existing pre-pull pod", "pod", podName, "node", node.Name)
			return fmt.Errorf("failed to check for existing pre-pull pod %s: %w", podName, err)
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podName,
				Namespace: "default",
			},
			Spec: corev1.PodSpec{
				NodeName:      node.Name,
				RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{
					{
						Name:            "prepull",
						Image:           checkpointImage,
						Command:         []string{"sleep", "30"}, // Sleep to keep the pod alive for a short time
						ImagePullPolicy: corev1.PullAlways,
					},
				},
			},
		}

		// Create the Pod
		if err := targetClusterClient.Create(ctx, pod); err != nil {
			if !errors.IsAlreadyExists(err) {
				log.Error(err, "Failed to create pre-pull pod", "pod", podName, "node", node.Name)
				return fmt.Errorf("failed to create pre-pull pod %s: %w", podName, err)
			}
		}

		log.Info("Created pre-pull pod", "pod", podName, "node", node.Name)

		// Optionally, wait for the Pod to complete and then delete it or delete once image is pulled

		go func(podName string) {
			defer func() {
				podToDelete := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      podName,
						Namespace: "default",
					},
				}
				if err := targetClusterClient.Delete(context.Background(), podToDelete); err != nil {
					log.Error(err, "Failed to delete pre-pull pod", "pod", podName)
				} else {
					log.Info("Deleted pre-pull pod", "pod", podName)
				}
			}()

			// Wait for Pod to complete
			for {
				time.Sleep(3 * time.Second)
				var currentPod corev1.Pod
				if err := targetClusterClient.Get(context.Background(), types.NamespacedName{
					Name:      podName,
					Namespace: "default",
				}, &currentPod); err != nil {
					log.Error(err, "Failed to get pre-pull pod status", "pod", podName)
					return
				}
				if currentPod.Status.Phase == corev1.PodSucceeded || currentPod.Status.Phase == corev1.PodFailed {
					log.Info("Pre-pull pod completed", "pod", podName, "phase", currentPod.Status.Phase)
					return
				}
			}
		}(podName)
	}

	// do the same for the original image to ensure it's also pre-pulled
	if originalImage != "" && originalImage != checkpointImage {
		log.Info("Also pre-downloading original image to target cluster nodes", "image", originalImage, "targetCluster", checkpoint.Spec.TargetClusterRef.Name)

		for _, node := range nodeList.Items {

			//if control-plane node, skip
			if _, isControlPlane := node.Labels["node-role.kubernetes.io/control-plane"]; isControlPlane {
				log.Info("Skipping control-plane node", "node", node.Name)
				continue
			}

			namePart := sanitizeName(originalImage)
			maxLen := 50
			if len(namePart) > maxLen {
				h := sha1.Sum([]byte(namePart))
				namePart = namePart[:maxLen-8] + fmt.Sprintf("-%x", h)[:8]
			}

			// if pod already exists, delete it first
			podName := fmt.Sprintf("prepull-original-image-%s-%s", namePart, node.Name)
			var existingPod corev1.Pod
			err := targetClusterClient.Get(ctx, types.NamespacedName{
				Name:      podName,
				Namespace: "default",
			}, &existingPod)
			if err == nil {
				// Pod exists, delete it
				if err := targetClusterClient.Delete(ctx, &existingPod); err != nil {
					log.Error(err, "Failed to delete existing pre-pull pod", "pod", podName, "node", node.Name)
					return fmt.Errorf("failed to delete existing pre-pull pod %s: %w", podName, err)
				}
				log.Info("Deleted existing pre-pull pod", "pod", podName, "node", node.Name)
				// Wait a bit for deletion to complete
				time.Sleep(3 * time.Second)
			} else if !errors.IsNotFound(err) {
				//
				log.Error(err, "Failed to check for existing pre-pull pod", "pod", podName, "node", node.Name)
				return fmt.Errorf("failed to check for existing pre-pull pod %s: %w", podName, err)
			}

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					NodeName:      node.Name,
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "prepull",
							Image:   originalImage,
							Command: []string{"sleep", "30"}, // Sleep to keep the pod alive for a short time
							// ImagePullPolicy: corev1.PullAlways,
						},
					},
				},
			}

			// Create the Pod
			if err := targetClusterClient.Create(ctx, pod); err != nil {
				if !errors.IsAlreadyExists(err) {
					log.Error(err, "Failed to create pre-pull original pod image", "pod", podName, "node", node.Name)
					return fmt.Errorf("failed to create pre-pull original pod image %s: %w", podName, err)
				}
			}

			log.Info("Created pre-pull original pod image", "pod", podName, "node", node.Name)

			// Optionally, wait for the Pod to complete and then delete it
			go func(podName string) {
				defer func() {
					podToDelete := &corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      podName,
							Namespace: "default",
						},
					}
					if err := targetClusterClient.Delete(context.Background(), podToDelete); err != nil {
						log.Error(err, "Failed to delete pre-pull original pod image", "pod", podName)
					} else {
						log.Info("Deleted pre-pull original pod image", "pod", podName)
					}
				}()

				// Wait for Pod to complete
				for {
					time.Sleep(3 * time.Second)
					var currentPod corev1.Pod
					if err := targetClusterClient.Get(context.Background(), types.NamespacedName{
						Name:      podName,
						Namespace: "default",
					}, &currentPod); err != nil {
						log.Error(err, "Failed to get pre-pull original pod  image status", "pod", podName)
						return
					}
					if currentPod.Status.Phase == corev1.PodSucceeded || currentPod.Status.Phase == corev1.PodFailed {
						log.Info("Pre-pull original pod image completed", "pod", podName, "phase", currentPod.Status.Phase)
						return
					}
				}
			}(podName)
		}
	}

	return nil
}

func sanitizeName(s string) string {
	// Replace any invalid character with '-'
	re := regexp.MustCompile(`[^a-z0-9-.]`)
	s = strings.ToLower(s)
	s = re.ReplaceAllString(s, "-")
	// Trim leading/trailing '-'
	s = strings.Trim(s, "-")
	return s
}

// SetupWithManager sets up the controller with the Manager.
func (r *CheckpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&transitionv1.Checkpoint{}).
		Named("checkpoint").
		Complete(r)
}
