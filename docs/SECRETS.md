# Secrets and Azure deployment

This page describes the credential model for the GitHub Actions to Azure deploy
path. It contains no secret values, and the deploy path itself stores none in
GitHub.

## GitHub Actions: zero secrets

The deploy path uses **zero Actions secrets and zero environment secrets**. The
`azure-production` environment has three variables:

- `AZURE_CLIENT_ID`
- `AZURE_TENANT_ID`
- `AZURE_SUBSCRIPTION_ID`

These are public Azure identifiers, not credentials. The workflow reads them
with `vars.*`, never `secrets.*`. There is no client secret, certificate, or
personal access token to rotate.

## OIDC to the managed identity

`azure/login` exchanges a short-lived GitHub Actions OIDC token for access to the
User-Assigned Managed Identity `netweather-gha` in resource group
`netweather-rg`. The federated credential subject is:

```text
repo:Greg-2600@3452584/netweather@1469300630:environment:azure-production
```

The `@3452584` owner ID and `@1469300630` repository ID are required because this
repository uses immutable GitHub Actions OIDC subjects. A subject without those
IDs will not match. The workflow job uses `environment: azure-production`, which
is what makes the subject match, so Greg must approve the environment review
before the job receives a token.

## Azure role scope

The custom role **Netweather VM RunCommand Deployer** is assigned to the identity
only on the `netweather` server VM in `netweather-rg`:

```text
/subscriptions/5f932826-de80-438e-9a0e-2a7ed67f119c/resourceGroups/netweather-rg/providers/Microsoft.Compute/virtualMachines/netweather
```

It allows `Microsoft.Compute/virtualMachines/read`,
`Microsoft.Compute/virtualMachines/runCommand/action`, and
`Microsoft.Compute/virtualMachines/runCommands/*`. It is not scoped to the
resource group and not granted on the agent VM.

Deployment is `az vm run-command` against that VM. It is not `terraform apply`
and it does not use SSH or a kubeconfig file; ports 22 and 6443 stay closed.

## How to deploy

After this workflow is merged to `main`, an operator has two entry points:

- **Manual:** Actions → **deploy azure** → **Run workflow**, then enter a released
  version such as `1.0.1`. A leading `v` is accepted and stripped.
- **Automatic:** a successful tag-triggered **release** workflow starts a deploy
  with the tag as the version. The deploy job only runs when the release
  workflow conclusion is `success`.

Greg must approve the `azure-production` environment review. The workflow then
validates the version, signs in with OIDC, and invokes `RunShellScript` on VM
`netweather` in resource group `netweather-rg`. The remote script updates the
`netweather` and `tor` images, waits for the rollout, and checks
`https://netweather.westcountytech.com/healthz`.

Always deploy a specific version tag. The manifests use
`imagePullPolicy: IfNotPresent`, so changing `:latest` in place does not make
the cluster pull a new image.

## UI token: `netweather-auth`

The cluster-resident Kubernetes secret `netweather-auth` (key `token`) is the UI
login token. Bootstrap creates it if it is missing. An operator reads it on the
server over SSH, for example:

```sh
sudo k3s kubectl -n netweather get secret netweather-auth \
  -o jsonpath='{.data.token}' | base64 -d
```

Rotate it on the server without printing the new value into CI:

```sh
new_token="$(openssl rand -hex 24)"
sudo k3s kubectl -n netweather create secret generic netweather-auth \
  --from-literal=token="$new_token" --dry-run=client -o yaml \
  | sudo k3s kubectl apply -f -
unset new_token
sudo k3s kubectl -n netweather rollout restart deployment/netweather
sudo k3s kubectl -n netweather rollout status deployment/netweather --timeout=180s
```

This updates the same secret and restarts the pods. Do not print the token in
GitHub Actions logs, and do not store it in GitHub secrets or in the repository.

## Terraform state and SSH key

Terraform state and the SSH private key stay on the operator machine, are
gitignored, and are not needed for application continuous deployment. Do not put
them in GitHub. `terraform apply` can replace the server VM and is not part of
this workflow.

## Azure Key Vault

Key Vault is intentionally skipped. It is not required at this scale, and
Standard Key Vault is billed per operation, so it is not a zero-cost addition.
OIDC plus the cluster-resident `netweather-auth` secret already cover the need.

## Related documentation

- `deploy/azure/README.md` covers bootstrap, Terraform, and the server commands.
- `deploy/k8s/deployment.yaml` shows the image fields and the secret reference.
- `.github/workflows/deploy-azure.yml` is the deploy workflow.
