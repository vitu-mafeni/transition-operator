# Cluster Migration Guide

A step-by-step guide to migrate stateful and stateless workloads between clusters using container checkpointing, a management-plane operator, and a node-level checkpoint agent.

---

## Overview

- Management plane runs controllers on the management cluster.
- Workload plane runs the checkpoint-agent on source worker nodes.
- Storage and network configuration must match between source and target for successful restoration.

> Important: Source and destination clusters must run identical versions of kubelet, containerd, CRIU, and runc.
> 

---

## Prerequisites

- Kubernetes >= 1.30 on all clusters
- containerd >= 2.1.4 on all nodes
- CRIU >= 4.1.1 on worker nodes
- runc version aligned across all nodes (exact same version)
- Go >= 1.23 on all relevant hosts
- Helm installed on the management cluster
- clusterctl available on the management cluster
- Access to container registry and credentials
- Access to a Git server for package sources (for example, Gitea)
- Access to a MinIO or S3-compatible object store for checkpoints

---

## Package variants to keep when creating workload clusters

![image.png](images/image.png)


---

## Cluster naming and environment

- Azure clusters: Replace occurrences of the word "example" with the actual cluster name, for example, cluster1-azure.

![image.png](images/image%201.png)


- Set environment variables to match the cloud account you are using (Azure or AWS). Keep provider-specific CIDRs and network settings consistent with your cloud resources.

---

## Management Cluster Setup

### Initialize providers and tooling

```bash
clusterctl init --infrastructure azure
snap install helm --classic
apt install -y buildah
mkdir -p /var/lib/kubelet/checkpoints # where the controller will access checkpoints if needed
```

### Use Flannel CNI

All clusters here are configured with Pod CIDR 10.244.0.0/16.

```bash
kubectl apply -f https://github.com/flannel-io/flannel/releases/latest/download/kube-flannel.yml --kubeconfig <kubeconfig-file>
```

- Note: By default, CAPI resources often use Pod CIDR 192.168.0.0/16. Ensure this matches your Flannel config.

### Azure cloud controller for Flannel

For Azure, install the cloud provider controller and set a matching clusterCIDR. Adjust if your actual pod CIDR differs.

```bash
helm install --repo https://raw.githubusercontent.com/kubernetes-sigs/cloud-provider-azure/master/helm/repo cloud-provider-azure --generate-name --set infra.clusterName=${CLUSTER_NAME} --set cloudControllerManager.clusterCIDR="192.168.0.0/16" --kubeconfig <kubeconfig-file> 

```

---

## Prepare Worker Nodes (source and destination)

### Install CRIU and align runc

```bash
curl -fsSL https://download.opensuse.org/repositories/devel:/tools:/criu/xUbuntu_22.04/Release.key | gpg --dearmor -o /etc/apt/trusted.gpg.d/criu.gpg
echo "deb http://download.opensuse.org/repositories/devel:/tools:/criu/xUbuntu_22.04/ ./" | tee /etc/apt/sources.list.d/criu.list
apt-get update
apt-get install criu -y


# upgrade runc
cd /tmp
curl -L -o runc.new https://github.com/opencontainers/runc/releases/download/v1.3.1/runc.amd64
mv /tmp/runc.new /usr/local/sbin/runc
chmod +x /usr/local/sbin/runc
```

### Create /etc/criu/runc.conf

```bash
# /etc/criu/runc.conf
file-locks
tcp-close
skip-in-flight
tcp-established
log-file /tmp/criu.log
enable-external-masters
external mnt[]
skip-mnt /proc/latency_stats
```

### Verify runc

Output should be identical across nodes:

```bash
runc --version
# Example output:
# runc version 1.3.1
# commit: v1.3.1-0-ge6457afc
# spec: 1.2.1
# go: go1.23.12
# libseccomp: 2.5.6
```

### Enable kubelet feature gate (checkpointing)

```bash
nano /etc/default/kubelet
# Append or merge the following flag (or use your distro's kubelet drop-in)
KUBELET_EXTRA_ARGS="--feature-gates=ContainerCheckpoint=true"
```

Restart kubelet and containerd:

```bash
systemctl restart kubelet containerd
```

