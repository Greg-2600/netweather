# Deploy netweather on Azure with k3s

This directory contains a small Terraform module and Kubernetes manifests for a
single VM deployment. Terraform creates a resource group, a virtual network, a
subnet, a network security group, a public IP address, and one Linux VM. Cloud
init installs k3s and applies the manifests. There is no Azure Kubernetes
Service control plane and no managed Kubernetes service.

The module creates cloud resources. Read the plan before you apply it, and run
the teardown steps when you are done.

## Cost note

This design should fit inside the 100 USD Azure for Students credit for 12
months under these assumptions:

- The Standard_B1s size is covered by the student free compute allowance, or
  the VM is stopped when it is not in use.
- The root disk uses Standard_LRS and stays near the default size.
- There is one static public IP address.
- There are no extra disks, snapshots, load balancers, or large outbound
  transfers.

Check the Azure pricing page for your region before you apply. The default B1s
size is the low cost option. Standard_B2ats_v2 is an alternative with more
memory if the app needs it. The auto shutdown option stops the VM on a daily
schedule, which can stretch the credit further.

## Files

- `deploy/azure/terraform`: provider, resource group, virtual network, subnet,
  network security group, public IP address, Linux VM, optional auto shutdown,
  outputs, and the cloud init template.
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
- A GHCR image built from this repository. The release workflow publishes the
  netweather container image. The manifest has a placeholder image name, and
  cloud init replaces it with the `netweather_image` value.
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
   # location             = "eastus"
   # resource_group_name  = "netweather-rg"
   # vm_size              = "Standard_B1s"
   # app_allowed_cidr     = "203.0.113.0/24"
   # public_ip_sku        = "Basic"
   # auto_shutdown_enabled = true
   # auto_shutdown_time    = "1900"
   # netweather_image      = "ghcr.io/your-user/netweather:latest"
   # ingress_host          = "netweather.westcountytech.com"
   # acme_email            = "greg@westcountytech.com"
   ```

   If Azure rejects a Basic public IP for your subscription or region, set
   `public_ip_sku` to `Standard`.

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
   `deploy/DOMAINS.md`.

   ```sh
   terraform output
   terraform output -raw public_ip
   terraform output -raw app_url
   terraform output -raw public_ip_url
   ```

6. Check the cluster over SSH.

   ```sh
   ssh -i /path/to/private_key azureuser@"$(terraform output -raw public_ip)"
   sudo k3s kubectl -n netweather get pods
   sudo k3s kubectl -n netweather get ingress
   ```

7. Create the DNS records from `deploy/DOMAINS.md` if they do not exist, then
   open the `app_url` value in a browser. The network security group allows
   ports 80 and 443. The deployment creates a random UI token in a Kubernetes
   Secret. Read it with this command on the host.

   ```sh
   sudo k3s kubectl -n netweather get secret netweather-auth \
     -o jsonpath='{.data.token}' | base64 -d
   ```

To apply the manifests by hand, use a local cluster context and these commands.
Replace the image placeholder with your GHCR image.

```sh
kubectl apply -f deploy/k8s/traefik-config.yaml
kubectl apply -f deploy/k8s/namespace.yaml
kubectl -n netweather create secret generic netweather-auth \
  --from-literal=token="$(openssl rand -hex 24)" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f deploy/k8s/pvc.yaml
sed 's|ghcr.io/OWNER/netweather:latest|ghcr.io/your-user/netweather:latest|' \
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

Terraform removes the VM, its disk, the public IP address, the network security
group, the subnet, the virtual network, and the resource group that it created.
The SSH key pair is not managed here, so delete it yourself if you no longer need
it. The GHCR image and any DNS records are also outside this module.

If you configured remote state, keep the storage account until after destroy
finishes. You can delete it and the state container when you no longer need the
state.
