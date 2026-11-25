# DNS and domain pattern

Each public app gets its own A record named after its repo directly under `westcountytech.com`. For example, netweather uses `netweather.westcountytech.com`. There is no wildcard record.

## GoDaddy records

Create one record at GoDaddy for each app under `westcountytech.com`:

| Type | Name | Value | TTL |
| --- | --- | --- | --- |
| A | `netweather` | VM public IP | 600 |

Add another row in the same form when you deploy a new app, using the repo name as the record name.

## New app checklist

1. Give the app an Ingress with a host named after its repo, such as `myapp.westcountytech.com`.
2. Deploy the Ingress. Traefik reads the host and routes traffic.
3. Add an A record for the repo name at GoDaddy. Point it at the VM public IP.

## HTTPS

The k3s Traefik chart uses a Let's Encrypt ACME resolver with the HTTP01 challenge. The resolver stores certificates in `/data/acme.json`, and each Ingress asks for a certificate through the `letsencrypt` resolver. Ports 80 and 443 stay open in the Azure network security group so the challenge and HTTPS traffic can reach the VM.

Set the ACME email with the `acme_email` Terraform variable. The default is `greg@westcountytech.com`.

## Changing the host

Set the `ingress_host` Terraform variable, or edit the `host` and `tls` values in the Ingress manifest when you apply it by hand. The default is `netweather.westcountytech.com`.
