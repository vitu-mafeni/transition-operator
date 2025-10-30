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
	"sync"
	"time"

	"code.gitea.io/sdk/gitea"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/nephio-project/nephio/controllers/pkg/resource"
	transitionv1 "github.com/vitu1234/transition-operator/api/v1"
	capictrl "github.com/vitu1234/transition-operator/reconcilers/capi"
	checkpointtransition "github.com/vitu1234/transition-operator/reconcilers/checkpoint_transition"
	"github.com/vitu1234/transition-operator/reconcilers/controlplane"

	// "github.com/vitu1234/transition-operator/reconcilers/controlplane"
	giteaclient "github.com/vitu1234/transition-operator/reconcilers/gitaclient"
	helpers "github.com/vitu1234/transition-operator/reconcilers/helpers"

	velero "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	capiv1beta1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// ClusterPolicyReconciler reconciles a ClusterPolicy object
type ClusterPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

var (
	monitorRegistry   = make(map[string]context.CancelFunc)
	monitorRegistryMu sync.Mutex
)

// +kubebuilder:rbac:groups=transition.dcnlab.ssu.ac.kr,resources=clusterpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=transition.dcnlab.ssu.ac.kr,resources=clusterpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=transition.dcnlab.ssu.ac.kr,resources=clusterpolicies/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ClusterPolicy object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *ClusterPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	// println("Reconciling ClusterPolicy:")
	// fmt.Println("Request:", req)
	log := logf.FromContext(ctx)
	// log.Info("Reconciling ClusterPolicy")

	// check the heartbeat status of nodes frequently to trigger node failure handling from NodeHealth CRs
	//

	// List all Cluster resources
	clusterList := &capiv1beta1.ClusterList{}
	if err := r.Client.List(ctx, clusterList); err != nil {
		return ctrl.Result{}, err
	}
	// numClusters := len(clusterList.Items)
	// println("Number of clusters:", numClusters)

	clusterPolicy := &transitionv1.ClusterPolicy{}
	if err := r.Get(ctx, req.NamespacedName, clusterPolicy); err != nil {
		if apierrors.IsNotFound(err) {
			log.Error(err, "ClusterPolicy resource not found.")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}
		log.Error(err, "Failed to get ClusterPolicy resource")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err

	}

	// iterate through all package selectors and check if its a live package
	for _, pkg := range clusterPolicy.Spec.PackageSelectors {
		if pkg.LiveStatePackage {
			// log.Info("Found live state package", "package", pkg.Name)

			//we have to create checkpoints for this package
			err := r.CreateCheckpointForLiveStatePackage(ctx, clusterList, pkg, clusterPolicy)
			if err != nil {
				log.Error(err, "Failed to create checkpoint resource for live state package", "package", pkg.Name)
			}
		}
	}

	//get the clusters
	for _, workload_cluster := range clusterList.Items {
		// log.Info("Cluster found", "name", cluster.Name, "namespace", cluster.Namespace)
		// log.Info("Cluster details", "spec", cluster.Spec, "status", cluster.Status)
		// You can add logic here to process each cluster as needed
		if clusterPolicy.Spec.ClusterSelector.Name == workload_cluster.Name {
			// log.Info("Cluster matches ClusterPolicy selector", "cluster", cluster.Name)
			// Perform actions based on the matching cluster
			// For example, you can update the ClusterPolicy status or perform other operations
			r.performWorkloadClusterPolicyActions(ctx, clusterPolicy, &workload_cluster, req)
			// log.Info("Performed actions for matching cluster", "cluster", cluster.Name)
		} else {
			// log.Info("Cluster does not match ClusterPolicy selector - skipping", "cluster", workload_cluster.Name)

		}
	}

	go r.StartWorkloadClusterControlPlaneHealthMonitor(ctx, *clusterList, *clusterPolicy)

	// You can log or use numClusters as needed
	// Example: log the number of clusters
	// logf.FromContext(ctx).Info("Number of clusters", "count", numClusters)

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *ClusterPolicyReconciler) CreateCheckpointForLiveStatePackage(ctx context.Context, clusterList *capiv1beta1.ClusterList, pkg transitionv1.PackageSelector, clusterPolicy *transitionv1.ClusterPolicy) error {
	for _, workload_cluster := range clusterList.Items {

		if clusterPolicy.Spec.ClusterSelector.Name == workload_cluster.Name {
			err := checkpointtransition.PerformWorkloadClusterCheckpointAction(ctx, r.Client, pkg, clusterPolicy, &workload_cluster)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *ClusterPolicyReconciler) performWorkloadClusterPolicyActions(ctx context.Context, clusterPolicy *transitionv1.ClusterPolicy, cluster *capiv1beta1.Cluster, req ctrl.Request) {
	//get cluster pods and machines
	log := logf.FromContext(ctx)
	// log.Info("Performing actions for ClusterPolicy", "policy", clusterPolicy.Name, "cluster", cluster.Name)
	// Here you can implement the logic to perform actions based on the ClusterPolicy and Cluster
	// For example, you can update the ClusterPolicy status or perform other operations
	capiCluster, err := capictrl.GetCapiClusterFromName(ctx, cluster.Name, cluster.Namespace, r.Client)
	if err != nil {
		log.Error(err, "Failed to get CAPI cluster")
		return
	}

	//get cluster status
	if cluster.Status.Phase != "Provisioned" {
		log.Info("Cluster is not ready or not provisioned yet to apply cluster policy", "cluster", cluster.Name)
		return
	}

	// list cluster machines
	machineList := &capiv1beta1.MachineList{}
	if err := r.Client.List(ctx, machineList, client.InNamespace(cluster.Namespace), client.MatchingLabels{"cluster.x-k8s.io/cluster-name": cluster.Name}); err != nil {
		log.Error(err, "Failed to list machines for cluster", "cluster", cluster.Name)
		return
	}

	//get all machine statuses
	for _, machine := range machineList.Items {
		// log.Info("Machine found", "name", machine.Name, "status", machine.Status.Phase)
		//check
		if machine.Status.Phase != "Running" {
			r.handleWorkloadClusterMachine(ctx, clusterPolicy, capiCluster, &machine, req)
		}

	}

	err = r.handleNodesInWorkloadCluster(ctx, clusterPolicy, capiCluster, cluster, req)
	if err != nil {
		log.Error(err, "Failed to access node(s) in workload cluster, will run fallback logic", "cluster", cluster.Name)
	}

	// r.handlePodsInWorkloadCluster(ctx, capiCluster, cluster)
}

// Node failure
func (r *ClusterPolicyReconciler) handleNodesInWorkloadCluster(ctx context.Context, clusterPolicy *transitionv1.ClusterPolicy, capiCluster *capictrl.Capi, cluster *capiv1beta1.Cluster, req ctrl.Request) error {
	log := logf.FromContext(ctx)
	// get all pods in the cluster
	clusterClient, _, ready, err := capiCluster.GetClusterClient(ctx)
	if err != nil {
		log.Error(err, "Failed to get workload cluster client", "cluster", capiCluster.GetClusterName())
		return err
	}
	if !ready {
		log.Info("Cluster is not ready", "cluster", capiCluster.GetClusterName())
		return nil
	}

	nodeList := &corev1.NodeList{}
	if err := clusterClient.List(ctx, nodeList); err != nil {
		log.Error(err, "Failed to list pods for cluster", "cluster", cluster.Name)
		return err
	}
	// log.Info("Found nodes in cluster", "count", len(nodeList.Items))
	//list all pods in the cluster
	for _, node := range nodeList.Items {
		// log.Info("Node found", "name", node.Name, "status", node.Status, "addresses", node.Status.Addresses)
		//get node type, control-plane or worker
		status := helpers.GetNodeStatusSummary(node)
		// log.Info("Node status", "name", node.Name, "status", status)
		if status != "Ready" {
			log.Info("Node is not ready, checking conditions", "name", node.Name)
			// transition workloads on this node
			//get all pods on the machine or node
			podList := &corev1.PodList{}
			if err := clusterClient.List(ctx, podList, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
				log.Error(err, "Failed to list pods for machine", "machine", capiCluster.GetClusterName())
				return err
			}
			// log.Info("Found pods on machine", "machine", machine.Name, "count", len(podList.Items))

			// Iterate through the pods and take action based on the cluster policy
			// log.Info("Cluster Policy SelectMode", "Mode", string(clusterPolicy.Spec.SelectMode), "node", node.Name, "status", status)
			r.HandlePodsOnNodeForPolicy(ctx, clusterClient, node, podList, clusterPolicy, req, log)

		}

	}
	return nil
}

// this metthod is called when a machine is not running
// it should recover the machine by checking the cluster status and applying the cluster policy
func (r *ClusterPolicyReconciler) handleWorkloadClusterMachine(ctx context.Context, clusterPolicy *transitionv1.ClusterPolicy, capiCluster *capictrl.Capi, machine *capiv1beta1.Machine, req ctrl.Request) {
	log := logf.FromContext(ctx)
	// log.Info("Handling machine in workload cluster", "machine", machine.Name, "status", machine.Status.Phase)

	clusterClient, _, ready, err := capiCluster.GetClusterClient(ctx)
	if err != nil {
		log.Error(err, "Failed to get workload cluster client", "cluster", capiCluster.GetClusterName())
		return
	}
	if !ready {
		log.Info("Cluster is not ready", "cluster", capiCluster.GetClusterName())
		return
	}

	// or search by machine address especially on AWS if machine.Name is not there

	node := &corev1.Node{}
	err = clusterClient.Get(ctx, types.NamespacedName{Name: machine.Name}, node)
	if err != nil {
		if apierrors.IsNotFound(err) && len(machine.Status.Addresses) > 0 {
			// Try to find by address if not found by name
			err = clusterClient.Get(ctx, types.NamespacedName{Name: machine.Status.Addresses[0].Address}, node)
			if err != nil {
				if apierrors.IsNotFound(err) {
					log.Error(err, "Node not found by name or address", "node", machine.Name, "address", machine.Status.Addresses[0].Address)
					node = nil // Explicitly set node to nil if not found
				} else {
					log.Error(err, "Failed to get node by address", "address", machine.Status.Addresses[0].Address)
					return
				}
			}
		} else if err != nil {
			log.Error(err, "Failed to get node", "node", machine.Name)
			return
		}
	}

	// Determine if it's a control plane machine
	if machine.Labels["cluster.x-k8s.io/cluster-name"] == clusterPolicy.Spec.ClusterSelector.Name {
		log.Info("Control plane machine found", "machine", machine.Name)

	} else {
		log.Info("Worker machine found", "machine", machine.Name)
		hasBadConditions := false

		if node != nil {
			for _, cond := range node.Status.Conditions {
				switch cond.Type {
				case corev1.NodeReady:
					if cond.Status != corev1.ConditionTrue {
						hasBadConditions = true
					}
				case corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure, corev1.NodeNetworkUnavailable:
					if cond.Status == corev1.ConditionTrue {
						hasBadConditions = true
					}
				}
			}
		}

		status := helpers.GetNodeStatusSummary(*node)

		isMachineFailed := machine.Status.Phase == "Failed" || machine.Status.Phase == "Unknown"
		// isNodeNotRunning := node == nil || node.Status.Phase != corev1.NodeRunning

		if isMachineFailed || status != "Ready" || hasBadConditions {
			log.Info("Machine is not running, applying cluster policy", "machine", machine.Name)

			//get all pods on the machine or node
			podList := &corev1.PodList{}
			if err := clusterClient.List(ctx, podList, client.MatchingFields{"spec.nodeName": machine.Name}); err != nil {
				log.Error(err, "Failed to list pods for machine", "machine", capiCluster.GetClusterName())
				return
			}
			// log.Info("Found pods on machine", "machine", machine.Name, "count", len(podList.Items))

			r.HandlePodsOnNodeForPolicy(ctx, clusterClient, *node, podList, clusterPolicy, req, log)

		} else {
			log.Info("Skip, Machine is running", "machine", machine.Name)

		}
	}
}

func (r *ClusterPolicyReconciler) TransitionAllWorkloads(ctx context.Context, clusterClient resource.APIPatchingApplicator, clusterPolicy *transitionv1.ClusterPolicy, req ctrl.Request) {
	log := logf.FromContext(ctx)
	log.Info("Transitioning all workloads", clusterPolicy.Name)
	//transition all workloads in the cluster

}

func (r *ClusterPolicyReconciler) TransitionSelectedWorkloads(ctx context.Context, clusterClient resource.APIPatchingApplicator, pod *corev1.Pod, transitionPackage transitionv1.PackageSelector, clusterPolicy *transitionv1.ClusterPolicy, req ctrl.Request) {
	log := logf.FromContext(ctx)
	// log.Info("Transitioning Selected workload", "pod", pod.Name, "package", transitionPackage.Name)

	apiClient := resource.NewAPIPatchingApplicator(r.Client)
	giteaClient, err := giteaclient.GetClient(ctx, apiClient)
	if err != nil {
		log.Error(err, "Failed to initialize Gitea client")
		return
	}
	if !giteaClient.IsInitialized() {
		log.Info("Gitea client not yet initialized, retrying later")
		return
	}

	user, resp, err := giteaClient.GetMyUserInfo()
	if err != nil {
		log.Error(err, "Failed to get Gitea user info", "response", resp)
		return
	}
	// log.Info("Authenticated with Gitea", "username", user.UserName)

	sourceRepo := clusterPolicy.Spec.ClusterSelector.Repo
	if sourceRepo == "" {
		log.Info("ClusterPolicy does not specify source repo; skipping transition")
		return
	}

	repos, resp, err := giteaClient.Get().ListMyRepos(gitea.ListReposOptions{})
	if err != nil {
		log.Error(err, "Failed to list Gitea repositories", "response", resp)
		return
	}
	if len(repos) == 0 {
		log.Info("No repositories found for user", "username", user.UserName)
		return
	}

	var drRepo *gitea.Repository
	for i := range repos {
		if repos[i].Name == "dr" {
			drRepo = repos[i]
			break
		}
	}
	if drRepo == nil {
		log.Info("Repository named 'dr' not found; using last repository as fallback, skipping transition", clusterPolicy.Name)
		// drRepo = repos[len(repos)-1]
		return
	}

	helpers.LogRepositories(log, repos)
	// log.Info("Source repository", "cluster", clusterPolicy.Spec.ClusterSelector.Name, "repo", sourceRepo)

	targetRepoName, targetClusterName, found := helpers.DetermineTargetRepo(clusterPolicy, log)
	if !found {
		log.Info("No suitable target repository found; canceling transition")
		return
	}

	_, err, targetClusterClient := r.GetWorkloadClusterClientByName(ctx, targetClusterName)
	if err != nil {
		log.Error(err, "Failed to get target workload cluster client", "cluster", targetClusterName)
		return
	}

	switch transitionPackage.PackageType {
	case transitionv1.PackageTypeStateful:
		// log.Info("Handling stateful package transition", "package", transitionPackage.Name)

		backupMatching := transitionv1.BackupInformation{}

		for _, backup := range transitionPackage.BackupInformation {
			switch backup.BackupType {
			case transitionv1.BackupTypeSchedule:

				backupListVelero := &velero.BackupList{}
				if err := clusterClient.List(ctx, backupListVelero, &client.ListOptions{
					Namespace: "velero", // Or leave blank for all namespaces (if using client.Cluster),
				}); err != nil {
					log.Error(err, "failed to list registered velero backups")
				}

				if len(backupListVelero.Items) == 0 {
					log.Info("No velero backups found.")
					return
				}

				var latestTime time.Time
				var foundValidBackup bool

				for _, veleroBackup := range backupListVelero.Items {
					scheduleName, ok := veleroBackup.Labels["velero.io/schedule-name"]
					if !ok || scheduleName != backup.Name {
						continue
					}

					// Skip backups not in a successful phase
					if veleroBackup.Status.Phase != "Completed" {
						log.Info("Skipping Velero backup with non-successful phase", "backup", veleroBackup.Name, "phase", veleroBackup.Status.Phase)
						continue
					}

					created := veleroBackup.CreationTimestamp.Time
					if latestTime.IsZero() || created.After(latestTime) {
						latestTime = created
						backupMatching.Name = veleroBackup.Name
						backupMatching.BackupType = backup.BackupType
						foundValidBackup = true
					}
				}

				if foundValidBackup {
					log.Info("Found latest successful velero backup from Velero Schedule", "backup", backupMatching.Name, "createdAt", latestTime)
				} else {
					log.Info("No successful velero backups found for Velero Schedule", "schedule-name", backup.Name)
				}

			case transitionv1.BackupTypeManual:
				// scheme := runtime.NewScheme()
				// _ = velero.AddToScheme(scheme)
				backupListVelero := &velero.BackupList{}
				if err := clusterClient.List(ctx, backupListVelero, &client.ListOptions{
					Namespace: "velero", // Or leave blank for all namespaces (if using client.Cluster),
				}); err != nil {
					log.Error(err, "failed to list registered velero backups")
				}

				if len(backupListVelero.Items) == 0 {
					log.Info("No velero backups found.")
					return
				}

				var latestTime time.Time
				var foundValidBackup bool

				for _, veleroBackup := range backupListVelero.Items {

					if veleroBackup.Name != backup.Name {
						continue
					}

					// Skip backups not in a successful phase
					if veleroBackup.Status.Phase != "Completed" {
						log.Info("Skipping Velero backup with non-successful phase", "backup", veleroBackup.Name, "phase", veleroBackup.Status.Phase)
						continue
					}

					created := veleroBackup.CreationTimestamp.Time
					if latestTime.IsZero() || created.After(latestTime) {
						latestTime = created
						backupMatching.Name = veleroBackup.Name
						backupMatching.BackupType = backup.BackupType
						foundValidBackup = true
					}
					if foundValidBackup {
						log.Info("Found latest successful velero backup", "backup", backupMatching.Name, "createdAt", latestTime)
					} else {
						log.Info("No successful velero backups found for schedule", "schedule-name", backup.Name)
					}

				}

			}
		}

		//backup name cannot be empty

		if backupMatching.Name == "" {
			log.Error(err, "Failed to find a proper backup - backup name cannot be empty")
			return
		}

		_, err := helpers.CreateAndPushVeleroRestore(ctx, giteaClient.Get(), user.UserName, drRepo.Name, targetRepoName, clusterPolicy, transitionPackage, log, backupMatching, targetClusterClient, targetClusterName+"-dr")
		if err != nil {
			log.Error(err, "Failed to push Velero manifest")
			//add status that it failed to transition the package
			clusterPolicy.Status.TransitionedPackages = append(clusterPolicy.Status.TransitionedPackages, transitionv1.TransitionedPackages{
				PackageSelectors:           []transitionv1.PackageSelector{transitionPackage},
				LastTransitionTime:         metav1.Now(),
				PackageTransitionCondition: transitionv1.PackageTransitionConditionFailed,
				PackageTransitionMessage:   err.Error(),
			})

			if err := r.Status().Update(ctx, clusterPolicy); err != nil {
				log.Error(err, "Failed to update ClusterPolicy status after transition failure")
				return
			}
			return
		}

		message := "Transitioned stateful package successfully"

		err = helpers.TriggerArgoCDSyncWithKubeClient(targetClusterClient, targetClusterName+"-dr", "argocd")
		if err != nil {
			log.Error(err, "Failed to trigger ArgoCD sync with kube client")
			message += "; but the ArgoCD sync was not triggered successfully"
		}
		clusterPolicy.Status.TransitionedPackages = append(clusterPolicy.Status.TransitionedPackages, transitionv1.TransitionedPackages{
			PackageSelectors:           []transitionv1.PackageSelector{transitionPackage},
			LastTransitionTime:         metav1.Now(),
			PackageTransitionCondition: transitionv1.PackageTransitionConditionCompleted,
			PackageTransitionMessage:   message,
		})

		if err := r.Status().Update(ctx, clusterPolicy); err != nil {
			log.Error(err, "Failed to update ClusterPolicy status after successful stateful transition")
			return
		}
		log.Info("Successfully transitioned stateful package", "package", transitionPackage.Name, "clusterPolicy", clusterPolicy.Name)
		return

	case transitionv1.PackageTypeStateless:
		log.Info("Handling stateless package transition", "package", transitionPackage.Name)
		ignoreDifferences := []helpers.ArgoAppSkipResourcesIgnoreDifferences{}
		_, err := helpers.CreateAndPushArgoApp(ctx, giteaClient.Get(), user.UserName, drRepo.Name, targetRepoName, clusterPolicy, transitionPackage, ignoreDifferences, log)
		if err != nil {
			log.Error(err, "Failed to push ArgoCD app manifest")
			//add status that it failed to transition the package
			clusterPolicy.Status.TransitionedPackages = append(clusterPolicy.Status.TransitionedPackages, transitionv1.TransitionedPackages{
				PackageSelectors:           []transitionv1.PackageSelector{transitionPackage},
				LastTransitionTime:         metav1.Now(),
				PackageTransitionCondition: transitionv1.PackageTransitionConditionFailed,
				PackageTransitionMessage:   err.Error(),
			})

			if err := r.Status().Update(ctx, clusterPolicy); err != nil {
				log.Error(err, "Failed to update ClusterPolicy status after transition failure")
				return
			}
			return
		}

		message := "Transitioned stateless package successfully"

		err = helpers.TriggerArgoCDSyncWithKubeClient(targetClusterClient, targetClusterName+"-dr", "argocd")
		if err != nil {
			log.Error(err, "Failed to trigger ArgoCD sync with kube client")
			message += "; but the ArgoCD sync was not triggered successfully"
		}
		clusterPolicy.Status.TransitionedPackages = append(clusterPolicy.Status.TransitionedPackages, transitionv1.TransitionedPackages{
			PackageSelectors:           []transitionv1.PackageSelector{transitionPackage},
			LastTransitionTime:         metav1.Now(),
			PackageTransitionCondition: transitionv1.PackageTransitionConditionCompleted,
			PackageTransitionMessage:   message,
		})

		if err := r.Status().Update(ctx, clusterPolicy); err != nil {
			log.Error(err, "Failed to update ClusterPolicy status after successful transition")
			return
		}
		log.Info("Successfully transitioned stateless package", "package", transitionPackage.Name, "clusterPolicy", clusterPolicy.Name)
		return

	default:
		log.Info("Unknown package type; skipping transition", "package", transitionPackage.Name)
	}

}

// HandlePodsOnNodeForPolicy processes pods on a specific node based on the cluster policy.
// It checks the owning ReplicaSet and Deployment of each pod and applies the transition policy if applicable.
// It handles both specific and all pod selection modes as defined in the cluster policy.
// If the cluster policy specifies a specific selection mode, it will only apply the transition to pods that match the specified criteria.
// If the cluster policy specifies an all selection mode, it will apply the transition to all pods on the node.
// If no selection mode is specified, it will apply the transition to all pods on the node.
// This function is called when a node is not ready or has bad conditions, and it processes the pods on that node accordingly.
func (r *ClusterPolicyReconciler) HandlePodsOnNodeForPolicy(
	ctx context.Context,
	clusterClient resource.APIPatchingApplicator,
	node corev1.Node,
	podList *corev1.PodList,
	clusterPolicy *transitionv1.ClusterPolicy,
	req ctrl.Request,
	log logr.Logger,
) {
	// if clusterPolicy.Spec.SelectMode == transitionv1.SelectSpecific {
	// 	log.Info("Applying transition policy to specific pods on node", "node", node.Name)

	processed := make(map[string]struct{}) // To avoid duplicate transitions

	for _, pod := range podList.Items {
		namespace := pod.Namespace
		workloadKind, workloadName, hasOwner := helpers.GetWorkloadOwnerControllerInfo(pod)
		workloadID := fmt.Sprintf("%s/%s/%s", namespace, workloadName, workloadKind)

		var annotations map[string]string

		// --- Step 1: Pod-level annotations ---
		if !hasOwner {
			// If no owner, skip to avoid pod-level checkpoint creation
			log.Info("Pod has no owner; skipping pod-level checkpoint creation", "pod", pod.Name)
			continue
			// If you want to enable pod-level checkpoint creation, uncomment the following line
			// annotations = pod.Annotations

		}

		// --- Step 2: Parent workload annotations ---
		parentObject := &metav1.PartialObjectMetadata{}
		if hasOwner {
			parentAnnotations, parentKind, err := helpers.GetParentAnnotations(ctx, clusterClient, workloadKind, workloadName, namespace)
			if err != nil {
				log.Error(fmt.Errorf("an error occured finding object parent"), err.Error())
			}
			if parentAnnotations != nil {
				annotations = parentAnnotations.Annotations
				parentObject.Kind = parentKind
				parentObject.Name = parentAnnotations.Name
				parentObject.Namespace = parentAnnotations.Namespace
			}
		}

		if annotations == nil {
			continue
		}

		for _, transitionPackage := range clusterPolicy.Spec.PackageSelectors {
			if annotations["transition.dcnlab.ssu.ac.kr/cluster-policy"] == "true" &&
				annotations["transition.dcnlab.ssu.ac.kr/packageName"] == transitionPackage.Name {

				if helpers.IsPackageTransitioned(clusterPolicy, transitionPackage) {
					log.Info("Package already transitioned; skipping", "package", transitionPackage.Name, "pod", pod.Name)
					continue
				}

				// Deduplicate by workload and package
				key := fmt.Sprintf("%s/%s", workloadID, transitionPackage.Name)
				if _, seen := processed[key]; seen {
					continue
				}
				processed[key] = struct{}{}

				// log.Info("Matched workload for transition", "workload", workloadID, "package", transitionPackage.Name, "pod", pod.Name)
				if transitionPackage.LiveStatePackage {
					log.Info("Handling Live package", "package", transitionPackage.Name, "pod", pod.Name)
					r.TransitionSelectedLiveWorkloads(ctx, clusterClient, &pod, transitionPackage, clusterPolicy, req)
				} else {
					r.TransitionSelectedWorkloads(ctx, clusterClient, &pod, transitionPackage, clusterPolicy, req)
				}

			}
		}
	}

	// } else if clusterPolicy.Spec.SelectMode == transitionv1.SelectAll {
	// 	r.TransitionAllWorkloads(ctx, clusterClient, clusterPolicy, req)
	// } else {
	// 	log.Info("Invalid or unspecified select mode; skipping policy application")
	// }
}

func (r *ClusterPolicyReconciler) TransitionSelectedLiveWorkloads(ctx context.Context, clusterClient resource.APIPatchingApplicator, pod *corev1.Pod, transitionPackage transitionv1.PackageSelector, clusterPolicy *transitionv1.ClusterPolicy, req ctrl.Request) {
	log := logf.FromContext(ctx)
	// log.Info("Transitioning Selected live workload", "pod", pod.Name, "package", transitionPackage.Name)

	apiClient := resource.NewAPIPatchingApplicator(r.Client)
	giteaClient, err := giteaclient.GetClient(ctx, apiClient)
	if err != nil {
		log.Error(err, "Failed to initialize Gitea client")
		return
	}
	if !giteaClient.IsInitialized() {
		log.Info("Gitea client not yet initialized, retrying later")
		return
	}

	user, resp, err := giteaClient.GetMyUserInfo()
	if err != nil {
		log.Error(err, "Failed to get Gitea user info", "response", resp)
		return
	}
	// log.Info("Authenticated with Gitea", "username", user.UserName)

	sourceRepo := clusterPolicy.Spec.ClusterSelector.Repo
	if sourceRepo == "" {
		log.Info("ClusterPolicy does not specify source repo; skipping transition")
		return
	}

	repos, resp, err := giteaClient.Get().ListMyRepos(gitea.ListReposOptions{})
	if err != nil {
		log.Error(err, "Failed to list Gitea repositories", "response", resp)
		return
	}
	if len(repos) == 0 {
		log.Info("No repositories found for user", "username", user.UserName)
		return
	}

	var drRepo *gitea.Repository
	for i := range repos {
		if repos[i].Name == "dr" {
			drRepo = repos[i]
			break
		}
	}
	if drRepo == nil {
		log.Info("Repository named 'dr' not found; using last repository as fallback, skipping transition", clusterPolicy.Name)
		// drRepo = repos[len(repos)-1]
		return
	}

	helpers.LogRepositories(log, repos)
	// log.Info("Source repository", "cluster", clusterPolicy.Spec.ClusterSelector.Name, "repo", sourceRepo)

	targetRepoName, targetClusterName, found := helpers.DetermineTargetRepo(clusterPolicy, log)
	if !found {
		log.Info("No suitable target repository found; canceling transition")
		return
	}

	_, err, targetClusterClient := r.GetWorkloadClusterClientByName(ctx, targetClusterName)
	if err != nil {
		log.Error(err, "Failed to get target workload cluster client", "cluster", targetClusterName)
		return
	}
	associatedCheckpoint := transitionv1.Checkpoint{}

	switch transitionPackage.PackageType {
	case transitionv1.PackageTypeStateful:
		// log.Info("Handling stateful live package transition", "package", transitionPackage.Name)

		backupMatching := transitionv1.BackupInformation{}

		for _, backup := range transitionPackage.BackupInformation {
			switch backup.BackupType {
			case transitionv1.BackupTypeSchedule:

				checkpointName := fmt.Sprintf("checkpoint-%s-%s-%s", clusterPolicy.Name, pod.Name, transitionPackage.Name)
				//get checkpoint
				checkpoint := &transitionv1.Checkpoint{}
				err := r.Client.Get(ctx, types.NamespacedName{Name: checkpointName, Namespace: "default"}, checkpoint)
				if err != nil {
					log.Error(err, "Failed to get checkpoint", "name", checkpointName)
					return
				}
				if checkpoint.Status.LastCheckpointImage == "" {
					log.Info("Checkpoint has no associated backup; cannot proceed with live state transition", "checkpoint", checkpointName)
					return
				}

				associatedCheckpoint = *checkpoint

			case transitionv1.BackupTypeManual:
				// scheme := runtime.NewScheme()
				// _ = velero.AddToScheme(scheme)
				log.Info("Looking for stateful live manual backup - nothing yet", "name", backup.Name)

			}
		}

		if associatedCheckpoint.Name == "" {
			log.Error(err, "Failed to find a proper associated live backup - backup name cannot be empty")
			return
		}

		tmpDir, matches, err := giteaclient.CheckRepoForMatchingManifests(ctx, sourceRepo, "main", &associatedCheckpoint.Spec.ResourceRef)

		if err != nil {
			log.Error(err, "Failed to find matching manifests in source repo", "repo", sourceRepo)
			return
		}

		if len(matches) > 0 {
			log.Info("Found matching manifests",
				"repo", sourceRepo,
				"tmpDir", tmpDir,
				"files", matches)

			for _, f := range matches {
				// fullPath := filepath.Join(tmpDir, f)
				if err := giteaclient.UpdateResourceContainers(f, associatedCheckpoint.Status.LastCheckpointImage, associatedCheckpoint.Status.OriginalImage); err != nil {
					log.Error(err, "failed to update containers in manifest", "file", f)
					return
				}
				log.Info("Updated containers in manifest", "file", f)
			}

			// commit & push changes back to Gitea
			// log.Info("will commit and push changes back to git here")
			commitMsg := fmt.Sprintf("Update container image %s/%s", associatedCheckpoint.Status.LastCheckpointImage, associatedCheckpoint.Status.OriginalImage)

			username, password, _, err := giteaclient.GetGiteaSecretUserNamePassword(ctx, r.Client)
			if err != nil {
				log.Error(err, "failed to get gitea")
			}

			if err := giteaclient.CommitAndPush(ctx, tmpDir, "main", sourceRepo, username, password, commitMsg); err != nil {
				log.Error(err, "failed to commit & push changes")
			}

			// log.Info("Changes committed and pushed", "repo", sourceRepo)

			_, err = helpers.CreateAndPushLiveStateBackupRestore(ctx, giteaClient.Get(), user.UserName, drRepo.Name, targetRepoName, clusterPolicy, transitionPackage, log, backupMatching, associatedCheckpoint, r.Client, targetClusterName+"-dr")
			if err != nil {
				log.Error(err, "Failed to push live workloads pod manifest")
				//add status that it failed to transition the package
				clusterPolicy.Status.TransitionedPackages = append(clusterPolicy.Status.TransitionedPackages, transitionv1.TransitionedPackages{
					PackageSelectors:           []transitionv1.PackageSelector{transitionPackage},
					LastTransitionTime:         metav1.Now(),
					PackageTransitionCondition: transitionv1.PackageTransitionConditionFailed,
					PackageTransitionMessage:   err.Error(),
				})

				if err := r.Status().Update(ctx, clusterPolicy); err != nil {
					log.Error(err, "Failed to update ClusterPolicy status after transition failure")
					return
				}
				return
			}

			message := "Transitioned stateful package successfully"

			err = helpers.TriggerArgoCDSyncWithKubeClient(targetClusterClient, targetClusterName+"-dr", "argocd")
			if err != nil {
				log.Error(err, "Failed to trigger ArgoCD sync with kube client")
				message += "; but the ArgoCD sync was not triggered successfully"
			}
			clusterPolicy.Status.TransitionedPackages = append(clusterPolicy.Status.TransitionedPackages, transitionv1.TransitionedPackages{
				PackageSelectors:           []transitionv1.PackageSelector{transitionPackage},
				LastTransitionTime:         metav1.Now(),
				PackageTransitionCondition: transitionv1.PackageTransitionConditionCompleted,
				PackageTransitionMessage:   message,
			})

			if err := r.Status().Update(ctx, clusterPolicy); err != nil {
				log.Error(err, "Failed to update ClusterPolicy status after successful stateful transition")
				return
			}
			log.Info("Successfully transitioned stateful package", "package", transitionPackage.Name, "clusterPolicy", clusterPolicy.Name)
			return
		}
	case transitionv1.PackageTypeStateless:
		log.Info("Handling stateless package transition", "package", transitionPackage.Name)
		ignoreDifferences := []helpers.ArgoAppSkipResourcesIgnoreDifferences{}
		_, err := helpers.CreateAndPushArgoApp(ctx, giteaClient.Get(), user.UserName, drRepo.Name, targetRepoName, clusterPolicy, transitionPackage, ignoreDifferences, log)
		if err != nil {
			log.Error(err, "Failed to push ArgoCD app manifest")
			//add status that it failed to transition the package
			clusterPolicy.Status.TransitionedPackages = append(clusterPolicy.Status.TransitionedPackages, transitionv1.TransitionedPackages{
				PackageSelectors:           []transitionv1.PackageSelector{transitionPackage},
				LastTransitionTime:         metav1.Now(),
				PackageTransitionCondition: transitionv1.PackageTransitionConditionFailed,
				PackageTransitionMessage:   err.Error(),
			})

			if err := r.Status().Update(ctx, clusterPolicy); err != nil {
				log.Error(err, "Failed to update ClusterPolicy status after transition failure")
				return
			}
			return
		}

		message := "Transitioned stateless package successfully"

		err = helpers.TriggerArgoCDSyncWithKubeClient(targetClusterClient, targetClusterName+"-dr", "argocd")
		if err != nil {
			log.Error(err, "Failed to trigger ArgoCD sync with kube client")
			message += "; but the ArgoCD sync was not triggered successfully"
		}
		clusterPolicy.Status.TransitionedPackages = append(clusterPolicy.Status.TransitionedPackages, transitionv1.TransitionedPackages{
			PackageSelectors:           []transitionv1.PackageSelector{transitionPackage},
			LastTransitionTime:         metav1.Now(),
			PackageTransitionCondition: transitionv1.PackageTransitionConditionCompleted,
			PackageTransitionMessage:   message,
		})

		if err := r.Status().Update(ctx, clusterPolicy); err != nil {
			log.Error(err, "Failed to update ClusterPolicy status after successful transition")
			return
		}
		log.Info("Successfully transitioned stateless package", "package", transitionPackage.Name, "clusterPolicy", clusterPolicy.Name)
		return

	default:
		log.Info("Unknown package type; skipping transition", "package", transitionPackage.Name)
	}

}

// trigger migration of workloads on a cluster node is unhealthy
func (r *ClusterPolicyReconciler) ReconcileClusterPoliciesForNode(ctx context.Context, nodeName, clusterName string) error {
	// 1. Create checkpoint CR
	// 2. Trigger migration workflow
	// 3. Patch ClusterPolicy status
	// (You already have this flow inside Reconcile, reuse here)
	log := logf.FromContext(ctx)
	// log.Info("Handling node failure controller", "node", nodeName, "cluster", clusterName)

	// trigger migration
	if err := r.triggerMigrationNodeCP(ctx, clusterName, "heartbeat node unhealthy"); err != nil {
		log.Error(err, "Failed to trigger migration")
		return err
	}

	return nil
}

func (r *ClusterPolicyReconciler) triggerMigrationNodeCP(ctx context.Context, clusterName, migrationType string) error {
	log := logf.FromContext(ctx)
	log.Info("Triggering migration from "+migrationType, "cluster", clusterName)

	req := r.Client

	// workloadCluster := &capiv1beta1.Cluster{}
	// err := req.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: "default"}, workloadCluster)
	// if err != nil {
	// 	log.Error(err, "Failed to get workload cluster", "cluster", clusterName)
	// 	return err
	// }

	// // List all Cluster resources
	// clusterList := &capiv1beta1.ClusterList{}
	// if err := req.List(ctx, clusterList); err != nil {
	// 	return err
	// }

	// _, err, targetClusterClient := r.GetWorkloadClusterClientByName(ctx, clusterName)
	// if err != nil {
	// 	log.Error(err, "Failed to get target workload cluster client", "cluster", clusterName)
	// 	return err
	// }

	clusterPolicy := &transitionv1.ClusterPolicy{}
	clusterPolicyList := &transitionv1.ClusterPolicyList{}
	if err := req.List(ctx, clusterPolicyList); err != nil {
		// log.Error(err, "Failed to list ClusterPolicies")
		return err
	}

	// Find the ClusterPolicy associated with the given clusterName
	for _, cp := range clusterPolicyList.Items {
		if cp.Spec.ClusterSelector.Name == clusterName {
			clusterPolicy = &cp
			break
		}
	}

	if clusterPolicy.Name == "" {
		// log.Info("No ClusterPolicy found for cluster", "cluster", clusterName)
		return fmt.Errorf("no ClusterPolicy found for cluster %s", clusterName)
	}
	// log.Info("Found ClusterPolicy for cluster true true", "clusterPolicy", clusterPolicy.Name, "cluster", clusterName)

	// iterate through all package selectors and check if its a live package
	for _, pkg := range clusterPolicy.Spec.PackageSelectors {

		if pkg.LiveStatePackage {
			// log.Info("Found live state package from heartbeat", "package", pkg.Name)

			//we have to create transition on missed node health for this package
			err := checkpointtransition.TriggerTransitionOnMissedNodeHealth(ctx, r.Client, pkg, clusterPolicy, clusterName)

			if err != nil {
				log.Error(err, "Failed to get cluster resources info for package", "package", pkg.Name)
			}
		}

	}
	// log.Info("error Completed triggering migration from "+migrationType+" for cluster", "cluster", clusterName)
	return nil
}

func (r *ClusterPolicyReconciler) mapClusterToClusterPolicy(ctx context.Context, obj client.Object) []reconcile.Request {
	// log := logf.FromContext(ctx)
	// log.Info("Mapping Cluster to ClusterPolicy", "cluster", obj.GetName())

	// Assuming the ClusterPolicy is named after the Cluster
	// clusterName := obj.GetName()
	// policyName := fmt.Sprintf("%s-policy", clusterName)

	// Create a request for the corresponding ClusterPolicy
	// return []reconcile.Request{
	// 	{NamespacedName: types.NamespacedName{Name: policyName}},
	// }
	return []reconcile.Request{}
}

func (r *ClusterPolicyReconciler) GetWorkloadClusterClientByName(ctx context.Context, clusterName string) (*capictrl.Capi, error, resource.APIPatchingApplicator) {
	log := logf.FromContext(ctx)
	// log.Info("Getting CAPI client for cluster", "clusterName", clusterName)

	capiCluster, err := capictrl.GetCapiClusterFromName(ctx, clusterName, "default", r.Client)
	if err != nil {
		log.Error(err, "Failed to get CAPI cluster")
		return nil, err, resource.APIPatchingApplicator{}
	}

	clusterClient, _, ready, err := capiCluster.GetClusterClient(ctx)
	if err != nil {
		log.Error(err, "Failed to get workload cluster client", "cluster", capiCluster.GetClusterName())
		return nil, err, resource.APIPatchingApplicator{}
	}
	if !ready {
		log.Info("Cluster is not ready", "cluster", capiCluster.GetClusterName())
		return nil, fmt.Errorf("cluster is not ready"), resource.APIPatchingApplicator{}
	}

	return capiCluster, nil, clusterClient

}

// code for control-plane node failure handling

// StartWorkloadClusterControlPlaneHealthMonitor runs a lightweight control-plane checker
// StartWorkloadClusterControlPlaneHealthMonitor runs a lightweight control-plane checker
// that periodically verifies if a cluster's API server and control-plane nodes are healthy.
func (r *ClusterPolicyReconciler) StartWorkloadClusterControlPlaneHealthMonitor(
	ctx context.Context,
	clusters capiv1beta1.ClusterList,
	clusterPolicy transitionv1.ClusterPolicy,
) {

	log := logf.FromContext(ctx)
	clusterName := clusterPolicy.Spec.ClusterSelector.Name

	// --- Ensure only one monitor per cluster ---
	monitorRegistryMu.Lock()
	if _, exists := monitorRegistry[clusterName]; exists {
		log.Info("Control plane monitor already running", "cluster", clusterName)
		monitorRegistryMu.Unlock()
		return
	}
	monitorCtx, cancel := context.WithCancel(ctx)
	monitorRegistry[clusterName] = cancel
	monitorRegistryMu.Unlock()

	log.Info("Starting control plane health monitor", "cluster", clusterName)

	go func() {
		defer func() {
			monitorRegistryMu.Lock()
			delete(monitorRegistry, clusterName)
			monitorRegistryMu.Unlock()
			log.Info("Stopped control plane health monitor", "cluster", clusterName)
		}()

		const checkInterval = 1 * time.Second
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				capiCluster, err := capictrl.GetCapiClusterFromName(monitorCtx, clusterName, "default", r.Client)
				if err != nil {
					log.Error(err, "Failed to get CAPI cluster", "cluster", clusterName)
					continue
				}

				clusterClient, _, ready, err := capiCluster.GetClusterClient(monitorCtx)
				if err != nil || !ready {
					log.Info("Cluster not ready yet; retrying", "cluster", clusterName)
					continue
				}

				checkCtx, cancelCheck := context.WithTimeout(monitorCtx, 2*time.Second)
				status, err := controlplane.CheckControlPlaneStatus(checkCtx, clusterClient)
				cancelCheck()

				if err != nil {
					status = controlplane.ControlPlaneUnreachable
				}

				log.Info("Control plane status: "+string(status), "cluster", clusterName)

				if status != controlplane.ControlPlaneReady {

					var refreshedPolicy transitionv1.ClusterPolicy
					if err := r.Client.Get(ctx, types.NamespacedName{
						Name:      clusterPolicy.Name,
						Namespace: clusterPolicy.Namespace,
					}, &refreshedPolicy); err != nil {
						log.Error(err, "Failed to fetch latest ClusterPolicy", "cluster", clusterName)
						continue
					}

					if isClusterPolicyStatusEmpty(refreshedPolicy.Status) {

						log.Info("Triggering migration reconciliation due to control plane issue", "cluster", clusterName)
						if err := r.triggerMigrationNodeCP(ctx, clusterName, "control-plane unhealthy or fault"); err != nil {
							log.Error(err, "Failed to trigger migration")
						}
					}
				}

			case <-monitorCtx.Done():
				return
			}
		}
	}()

}
func isClusterPolicyStatusEmpty(status transitionv1.ClusterPolicyStatus) bool {

	return len(status.TransitionedPackages) == 0
}

