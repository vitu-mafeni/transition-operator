package giteaclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	argov1alpha1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	gitplumbing "github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	transitionv1 "github.com/vitu1234/transition-operator/api/v1"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

type IPInfo struct {
	Address string
	Gateway string
}

type NadMasterInterface struct {
	MasterInterfaceHost string
}

type MatchingNadFiles struct {
	File string
	Name string
}

// CheckRepoForMatchingManifests clones a repo and searches YAML files for a name/namespace match.
func CheckRepoForMatchingManifests(
	ctx context.Context,
	repoURL string,
	branch string,
	resourceRef *transitionv1.ResourceRef,
) (cloneDirectory string, matchingFiles []string, err error) {

	log := logf.FromContext(ctx)
	log.Info("url " + repoURL)

	// clone into a temp dir
	tmpDir, err := os.MkdirTemp("", "transition-manifests-*")
	if err != nil {
		return "", nil, err
	}

	_, err = git.PlainCloneContext(ctx, tmpDir, false, &git.CloneOptions{
		URL:           repoURL,
		ReferenceName: gitplumbing.ReferenceName("refs/heads/" + branch),
		Depth:         1,
		SingleBranch:  true,
	})
	if err != nil {
		return "", nil, fmt.Errorf("git clone failed: %w", err)
	}

	var matches []string

	walkFn := func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if ext := strings.ToLower(filepath.Ext(path)); ext != ".yaml" && ext != ".yml" {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		// A file can contain multiple YAML documents separated by ---
		dec := yaml.NewDecoder(bytes.NewReader(data))
		for {
			// Minimal struct with kind, name, namespace
			var obj struct {
				Kind     string `yaml:"kind"`
				Metadata struct {
					Name      string `yaml:"name"`
					Namespace string `yaml:"namespace"`
				} `yaml:"metadata"`
			}

			if err := dec.Decode(&obj); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return err
			}
			// namespace := ""
			// if obj.Metadata.Namespace == "" {
			// 	namespace = "default"
			// }

			// if obj.Kind == resourceRef.Kind &&
			// 	obj.Metadata.Name == resourceRef.Name &&
			// 	namespace == resourceRef.Namespace {

			// 	matches = append(matches, path)
			// 	// don't break; a file may have multiple matching docs
			// }

			objNs := obj.Metadata.Namespace
			if objNs == "" {
				objNs = "default"
			}
			refNs := resourceRef.Namespace
			if refNs == "" {
				refNs = "default"
			}

			if obj.Kind == resourceRef.Kind &&
				obj.Metadata.Name == resourceRef.Name &&
				objNs == refNs {
				matches = append(matches, path)
				// don't break; a file may have multiple matching docs
			}

		}
		return nil
	}

	if err := filepath.WalkDir(tmpDir, walkFn); err != nil {
		return "", nil, err
	}

	return tmpDir, matches, nil
}

// FindMatchingNADs walks a directory tree and returns all files containing
// NetworkAttachmentDefinition objects that match any of the provided nfconfigInterfaces.
func FindMatchingResource(ctx context.Context, baseDir string, checkpoint transitionv1.Checkpoint) ([]string, error) {
	log := logf.FromContext(ctx)
	var matchingNadFiles []string
	// log := logf.FromContext(ctx)
	walkFn := func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		dec := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var obj struct {
				APIVersion string `yaml:"apiVersion"`
				Kind       string `yaml:"kind"`
				Metadata   struct {
					Name      string `yaml:"name"`
					Namespace string `yaml:"namespace"`
				} `yaml:"metadata"`
			}

			err := dec.Decode(&obj)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}

			if strings.EqualFold(obj.Kind, "NetworkAttachmentDefinition") {

				// log.Info("file................" + obj.Metadata.Name + " --- " + obj.Metadata.Namespace)
				// log.Info("nfconfig................" + n.NameNad + " --- " + n.NamespaceNad)

				if obj.Metadata.Name == checkpoint.Spec.ResourceRef.Name &&
					obj.Metadata.Namespace == checkpoint.Spec.ResourceRef.Namespace {
					matchingNadFiles = append(matchingNadFiles, path)
					// continue decoding in case file contains multiple docs
					// break

					// Call the helper to update image immediately
					if err := UpdateResourceContainers(path, checkpoint.Status.LastCheckpointImage, checkpoint.Status.OriginalImage); err != nil {
						log.Error(err, "failed to update resource containers", "file", path)
						return err
					}
				}

			}
		}
		return nil
	}

	if err := filepath.WalkDir(baseDir, walkFn); err != nil {
		return nil, err
	}

	return matchingNadFiles, nil
}

