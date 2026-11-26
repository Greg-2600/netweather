# Deploy netweather on Azure with k3s

This directory contains a small Terraform module and Kubernetes manifests for a
two node k3s deployment. Terraform creates a resource group, a virtual network,
a subnet, a network security group, one public IP address, and two Linux VMs.
The first VM is the k3s server and the second VM is a k3s agent. Cloud init
installs k3s on both nodes and the server applies the manifests. There is no
Azure Kubernetes Service control plane and no managed Kubernetes service.

The module creates cloud resources. Read the plan before you apply it, and run
the teardown steps when you are done.

## Two node design

The default server size is Standard_B2ats_v2, an AMD size with 2 vCPUs and
1 GiB of RAM. The default agent size is Standard_B2pts_v2, an Arm size with
2 vCPUs and 1 GiB of RAM. Azure for Students covers 750 free hours for each of
these sizes, so running one server and one agent stays inside the free compute
allowance when the VMs share the month. The server is the only node with a
public IP address. The agent has no public IP address and joins the cluster over
the private subnet by connecting to the server at `https://10.240.1.4:6443`.

Terraform uses `10.240.0.0/16` for the virtual network and `10.240.1.0/24` for
the subnet. The node network must not overlap the k3s pod network
(`10.42.0.0/16`) or the k3s service network (`10.43.0.0/16`). If those ranges
overlap, the pod route on an agent can capture traffic for the server address,
and the agent will lose its connection to the API server. Keep `vnet_cidr`,
`subnet_cidr`, `k3s_cluster_cidr`, and `k3s_service_cidr` on separate ranges.

Both nodes use a 64 GiB Premium SSD, which is the P6 disk size. The P6 Premium
SSD allowance is included in the free disk allowance, so the disks are not a
paid item at this size. The default storage account type is Premium_LRS. Set
`os_disk_storage_account_type` to StandardSSD_LRS or Standard_LRS if your
subscription does not offer the Premium SSD allowance.

The only expected paid item is the Standard static IPv4 public IP address, at
about 3.60 USD per month. The module creates no NAT gateway, no load balancer,
and no other billable network resource. The subnet uses default outbound access
so the agent, which has no public IP, can still reach the internet to download
k3s and the container image.

## Cost note

This design should fit inside the 100 USD Azure for Students credit for 12
months under these assumptions:

- The Standard_B2ats_v2 server and the Standard_B2pts_v2 agent are covered by
  the student free compute allowance of 750 hours per size.
- Both root disks use the 64 GiB P6 Premium SSD size, which is covered by the
  free disk allowance.
- There is one Standard static public IP address at about 3.60 USD per month.
- There are no extra disks, snapshots, NAT gateways, load balancers, or large
  outbound transfers.

Check the Azure pricing page for your region before you apply. The auto shutdown
option stops the VMs on a daily schedule, which can stretch the credit further.

## Memory note

Each node has only 1 GiB of RAM. The default install runs the full 1000 target
fleet plus one Tor sidecar, which is tight on 1 GiB even with the 2 GB swap file
that cloud init creates at `/swapfile`. Both cloud init templates create the
swap file and set `vm.swappiness=10` before installing k3s. Expect some swapping
during a soak. If you need more headroom, raise `server_vm_size` or
`agent_vm_size`, or add agents with `agent_count`. The Terraform defaults stay on
the free student sizes for cost.

## Files

- `deploy/azure/terraform`: provider, resource group, virtual network, subnet,
  network security group, public IP address, k3s server and agent VMs, optional
  auto shutdown, outputs, and the server and agent cloud init templates.
- `deploy/k8s`: namespace, PersistentVolumeClaim, Deployment, Service, the
  Traefik Ingress, and the HelmChartConfig that enables the Let's Encrypt ACME
  resolver.
- `deploy/DOMAINS.md`: DNS pattern for one A record per app under
  `westcountytech.com`.
- `.github/workflows/azure_k3s.yml`: CI checks only. It runs Terraform format
  checks, `terraform init -backend=false`, `terraform validate`, and
  kubeconform. It never runs apply and uses no Azure credentials.

## Prerequisites

- Terraform 1.5 or newer.
- An Azure for Students subscription. Greg signs in with the Azure CLI himself,
  using `az login`, before he runs Terraform.
- An SSH key pair. The module takes the public key. Keep the private key on your
  machine and out of this repository.
- GHCR images built from this repository. The release workflow publishes the
  netweather and netweather-tor images. The netweather image is multi arch, so
  the same tag runs on both the x86_64 server and the Arm agent. The manifests
  have placeholder image names, and cloud init replaces them with the
  `netweather_image` value and a Tor image that uses the same tag.
- kubectl if you want to inspect the cluster from your machine.

## Deploy

1. Sign in with the Azure CLI. This step is yours to run.

   ```sh
   az login
   az account show
   ```