/*
func (r *ClusterPolicyReconciler) StartWorkloadClusterControlPlaneHealthMonitor(
	ctx context.Context,
	clusters capiv1beta1.ClusterList,
	clusterPolicy transitionv1.ClusterPolicy,
) {
	log := logf.FromContext(ctx)

	capiCluster, err := capictrl.GetCapiClusterFromName(ctx, clusterPolicy.Spec.ClusterSelector.Name, "default", r.Client)
	if err != nil {
		log.Error(err, "Failed to get CAPI cluster for control plane health monitor",
			"cluster", clusterPolicy.Spec.ClusterSelector.Name)
		return
	}

	clusterClient, _, ready, err := capiCluster.GetClusterClient(ctx)
	if err != nil {
		log.Error(err, "Failed to get workload cluster client for control plane health monitor",
			"cluster", capiCluster.GetClusterName())
		return
	}
	if !ready {
		log.Info("Cluster not ready for control plane health monitor",
			"cluster", capiCluster.GetClusterName())
		return
	}

	workloadClusterClient := clusterClient
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var lastStatus controlplane.ControlPlaneStatus = "Unknown"

	log.Info("control plane health monitor",
		"cluster", capiCluster.GetClusterName())

	for {
		select {
		case <-ticker.C:
			status, err := controlplane.CheckControlPlaneStatus(ctx, workloadClusterClient)
			if err != nil {
				status = controlplane.ControlPlaneUnreachable
			}

			// --- Only log on state change ---
			if status != lastStatus {
				switch status {
				case controlplane.ControlPlaneReady:
					log.Info("Control plane is healthy",
						"cluster", capiCluster.GetClusterName())
				case controlplane.ControlPlaneUnreachable:
					log.Error(err, "Control plane is unreachable",
						"cluster", capiCluster.GetClusterName())
				default:
					log.Info("Control plane is unhealthy",
						"cluster", capiCluster.GetClusterName(),
						"status", status)
				}

				lastStatus = status

				if status != controlplane.ControlPlaneReady {
					log.Info("Triggering reconciliation due to control plane issue",
						"cluster", capiCluster.GetClusterName())
					// You can trigger reconcile here if needed
				}
			}

		case <-ctx.Done():
			log.Info("Stopping control plane health monitor",
				"cluster", capiCluster.GetClusterName())
			return
		}
	}
}
*/

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {

	return builder.ControllerManagedBy(mgr).
		For(&transitionv1.ClusterPolicy{}).
		Watches(
			&capiv1beta1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.mapClusterToClusterPolicy),
		).
		Watches(
			&v1alpha1.Application{},
			handler.EnqueueRequestsFromMapFunc(r.mapClusterToClusterPolicy),
		).
		Complete(r)
}
