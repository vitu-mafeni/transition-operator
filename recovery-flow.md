
## 🔁 Kubernetes Recovery Operator – Logic Flow

```text
[ Start recovery loop ]
|
|-- Check CAPI Cluster: Ready?
|    |-- No: 
|         |-- Check InfraCluster readiness
|         |-- Check KubeadmControlPlane status
|         |-- Exit and requeue if not ready
|    |-- Yes: Continue
|
|-- List Machines (cluster.x-k8s.io/cluster-name = <cluster>)
|    |-- For each machine:
|        |-- Status != Running OR FailureReason set?
|            |-- Yes:
|                |-- If control plane and HA not met -> Skip
|                |-- Else -> Delete to trigger replacement
|
|-- List Nodes (corev1.Node)
|    |-- For each node:
|        |-- NotReady for more than N minutes?
|            |-- Yes: Cordon + Delete
|        |-- Has memory/disk/network pressure?
|            |-- Yes: Consider draining or re-allocating pods
|
|-- List Pods (all namespaces)
|    |-- For each pod:
|        |-- Status = Failed OR CrashLoopBackOff?
|            |-- Trace to owning controller:
|                |-- DaemonSet? Skip
|                |-- Deployment/ReplicaSet/StatefulSet?
|                    |-- If crash duration > threshold:
|                        |-- Delete pod for recreation
|                |-- No controller? Alert operator
|
|-- List Workloads (Deployments / StatefulSets / DaemonSets)
|    |-- For each workload:
|        |-- readyReplicas < replicas?
|            |-- Check rollout strategy / conditions
|            |-- If stalled > rollout timeout:
|                |-- Annotate or restart rollout
|
|-- Validate Critical CRDs present?
|    |-- No: Alert / reapply CRDs or reinstall operator
|
[ Repeat loop every N seconds based on ClusterPolicy ]
```
<!-- --- -->
<!-- [ Start recovery loop ]
|
|-- Check CAPI Cluster: Ready?
|    |-- No: Check if stuck due to infra or control plane
|    |-- Yes: Continue
|
|-- List Machines -> Check status
|    |-- Not running or failed -> delete to trigger re-creation
|
|-- List Nodes -> Check readiness and pressure conditions
|    |-- NotReady > N minutes -> cordon + delete
|
|-- List Pods (all namespaces)
|    |-- CrashLoopBackOff / Failed -> log and trace to parent
|        |-- If caused by bad node, mark node for recovery
|        |-- If caused by config, alert operator
|
|-- List Deployments/DaemonSets/StatefulSets
|    |-- .readyReplicas < .replicas -> investigate
|
[ Repeat ] -->


---
## A. Infra & Control-Plane Sanity
  ### A1. InfraCluster Ready?
  infra := fetch InfraCluster for cluster
  if err or infra.Status.Ready != True:
    log error or “Infra not ready”
    annotate ClusterPolicy: “InfraUnready”
    return

  ### A2. ControlPlane Ready?
  cp := fetch KubeadmControlPlane (or equivalent)
  if cp.Status.ReadyReplicas < cp.Spec.Replicas OR
     cp.HasCondition(“Initialized” or “Ready”, != True):
    log “Control plane not healthy”
    if age(cp) > policy.ControlPlaneTimeout:
      triggerControlPlaneRecovery(cp)
    return
## B. Machine Recovery
  machines ← list Machines with label cluster-name=cluster.Name
  for each m in machines:
    if m.Status.Phase != “Running” OR
       m.Status.FailureReason != nil:
      if isControlPlaneMachine(m) AND machines.ControlPlaneRunning < 2:
        # avoid single-CP outage
        log “Defer CP machine deletion until HA”
      else:
        delete m  # triggers MachineSet/MachineDeployment to recreate
        emit Metric “machine_deleted_total”
## C. Node Recovery
  nodes ← list corev1.Node in all namespaces
  for each n in nodes:
    if !n.IsReady():
      if timeSince(n.LastTransition) > policy.NodeNotReadyThreshold:
        if hasBackingMachine(n):
          cordon n
          delete n
          emit Metric “node_replaced_total”
        else:
          log “Node without Machine—manual intervention”
## D. Pod-Level Recovery
  pods ← list corev1.Pod in all namespaces
  for each p in pods:
    if p.Status.Phase == “Failed” OR
       hasContainerState(p, Waiting, Reason=CrashLoopBackOff):
      owner := resolveController(p.OwnerReferences)
      if owner is DaemonSet:
        # DS pods auto-heal; just log
        log “DaemonSet pod crash” 
      else if owner is ReplicaSet/Deployment:
        # usually auto-healed; but…
        if timeSince(p.FirstObservedFailure) > policy.PodCrashThreshold:
          recordEvent(p, “PersistentCrashLoop”, “Deleting pod to force recreate”)
          delete p
      else:
        log “Pod unmanaged or unknown owner—alert”
## E. Workload-Resource Health
  for each workload in {Deployment, StatefulSet, DaemonSet}:
    wsList ← list workload in all namespaces
    for each ws in wsList:
      if ws.Status.ReadyReplicas < ws.Spec.Replicas:
        if timeSince(ws.Status.LastUpdateTime) > policy.WorkloadRolloutTimeout:
          annotate ws: “recovery.nephio.io/restarted=true”
          delete pods of ws  # force rollout retry
          emit Metric “workload_rollout_restarted_total”
## F. Loop & Backoff
  ## At end of each full pass:
  if any recovery actions performed:
    requeue after policy.ShortInterval  # e.g. 30s
  else:
    requeue after policy.LongInterval   # e.g. 5m