---

## SSH and Access (optional helpers)

To copy SSH keys from capi to root:

```bash
cp -r /home/capi/.ssh /root/
```

Enable root SSH in /etc/ssh/sshd_config (PermitRootLogin yes), then:

```bash
systemctl restart ssh || service sshd restart
```

VS Code SSH config example:

```
Host azure-bastion-box
    HostName 20.214.25.226
    User capi
    IdentityFile "C:\\Users\\Vt\\Downloads\\azure-vm-test_key.pem"

Host azure-target-box
    HostName 10.1.0.4
    User root
    IdentityFile "C:\\Users\\Vt\\Downloads\\azure-vm-test_key.pem"
    ProxyCommand ssh -q -W %h:%p azure-bastion-box
```

---

## Cloud Setup on Management Cluster (Azure example)

Install Azure CLI:

```bash
curl -sL https://aka.ms/InstallAzureCLIDeb | bash
```

Login:

```bash
az login
```

Create resource group:

```bash
az group create --location koreasouth --resource-group capi-test
```

Create user-assigned identity:

```bash
az identity create \
  --name cloud-provider-user-identity \
  --resource-group capi-test
```

---

## Secrets and Credentials

### Gitea credentials secret

```bash
kubectl create secret generic git-user-secret \
  --from-literal=username=nephio \
  --from-literal=password=secret \
  -n default
```

### Go installation

