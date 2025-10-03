package heartbeat

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	transitionv1 "github.com/vitu1234/transition-operator/api/v1"
	"github.com/vitu1234/transition-operator/internal/controller"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	capiv1beta1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Payload sent by agents
type HeartbeatPayload struct {
	NodeName   string            `json:"node"`
	IPAddress  string            `json:"ip"`
	OS         string            `json:"os"`
	Arch       string            `json:"arch"`
	CPUCount   int               `json:"cpu"`
	MemoryMB   uint64            `json:"memory_mb"`
	Extra      map[string]string `json:"extra,omitempty"`
	ReceivedAt time.Time         `json:"received_at"`

	// From node annotations
	ClusterName string `json:"cluster_name,omitempty"`
	ClusterNS   string `json:"cluster_namespace,omitempty"`
	Machine     string `json:"machine,omitempty"`
	OwnerKind   string `json:"owner_kind,omitempty"`
	OwnerName   string `json:"owner_name,omitempty"`
	ProvidedIP  string `json:"provided_node_ip,omitempty"`
	CRISocket   string `json:"cri_socket,omitempty"`
}

// In-memory state store
type HeartbeatStore struct {
	sync.RWMutex
	data map[string]HeartbeatPayload
}

func NewHeartbeatStore() *HeartbeatStore {
	return &HeartbeatStore{data: make(map[string]HeartbeatPayload)}
}

func (s *HeartbeatStore) Update(hb HeartbeatPayload) {
	s.Lock()
	defer s.Unlock()
	hb.ReceivedAt = time.Now()
	s.data[hb.NodeName] = hb
}

func (s *HeartbeatStore) Get(node string) (HeartbeatPayload, bool) {
	s.RLock()
	defer s.RUnlock()
	hb, ok := s.data[node]
	return hb, ok
}

func (s *HeartbeatStore) All() []HeartbeatPayload {
	s.RLock()
	defer s.RUnlock()
	res := []HeartbeatPayload{}
	for _, hb := range s.data {
		res = append(res, hb)
	}
	return res
}

// Node fault detection threshold
const NodeTimeout = 20 * time.Second // adjust as needed

// RunHeartbeatServer starts HTTP server inside your controller
func RunHeartbeatServer(store *HeartbeatStore, addr string, k8sClient ctrl.Client, namespace string) {
	mux := http.NewServeMux()
	log := logf.FromContext(context.Background())

	// endpoint to receive heartbeats
	mux.HandleFunc("/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		defer r.Body.Close()

		var hb HeartbeatPayload
		if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// update store
		store.Update(hb)
		log.Info("heartbeat received",
			"nodeName", hb.NodeName,
			"ip", hb.IPAddress,
		)

		// upsert NodeHealth CR as Healthy
		if err := UpsertNodeHealth(r.Context(), k8sClient, namespace, hb, "Healthy"); err != nil {
			log.Error(err, "failed to upsert NodeHealth",
				"nodeName", hb.NodeName,
				"clusterName", hb.ClusterName,
			)
		}

		w.WriteHeader(http.StatusNoContent)
	})

	// optional: endpoint to list all nodes
	mux.HandleFunc("/nodes", func(w http.ResponseWriter, r *http.Request) {
		nodes := store.All()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(nodes)
	})

	go func() {
		log.Info("heartbeat server listening",
			"addr", addr,
		)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Error(err, "heartbeat server error")
		}
	}()
}

// UpsertNodeHealth creates or updates a NodeHealth CR
func UpsertNodeHealth(ctx context.Context, c ctrl.Client, namespace string, hb HeartbeatPayload, condition string) error {
	name := hb.NodeName
	nh := &transitionv1.NodeHealth{}

	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, nh)
	if err != nil {
		// create new
		nh = &transitionv1.NodeHealth{
			TypeMeta: metav1.TypeMeta{
				Kind:       "NodeHealth",
				APIVersion: "nephio.dev/v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      name,
			},
			Spec: transitionv1.NodeHealthSpec{
				NodeName: hb.NodeName,
			},
			Status: transitionv1.NodeHealthStatus{
				IP:        hb.IPAddress,
				OS:        hb.OS,
				Arch:      hb.Arch,
				CPUs:      hb.CPUCount,
				MemTotal:  hb.MemoryMB,
				LastSeen:  metav1.NewTime(hb.ReceivedAt),
				Condition: condition,
			},
		}
		return c.Create(ctx, nh)
	}

	// update existing
	nh.Status.IP = hb.IPAddress
	nh.Status.OS = hb.OS
	nh.Status.Arch = hb.Arch
	nh.Status.CPUs = hb.CPUCount
	nh.Status.MemTotal = hb.MemoryMB
	nh.Status.LastSeen = metav1.NewTime(hb.ReceivedAt)
	nh.Status.Condition = condition

	// update status
	if err := c.Status().Update(ctx, nh); err != nil {
		// fallback to full update
		return c.Update(ctx, nh)
	}

	return nil
}

// MonitorNodes periodically checks heartbeat timestamps to detect faults
func MonitorNodes(store *HeartbeatStore, k8sClient ctrl.Client, namespace string, clusterPolicyReconciler *controller.ClusterPolicyReconciler) {
	ctx := context.Background()
	log := logf.FromContext(ctx)

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		nodes := store.All()
		for _, hb := range nodes {
			if now.Sub(hb.ReceivedAt) > NodeTimeout {
				log.Info("Triggering ClusterPolicy reconciliation due to unhealthy node",
					"nodeName", hb.NodeName,
					"clusterName", hb.ClusterName,
				)

				// get all machines
				machine := &capiv1beta1.Machine{}
				err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: hb.NodeName}, machine)
				if err != nil {
					log.Error(err, "could not get capi machine with the name "+hb.NodeName)
				}

				clusterName := ""
				if machine != nil {
					clusterName = machine.Spec.ClusterName
				}

				// Trigger ClusterPolicy reconciliation to handle node failure
				if err := clusterPolicyReconciler.ReconcileClusterPoliciesForNode(
					context.Background(), hb.NodeName, clusterName); err != nil {
					log.Error(err, "failed to trigger ClusterPolicy reconciliation",
						"nodeName", hb.NodeName,
						"clusterName", clusterName,
					)
				}

				if err := UpsertNodeHealth(context.Background(), k8sClient, namespace, hb, "Unhealthy"); err != nil {
					log.Error(err, "failed to update NodeHealth",
						"nodeName", hb.NodeName,
						"clusterName", hb.ClusterName,
					)
				}
			}
		}
	}
}

// StartServer starts heartbeat server and fault monitoring
func StartServer(k8sClient ctrl.Client, namespace string, addr string, clusterPolicyReconciler *controller.ClusterPolicyReconciler) {
	store := NewHeartbeatStore()
	RunHeartbeatServer(store, addr, k8sClient, namespace)
	go MonitorNodes(store, k8sClient, namespace, clusterPolicyReconciler)
}