2. Enter the Terraform directory.

   ```sh
   cd deploy/azure/terraform
   ```

3. Create a local `terraform.tfvars`. This file is ignored by git. Replace the
   key and CIDR values with your own.

   ```hcl
   ssh_public_key   = "ssh-ed25519 AAAA... your-key-comment"
   ssh_allowed_cidr = "203.0.113.10/32"

   # Optional overrides.
   # location              = "northcentralus"
   # resource_group_name   = "netweather-rg"
   # server_vm_size        = "Standard_B2ats_v2"
   # agent_vm_size         = "Standard_B2pts_v2"
   # agent_count           = 1
   # app_allowed_cidr      = "203.0.113.0/24"
   # vnet_cidr             = "10.240.0.0/16"
   # subnet_cidr           = "10.240.1.0/24"
   # server_private_ip     = "10.240.1.4"
   # k3s_cluster_cidr      = "10.42.0.0/16"
   # k3s_service_cidr      = "10.43.0.0/16"
   # public_ip_sku         = "Standard"
   # root_disk_size_gb     = 64
   # swap_size_mb          = 2048
   # auto_shutdown_enabled = true
   # auto_shutdown_time    = "1900"
   # netweather_image      = "ghcr.io/greg-2600/netweather:1.0.1"
   # ingress_host          = "netweather.westcountytech.com"
   # acme_email            = "greg@westcountytech.com"
   ```

   If Azure rejects a Basic public IP for your subscription or region, keep
   `public_ip_sku` set to `Standard`. The Standard SKU is the default.

4. Initialize, check, and apply.

   ```sh
   terraform init
   terraform fmt -check
   terraform validate
   terraform plan -out netweather.tfplan
   terraform apply netweather.tfplan
   ```

5. Read the outputs. Cloud init needs a few minutes to install k3s and pull the
   image. The public IP is the target for the DNS records described in
   `deploy/DOMAINS.md`. The agent private IPs are listed in
   `agent_private_ips`.

   ```sh
   terraform output
   terraform output -raw public_ip
   terraform output -raw app_url
   terraform output -raw public_ip_url
   terraform output -json agent_private_ips
   ```

6. Check the cluster over SSH. Only the server has a public IP. Use it as a jump
   host if you want to reach an agent.

   ```sh
   ssh -i /path/to/private_key azureuser@"$(terraform output -raw public_ip)"
   sudo k3s kubectl get nodes -o wide
   sudo k3s kubectl -n netweather get pods
   sudo k3s kubectl -n netweather get ingress
   ```

7. Create the DNS records from `deploy/DOMAINS.md` if they do not exist, then
   open the `app_url` value in a browser. The network security group allows
   ports 80 and 443. It keeps SSH on port 22 and does not open port 6443 to the
   internet. Traffic inside the virtual network is allowed by the default
   AllowVnetInBound rule, so the agent can reach the server API on 6443. The
   deployment creates a random UI token in a Kubernetes Secret. Read it with
   this command on the server.

   ```sh
   sudo k3s kubectl -n netweather get secret netweather-auth \
     -o jsonpath='{.data.token}' | base64 -d
   ```

To apply the manifests by hand, use a local cluster context and these commands.
Replace both image placeholders with your GHCR images.

```sh
kubectl apply -f deploy/k8s/traefik-config.yaml
kubectl apply -f deploy/k8s/namespace.yaml
kubectl -n netweather create secret generic netweather-auth \
  --from-literal=token="$(openssl rand -hex 24)" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f deploy/k8s/pvc.yaml
sed -e 's|ghcr.io/OWNER/netweather:latest|ghcr.io/your-user/netweather:latest|' \
  -e 's|ghcr.io/OWNER/netweather-tor:latest|ghcr.io/your-user/netweather-tor:latest|' \
  deploy/k8s/deployment.yaml | kubectl apply -f -
kubectl apply -f deploy/k8s/service.yaml
kubectl apply -f deploy/k8s/ingress.yaml
```

The Ingress defaults to `netweather.westcountytech.com`. Change the `host`
and `tls` entries in `deploy/k8s/ingress.yaml` to use a different name.

To skip the Ingress, do not apply `deploy/k8s/ingress.yaml`. Use port forwarding
instead.

```sh
kubectl -n netweather port-forward svc/netweather 2600:80
```

## Tear down

Run destroy from the Terraform directory, using the same subscription and
variables that you used for apply.

```sh
cd deploy/azure/terraform
terraform destroy
```

Terraform removes the VMs, their disks, the public IP address, the network
security group, the subnet, the virtual network, and the resource group that it
created. The SSH key pair is not managed here, so delete it yourself if you no
longer need it. The GHCR image and any DNS records are also outside this module.

If you configured remote state, keep the storage account until after destroy
finishes. You can delete it and the state container when you no longer need the
state.