Follow the official guide: [https://go.dev/doc/install](https://go.dev/doc/install)

### Clone the operator

```bash
mkdir -p ~/projects && cd ~/projects
git clone https://github.com/vitu-mafeni/transition-operator.git
```

### Environment variables

MinIO console defaults: username "nephio1234" and password "secret1234".

```bash
# Discover MinIO ports
kubectl get svc -n minio-system

# Example environment variables
export MINIO_ENDPOINT="47.129.115.173:31092"
export MINIO_ACCESS_KEY="nephio1234"
export MINIO_SECRET_KEY="secret1234"
export MINIO_BUCKET="checkpoints"

export REPOSITORY="vitu1"
export SECRET_NAME_REF="reg-credentials"
export SECRET_NAMESPACE_REF="default"
export REGISTRY_URL="http://docker.io"
export HEARTBEAT_FAULT_DELAY=20

export GIT_SERVER_URL="http://47.129.115.173:31413"
export GIT_SECRET_NAME="git-user-secret"
export GIT_SECRET_NAMESPACE="default"
export POD_NAMESPACE="default"
export REGISTRY_PASSWORD="PUT_PASSWORD_HERE" # Use a Docker Hub access token
```

### Create registry credentials secret

```bash
 # Base64 ecredentials
 username_b64=$(echo -n "$REPOSITORY" | base64 -w 0) 
 password_b64=$(echo -n "$REGISTRY_PASSWORD" | base64 -w 0)
 registry_b64=$(echo -n "$REGISTRY_URL" | base64 -w 0) # docker.io
    
# Create secret manifest
  
cat > /tmp/registry-credentials.yaml <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: reg-credentials
type: Opaque
data:
  username: $username_b64
  password: $password_b64
  registry: $registry_b64
EOF

kubectl apply -f /tmp/registry-credentials.yaml
```

- Note: Use a Docker Hub access token rather than your account password.

### Service account for checkpoint operations (source cluster)

- source-cluster is the cluster where we will transition from

```bash
cat > /tmp/checkpoint-sa.yaml <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: checkpoint-sa
  namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: checkpoint-role
  namespace: default
rules:
  - apiGroups: [""]
    resources: ["pods/checkpoint"]
    verbs: ["patch", "create", "update", "proxy"]
  - apiGroups: [""]
    resources: ["serviceaccounts/token"]
    verbs: ["create"]
  - apiGroups: [""]
    resources: ["nodes/proxy"]
    verbs: ["create", "get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: checkpoint-rolebinding
  namespace: default
subjects:
  - kind: ServiceAccount
    name: checkpoint-sa
    namespace: default
roleRef:
  kind: Role
  name: checkpoint-role
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: checkpoint-sa-nodes-checkpoint
rules:
  - apiGroups: [""]
    resources: ["nodes/checkpoint"]
    verbs: ["create"]
  - apiGroups: [""]
    resources: ["serviceaccounts/token"]
    verbs: ["create"]
  - apiGroups: [""]
    resources: ["nodes/proxy"]
    verbs: ["create", "get"]


---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: checkpoint-sa-nodes-checkpoint-binding
subjects:
  - kind: ServiceAccount
    name: checkpoint-sa
    namespace: default
roleRef:
  kind: ClusterRole
  name: checkpoint-sa-nodes-checkpoint
  apiGroup: rbac.authorization.k8s.io
EOF

# Apply to the source cluster from which you will transition
kubectl apply -f /tmp/checkpoint-sa.yaml --kubeconfig <source-cluster>
```

---

## Build and Run the Controller (management cluster)

```bash
cd ~/projects/<controller-repo>
make generate
make manifests
make install
make run
```

### ClusterPolicy configuration

Update values to match your environment, then apply to the management cluster.

```bash
cat > /tmp/cluster-policy.yaml <<'EOF'
apiVersion: transition.dcnlab.ssu.ac.kr/v1
kind: ClusterPolicy
metadata:
  name: clusterpolicy-sample
spec:
  clusterSelector:
    name: cluster1-azure
    repo: http://13.212.242.157:31410/nephio/cluster1-azure.git # repo where the cluster workloads is defined
    repoType: git
  selectMode: Specific # Specific, All
  packageSelectors:
    - name: video # Package Name
      packagePath: video # where the package is in the repo
      packageType: Stateful # Stateless, Stateful
      liveStatePackage: true # tells the operator to use CRIU
      backupInformation:
        - name: my-test-backup # Schedule Name
          backupType: Schedule # Manual, Schedule
          schedulePeriod: "*/3 * * * *" # cron format
  targetClusterPolicy:
    preferClusters:
      - name: cluster2-aws
        repoType: git
        weight: 100

EOF

kubectl apply -f /tmp/cluster-policy.yaml
```

---

## Workload Cluster: Source Worker Node

Clone the checkpoint agent and run it on the node that hosts the workload to be transitioned.

```bash
git clone https://github.com/vitu-mafeni/checkpoint-agent.git
cd checkpoint-agent/agent-og
```

Set environment variables:

```bash
# MinIO client and checkpoint settings
export CHECKPOINT_DIR=/var/lib/kubelet/checkpoints
export MINIO_ENDPOINT=47.129.115.173:31092
export MINIO_ACCESS_KEY=nephio1234
export MINIO_SECRET_KEY=secret1234
export MINIO_BUCKET=checkpoints
export PULL_INTERVAL=5s

export AWS_REGION="ap-southeast-1" #only for AWS

# Fault detection client settings
export CONTROLLER_URL=http://47.129.115.173:8090/heartbeat
export FAULT_DETECTION_INTERVAL=10s
```

Run the agent after the transition-operator is running:

```bash
go run main.go
```

---

## Accessing the Sample Video Application

Port-forward for AWS or Azure clusters:

```bash
kubectl port-forward svc/video-service --address 0.0.0.0 30080:8080 --kubeconfig aws.kubeconfig
```

Adding the video application to the catalog

![image.png](images/image%202.png)


Controller annotations used to discover packages

![image.png](images/image%203.png)

---
## Other resources


---

## Notes and Best Practices

- All commands are installed and run with root privileges.
- Run the operator and node agent with root privileges.
- Keep software versions identical between source and destination nodes.
- Validate Pod CIDR consistency between CAPI resources and your CNI.

---

## Troubleshooting

- Flannel not routing pods across nodes: Verify Pod CIDR alignment and ensure cloud-controller integration on Azure.
- Checkpoint restore fails: Confirm CRIU and runc compatibility. Review /tmp/criu.log on the node.
- Agent cannot reach MinIO: Verify MINIO_ENDPOINT and network policy. Confirm bucket exists and credentials are correct.
- Controller cannot access Git or Registry: Validate secrets, network egress, and DNS.
- Feature gate ignored: Ensure kubelet flag is placed in the right location for your distro and that systemd drop-ins aren’t overriding it.