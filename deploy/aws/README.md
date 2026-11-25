# Deploy netweather on AWS with k3s

This directory contains a small Terraform module and Kubernetes manifests for a
single host deployment. Terraform creates a VPC, one public subnet, a security
group, and one EC2 instance. Cloud init installs k3s and applies the manifests.
There is no EKS control plane and no managed Kubernetes service.

The module creates cloud resources. Read the plan before you apply it, and run
the teardown steps when you are done.

## Cost note

Prices change and vary by region. Check the AWS price pages for the region you
use. The values below are rough defaults for `us-east-1`.

- A t4g.small instance is about 0.0168 USD per hour, about 12 USD per month if
  it runs for a full month. The x86 alternative t3.micro is about 0.0104 USD
  per hour.
- The 8 GiB gp3 root volume is about 0.64 USD per month.
- A public IPv4 address is about 0.005 USD per hour, about 3.60 USD per month.
- There is no EKS control plane fee because k3s runs directly on EC2. EKS would
  add about 0.10 USD per hour.
- The VPC, subnet, route table, security group, and k3s itself have no separate
  charge. Outbound data transfer may add a small charge.

## Files

- `deploy/aws/terraform`: provider, VPC, security group, EC2 instance, outputs,
  and the cloud init template.
- `deploy/k8s`: namespace, PVC, Deployment, Service, and optional Ingress for
  k3s Traefik.
- `.github/workflows/aws_k3s.yml`: CI checks only. It runs Terraform format
  checks, `terraform init -backend=false`, `terraform validate`, and
  kubeconform. It never runs apply and uses no AWS credentials.

## Prerequisites

- Terraform 1.5 or newer.
- AWS credentials for the target account. Terraform reads the standard AWS
  environment variables or a named profile. Keep credentials out of this
  repository.
- An existing EC2 key pair in the target region.
- A GHCR image built from this repository. The release workflow publishes
  `ghcr.io/greg-2600/netweather`. The manifest uses a placeholder, and cloud
  init replaces it with the `netweather_image` value.
- kubectl if you want to inspect the cluster from your machine.

## Deploy

1. Enter the Terraform directory.

   ```sh
   cd deploy/aws/terraform
   ```

2. Select the AWS account and region with your usual profile.

   ```sh
   export AWS_PROFILE=your-profile
   export AWS_REGION=us-east-1
   ```

3. Create a local `terraform.tfvars`. This file is ignored by git.

   ```hcl
   key_name         = "my-key"
   ssh_allowed_cidr = "203.0.113.10/32"

   # Optional overrides.
   # region           = "us-east-1"
   # instance_type    = "t3.micro"
   # ami_architecture = "amd64"
   # app_allowed_cidr = "203.0.113.0/24"
   # netweather_image = "ghcr.io/your-user/netweather:latest"
   ```

4. Initialize, check, and apply.

   ```sh
   terraform init
   terraform fmt -check
   terraform validate
   terraform plan -out netweather.tfplan
   terraform apply netweather.tfplan
   ```

5. Read the outputs. Cloud init needs a few minutes to install k3s and pull the
   image.

   ```sh
   terraform output
   terraform output -raw public_ip
   terraform output -raw app_url
   ```

6. Check the cluster over SSH.

   ```sh
   ssh -i /path/to/my-key.pem ubuntu@"$(terraform output -raw public_ip)"
   sudo k3s kubectl -n netweather get pods
   sudo k3s kubectl -n netweather get ingress
   ```

7. Open the `app_url` value in a browser. The deployment creates a random UI
   token in a Kubernetes Secret. Read it with this command on the host.

   ```sh
   sudo k3s kubectl -n netweather get secret netweather-auth \
     -o jsonpath='{.data.token}' | base64 -d
   ```

To apply the manifests by hand, use a local cluster context and these commands.
Replace the image placeholder with your GHCR image.

```sh
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

To skip the Ingress, do not apply `deploy/k8s/ingress.yaml`. Use port forwarding
instead.

```sh
kubectl -n netweather port-forward svc/netweather 2600:80
```

## Tear down

Run destroy from the Terraform directory, using the same profile and variables
that you used for apply.

```sh
cd deploy/aws/terraform
terraform destroy
```

Terraform removes the instance, its EBS volume, the security group, and the VPC
resources that it created. The key pair is not managed here, so delete it
yourself if you no longer need it. The GHCR image and any DNS records are also
outside this module.

If you configured remote state, keep the state bucket until after destroy
finishes. You can delete it and the lock table when you no longer need the
state.
