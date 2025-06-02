package helpers

import (
	"context"
	"fmt"
	"time"

	gitea "code.gitea.io/sdk/gitea"
	"github.com/go-logr/logr"
	transitionv1 "github.com/vitu1234/transition-operator/api/v1"
	"gopkg.in/yaml.v2"
)

type ArgoAppSpec struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name       string   `yaml:"name"`
		Namespace  string   `yaml:"namespace"`
		Finalizers []string `yaml:"finalizers"`
	} `yaml:"metadata"`
	Spec struct {
		Project string `yaml:"project"`
		Source  struct {
			RepoURL        string `yaml:"repoURL"`
			TargetRevision string `yaml:"targetRevision"`
			Path           string `yaml:"path"`
			Directory      struct {
				Recurse bool `yaml:"recurse"`
			} `yaml:"directory"`
		} `yaml:"source"`
		Destination struct {
			Server    string `yaml:"server"`
			Namespace string `yaml:"namespace"`
		} `yaml:"destination"`
		SyncPolicy struct {
			Automated struct {
				Prune    bool `yaml:"prune"`
				SelfHeal bool `yaml:"selfHeal"`
			} `yaml:"automated"`
			AllowEmpty  bool     `yaml:"allowEmpty"`
			SyncOptions []string `yaml:"syncOptions"`
			IgnoreDiffs []struct {
				Group string `yaml:"group"`
				Kind  string `yaml:"kind"`
			} `yaml:"ignoreDifferences"`
		} `yaml:"syncPolicy"`
	} `yaml:"spec"`
}

func LogRepositories(log logr.Logger, repos []*gitea.Repository) {
	for _, repo := range repos {
		log.Info("Found repository", "name", repo.Name, "full_name", repo.FullName, "url", repo.CloneURL)
	}
}

func DetermineTargetRepo(policy *transitionv1.ClusterPolicy, log logr.Logger) (string, bool) {
	highestWeight := -1
	var repo string

	for _, target := range policy.Spec.TargetClusterPolicy.PreferClusters {
		if target.RepoType != "git" {
			log.Info("Skipping non-git repository", "cluster", target.Name)
			continue
		}
		if target.Weight > highestWeight {
			highestWeight = target.Weight
			repo = target.Name + "-dr"
		}
	}

	return repo, repo != ""
}

func CreateAndPushArgoApp(
	ctx context.Context,
	client *gitea.Client,
	username, repoName, folder string,
	clusterPolicy *transitionv1.ClusterPolicy,
	transitionPackage transitionv1.PackageSelector,
	log logr.Logger,
) error {
	app := ArgoAppSpec{
		APIVersion: "argoproj.io/v1alpha1",
		Kind:       "Application",
	}
	app.Metadata.Name = fmt.Sprintf("argocd-%s-%s", clusterPolicy.Spec.ClusterSelector.Name, transitionPackage.Name)
	app.Metadata.Namespace = "argocd"
	app.Metadata.Finalizers = []string{"resources-finalizer.argocd.argoproj.io"}
	app.Spec.Project = "default"
	app.Spec.Source.RepoURL = clusterPolicy.Spec.ClusterSelector.Repo
	app.Spec.Source.TargetRevision = "HEAD"
	app.Spec.Source.Path = transitionPackage.PackagePath
	app.Spec.Source.Directory.Recurse = true
	app.Spec.Destination.Server = "https://kubernetes.default.svc"
	app.Spec.Destination.Namespace = "default"
	app.Spec.SyncPolicy.Automated.Prune = true
	app.Spec.SyncPolicy.Automated.SelfHeal = true
	app.Spec.SyncPolicy.AllowEmpty = true
	app.Spec.SyncPolicy.SyncOptions = []string{"CreateNamespace=true"}
	app.Spec.SyncPolicy.IgnoreDiffs = []struct {
		Group string `yaml:"group"`
		Kind  string `yaml:"kind"`
	}{
		{Group: "fn.kpt.dev", Kind: "ApplyReplacements"},
		{Group: "fn.kpt.dev", Kind: "StarlarkRun"},
	}

	yamlData, err := yaml.Marshal(app)
	if err != nil {
		return fmt.Errorf("failed to marshal Argo application YAML: %w", err)
	}

	timestamp := time.Now().Format("20060102-150405")
	filename := fmt.Sprintf("%s/argo-app-%s.yaml", folder, timestamp)
	message := fmt.Sprintf("ArgoCD app: %s-%s", clusterPolicy.Spec.ClusterSelector.Name, transitionPackage.Name)

	fileOpts := gitea.CreateFileOptions{
		Content: string(yamlData),
		FileOptions: gitea.FileOptions{
			Message:    message,
			BranchName: "main",
		},
	}

	_, _, err = client.CreateFile(username, repoName, filename, fileOpts)
	if err != nil {
		return fmt.Errorf("failed to create file in Gitea: %w", err)
	}

	log.Info("Successfully pushed Argo application", "repo", repoName, "file", filename)
	return nil
}