func UpdateInterfaceIPsNFDeployment(path string, newIPs map[string]IPInfo) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var doc map[string]interface{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}

	spec, ok := doc["spec"].(map[string]interface{})
	if !ok {
		return nil
	}
	ifaces, ok := spec["interfaces"].([]interface{})
	if !ok {
		return nil
	}

	for _, iface := range ifaces {
		m := iface.(map[string]interface{})
		name := m["name"].(string)
		if info, ok := newIPs[name]; ok {
			if ipv4, ok := m["ipv4"].(map[string]interface{}); ok {
				if info.Address != "" {
					ipv4["address"] = info.Address
				}
				if info.Gateway != "" {
					ipv4["gateway"] = info.Gateway
				}
			}
		}
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0644)
}

// UpdateInterfaceIPsConfigRefs updates the IP addresses and gateways
// in a manifest of kind Config that contains nested NFDeployment spec.
func UpdateInterfaceIPsConfigRefs(path string, newIPs map[string]IPInfo) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var doc map[string]interface{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}

	// Drill down to spec.config.spec.interfaces
	spec, ok := doc["spec"].(map[string]interface{})
	if !ok {
		return nil
	}

	config, ok := spec["config"].(map[string]interface{})
	if !ok {
		return nil
	}

	nfSpec, ok := config["spec"].(map[string]interface{})
	if !ok {
		return nil
	}

	ifaces, ok := nfSpec["interfaces"].([]interface{})
	if !ok {
		return nil
	}

	for _, iface := range ifaces {
		ifaceMap, ok := iface.(map[string]interface{})
		if !ok {
			continue
		}
		name, ok := ifaceMap["name"].(string)
		if !ok {
			continue
		}
		if info, ok := newIPs[name]; ok {
			if ipv4, ok := ifaceMap["ipv4"].(map[string]interface{}); ok {
				if info.Address != "" {
					ipv4["address"] = info.Address
				}
				if info.Gateway != "" {
					ipv4["gateway"] = info.Gateway
				}
			}
		}
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}

	return os.WriteFile(path, out, 0644)
}

// UpdateResourceContainers updates matching container images
// inside a local Kubernetes manifest file.
// images: map[containerName]newImage
func UpdateResourceContainers(path string, imageCheckpoint, originalImage string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	var doc map[string]interface{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("unmarshal yaml: %w", err)
	}

	kind, _ := doc["kind"].(string)
	if !isSupportedKind(kind) {
		return fmt.Errorf("%s: unsupported Kind %q", path, kind)
	}

	spec, ok := doc["spec"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("%s: missing spec", path)
	}

	// All these workload types keep pod spec under spec.template.spec
	tpl, ok := spec["template"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("%s: missing spec.template", path)
	}
	resourceSpec, ok := tpl["spec"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("%s: missing spec.template.spec", path)
	}

	changed := updateContainers(resourceSpec, imageCheckpoint, originalImage)
	if !changed {
		return nil // nothing to do
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal yaml: %w", err)
	}

	if changed {
		fmt.Printf("Updated container image %s: %s -> %s\n", path, originalImage, imageCheckpoint)
	}

	return os.WriteFile(path, out, 0644)
}

func isSupportedKind(k string) bool {
	switch k {
	case "Deployment", "ReplicaSet", "DaemonSet", "StatefulSet":
		return true
	default:
		return false
	}
}

