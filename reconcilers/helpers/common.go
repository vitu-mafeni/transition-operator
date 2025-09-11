package helpers

import (
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func GetWorkloadOwnerControllerInfo(pod corev1.Pod) (kind string, name string, ok bool) {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return ref.Kind, ref.Name, true
		}
	}
	return "", "", false
}

func GetWorkloadControllerOwnerName(refs []metav1.OwnerReference, kind string) (string, bool) {
	for _, ref := range refs {
		if ref.Kind == kind && ref.Controller != nil && *ref.Controller {
			return ref.Name, true
		}
	}
	return "", false
}

func GetEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func GetEnvDuration(key string, defaultVal time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultVal
}
