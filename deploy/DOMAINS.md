# DNS and domain pattern

All public apps live under `apps.westcountytech.com`. One wildcard record covers every current and future app.

## GoDaddy records

Create these records at GoDaddy for `westcountytech.com`:

| Type | Name | Value | TTL |
| --- | --- | --- | --- |
| A | `*.apps` | VM public IP | 600 |
| A | `apps` | VM public IP | 600 |

The wildcard record answers `netweather.apps.westcountytech.com` and any future host under `apps.westcountytech.com`. The record for `apps` makes the base name resolve as well.

## New app checklist

1. Give the app an Ingress with a host such as `myapp.apps.westcountytech.com`.
2. Deploy the Ingress. Traefik reads the host and routes traffic.
3. Leave DNS alone. The wildcard record already covers the new host.

## HTTPS

The k3s Traefik chart uses a Let's Encrypt ACME resolver with the HTTP01 challenge. The resolver stores certificates in `/data/acme.json`, and each Ingress asks for a certificate through the `letsencrypt` resolver. Ports 80 and 443 stay open in the Azure network security group so the challenge and HTTPS traffic can reach the VM.

Set the ACME email with the `acme_email` Terraform variable. The default is `greg@westcountytech.com`.

## Changing the host

Set the `ingress_host` Terraform variable, or edit the `host` and `tls` values in the Ingress manifest when you apply it by hand. The default is `netweather.apps.westcountytech.com`.