// updateContainers updates containers and initContainers by name.
func updateContainers(resourceSpec map[string]interface{}, imageCheckpoint, originalImage string) bool {
	changed := false
	for _, field := range []string{"containers", "initContainers"} {
		if arr, ok := resourceSpec[field].([]interface{}); ok {
			for _, c := range arr {
				if cm, ok := c.(map[string]interface{}); ok {
					if image, _ := cm["image"].(string); image != "" {
						if image == originalImage {
							cm["image"] = imageCheckpoint
							changed = true
						}
					}
				}
			}
		}
	}
	return changed
}

// CommitAndPush commits all staged changes in cloneDir and pushes to the remote branch.
// userName and password are Gitea credentials (from your secret).
func CommitAndPush(ctx context.Context, cloneDir, branch, repoURL, userName, password, commitMsg string) error {
	// userName, password, _, err := giteaclient.GetGiteaSecretUserNamePassword(ctx)

	// Open the repository
	r, err := git.PlainOpen(cloneDir)
	if err != nil {
		return fmt.Errorf("open repo: %w", err)
	}

	// Stage all changes
	w, err := r.Worktree()
	if err != nil {
		return fmt.Errorf("get worktree: %w", err)
	}
	if err := w.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return fmt.Errorf("git add: %w", err)
	}

	// Create commit
	_, err = w.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{
			Name:  userName,
			Email: fmt.Sprintf("%s@example.com", userName),
			When:  time.Now(),
		},
	})
	if err != nil {
		return fmt.Errorf("git commit: %w", err)
	}

	// Push commit
	if err := r.Push(&git.PushOptions{
		RemoteName: "origin",
		RefSpecs: []config.RefSpec{
			config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", branch, branch)),
		},
		Auth: &http.BasicAuth{
			Username: userName,
			Password: password,
		},
	}); err != nil {
		return fmt.Errorf("git push: %w", err)
	}

	return nil
}

// argocd sync | below is original
func TriggerArgoCDSyncWithKubeClient(k8sClient client.Client, appName, namespace string) error {
	ctx := context.TODO()

	// fmt.Printf("Triggering ArgoCD sync for application %q in namespace %q\n", appName, namespace)

	// Get the current Application object
	var app argov1alpha1.Application
	err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      appName,
		Namespace: namespace,
	}, &app)
	if err != nil {
		return fmt.Errorf("failed to get Argo CD application: %w", err)
	}

	// Deep copy to modify
	// updated := app.DeepCopy()
	// now := time.Now().UTC()

	// Update Operation field to trigger sync
	app.Operation = &argov1alpha1.Operation{
		Sync: &argov1alpha1.SyncOperation{
			Revision: "HEAD",
			SyncStrategy: &argov1alpha1.SyncStrategy{
				Apply: &argov1alpha1.SyncStrategyApply{
					Force: true, // This enables kubectl apply --force
				},
			},
		},
		InitiatedBy: argov1alpha1.OperationInitiator{
			Username: "gitea-client",
		},
		// StartedAt: &now,
	}

	//doing operations for this argocd application
	// fmt.Printf("Triggering sync for application %v", app)
	// fmt.Printf("Triggering sync for application %s in namespace %s\n", appName, namespace)

	if err := k8sClient.Update(ctx, &app); err != nil {
		return fmt.Errorf("failed to update application with sync operation: %w", err)
	}

	return nil
}

// listArgoApplications lists all Argo CD Applications in the argocd namespace
func ListArgoApplications(ctx context.Context, k8sClient client.Client) error {
	log := logf.FromContext(ctx)
	var appList argov1alpha1.ApplicationList
	if err := k8sClient.List(
		ctx,
		&appList,
		&client.ListOptions{Namespace: "argocd"},
	); err != nil {
		return err
	}

	for _, app := range appList.Items {
		log.Info("Found Argo CD Application",
			"name", app.Name,
			"project", app.Spec.Project,
			"destNamespace", app.Spec.Destination.Namespace,
			"destServer", app.Spec.Destination.Server,
		)
	}
	return nil
}

//create new argo app and push to the target cluster
