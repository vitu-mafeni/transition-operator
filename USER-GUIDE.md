# CLUSTER MIGRATION GUIDE

When creating workload clusters keep a minimum of the following package variants

![image.png](image.png)

For azure cluster, the “example” word should be replaced with the name of the cluster, in this case “cluster1-azure”

![image.png](image%201.png)

Add in the environment variables matching the azure account you are using

For AWS cluster, just change the AWSCluster network matching your AWS resources, 

On the management cluster, initialize azure cluster providers, buildah and install helm

```bash
clusterctl init --infrastructure azure
snap install helm --classic
apt install buildah -y

mkdir /var/lib/kubelet/checkpoints
```

Use Flannel CNI on the clusters [all clusters were configured with 10.244.0.0/16 pod CIDR]

```bash
kubectl apply -f https://github.com/flannel-io/flannel/releases/latest/download/kube-flannel.yml --kubeconfig <kubeconfig-file>

```

For flannel to work in azure cluster, we need to install cloud provider controller -  change pod CIDR:

```bash
helm install --repo https://raw.githubusercontent.com/kubernetes-sigs/cloud-provider-azure/master/helm/repo cloud-provider-azure --generate-name --set infra.clusterName=${CLUSTER_NAME} --set cloudControllerManager.clusterCIDR="192.168.0.0/16" --kubeconfig <kubeconfig-file> 

```

ssh to all worker nodes and install CRIU and make sure runc version is IDENTICAL on both cluster’s nodes

```bash
curl -fsSL https://download.opensuse.org/repositories/devel:/tools:/criu/xUbuntu_22.04/Release.key | sudo gpg --dearmor -o /etc/apt/trusted.gpg.d/criu.gpg
echo "deb http://download.opensuse.org/repositories/devel:/tools:/criu/xUbuntu_22.04/ ./" | sudo tee /etc/apt/sources.list.d/criu.list
sudo apt-get update
sudo apt-get install criu -y

# upgrade runc
cd /tmp
curl -L -o runc.new https://github.com/opencontainers/runc/releases/download/v1.3.1/runc.amd64
sudo mv /tmp/runc.new /usr/local/sbin/runc
sudo chmod +x /usr/local/sbin/runc
```

Create *runc.conf*  file in */etc/criu*, if the folder doesnt exist, create it and add these contents to the file

```bash
file-locks
tcp-close
skip-in-flight
tcp-established
log-file /tmp/criu.log

enable-external-masters
external mnt[]
skip-mnt /proc/latency_stats

```

Check the version on both nodes and the output should be identical and similar to this:

```bash
# runc --version
output:
runc version 1.3.1
commit: v1.3.1-0-ge6457afc
spec: 1.2.1
go: go1.23.12
libseccomp: 2.5.6

```

Edit this kubelet file and add the feature gate options

```bash
sudo nano /etc/default/kubelet
--feature-gates=ContainerCheckpoint=true
```

restart kubelet and containerd

```bash
systemctl restart kubelet containerd
```

**Note:** Ensure the pod's storage and network configuration match the original for successful restoration.

The operator is divided into 2; the controllers with run on the management cluster and checkpoint agent which runs on the workload cluster. 

To ssh to the worker node using VSCode, using the following ssh_config. Before doing it, copy the while ssh from the control node to the worker node, copy the ssh files from the capi user to the root user

```bash
cp /home/capi/.ssh . -r
```

Enable root ssh login by editing the /etc/ssh/sshd_config file and changing value of PermitRootLogin to yes

restart the ssh service

```bash
service sshd restart
```

The config in vscode:

```
Host azure-bastion-box
    HostName 20.214.25.226
    User capi
    IdentityFile "C:\Users\Vt\Downloads\azure-vm-test_key.pem"

# Target machine with private IP address
Host azure-target-box
    HostName 10.1.0.4
    User root
    IdentityFile "C:\Users\Vt\Downloads\azure-vm-test_key.pem"
    ProxyCommand ssh -q -W %h:%p azure-bastion-box
```

### ON THE MANAGEMENT CLUSTER

create a gitea secret with your gitea credentials 

```bash
kubectl create secret generic git-user-secret   --from-literal=username=nephio   --from-literal=password=secret   -n default
```

Install go lang following the official guide: https://go.dev/doc/install

Clone the operator to any directory, i am using /home/ubuntu/projects folder

```bash
mkdir projects; cd projects;git clone https://github.com/vitu-mafeni/transition-operator.git
```

Create environment variables matching gitea server, container registry, node heartbeat fault delay and minio server

Minio port can be found by the following command; get the port on minio service. minio-console is the web-ui for minio. 

```bash
kubectl get svc -n minio-system
```

and the default username and password for minio web console is “nephio1234” and password is “secret1234”

```bash
export  MINIO_ENDPOINT="47.129.115.173:31092"
export  MINIO_ACCESS_KEY="nephio1234"
export  MINIO_SECRET_KEY="secret1234"
export  MINIO_BUCKET="checkpoints"

export REPOSITORY=vitu1
export SECRET_NAME_REF=reg-credentials
export SECRET_NAMESPACE_REF=default
export REGISTRY_URL="docker.io"

export HEARTBEAT_FAULT_DELAY=20

export GIT_SERVER_URL="http://47.129.115.173:31413"
export GIT_SECRET_NAME="git-user-secret"
export GIT_SECRET_NAMESPACE="default"
export POD_NAMESPACE="default"
export REGISTRY_PASSWORD="PUT_PASSWORD_HERE"

```

Create registry credentials

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

Apply this Checkpoint service account 

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

kubectl apply -f /tmp/checkpoint-sa.yaml --kubeconfig <source-cluster>
```

Go into the controller cloned and run the following commands

```bash
make generate
make manifests
make install
make run
```

In the operator, there is the clusterpolicy resource. The configurations should be changed, matching your environment

```yaml
apiVersion: transition.dcnlab.ssu.ac.kr/v1
kind: ClusterPolicy
metadata:
  name: clusterpolicy-sample
spec:
  clusterSelector:
    name: cluster1-azure
    repo: http://13.212.242.157:31410/nephio/cluster1-azure.git # repo where the cluster workloads is defined
    repoType: git
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

```

## Workload cluster: source worker node

This will run on the node where our workload will run

clone this repository

```bash
git clone https://github.com/vitu-mafeni/checkpoint-agent.git
cd checkpoint-agent/agent-og
```

Make sure these environment variables are set

```bash
# MinIO client and checkpoint settings
export CHECKPOINT_DIR=/var/lib/kubelet/checkpoints
export MINIO_ENDPOINT=47.129.115.173:31092
export MINIO_ACCESS_KEY=nephio1234
export MINIO_SECRET_KEY=secret1234
export MINIO_BUCKET=checkpoints
export PULL_INTERVAL=5s

# fault detection client settings
export CONTROLLER_URL=http://47.129.115.173:8090/heartbeat
export FAULT_DETECTION_INTERVAL=10s
```

Once the transition-operator starts running in the management cluster, run the checkpoint-agent

```bash
go run main.go
```

To access the video use port-forwarding for AWS and azure

```bash
kubectl port-forward svc/video-service --address 0.0.0.0 30080:8080 --kubeconfig aws.kubeconfig
```

Adding the video application to the catalog

![image.png](image%202.png)

The controller uses the annotations as shown below to find the packages to transition

![image.png](image%203.png)
