# Security policy

`netweather` sends requests to third-party hosts from the operator's network and serves a dashboard that discloses the operator's vantage point. Both halves are security-relevant.

## Reporting a vulnerability

Use GitHub's **private vulnerability reporting** on this repository (Security → Report a vulnerability). Do not open a public issue.

Please include: what you did, what happened, what you expected, and the version/commit. A minimal reproduction is worth more than a description.

This is a volunteer project with no bounty programme. Reports are acknowledged on a best-effort basis; a confirmed issue in the SSRF guard, authentication, or the container hardening is treated as urgent.

## In scope

- Bypassing the private-address / SSRF guard (`docs/THREAT_MODEL.md` §5.2): DNS rebinding, redirect tricks, IPv4-mapped IPv6, alternate address encodings
- Bypassing the outbound port allowlist or the GET/HEAD restriction
- Authentication bypass, session fixation, CSRF, token leakage in logs or URLs
- XSS, CSP bypass, or any path that renders target-controlled content
- Container escape via configuration the project ships
- Supply-chain issues in the build or release artefacts
- Anything that turns `netweather` into an open proxy, scanner, or amplifier

## Out of scope

- Volumetric DoS against your own instance
- Vulnerabilities in probed third-party services
- Missing hardening on the host (kernel, Docker daemon, cloud metadata configuration) that the container cannot control
- The disclosure that the dashboard publishes your target list and egress ASN — that is the product, documented in `docs/NETWORKING.md` §7.1
- Reports generated solely by a scanner with no demonstrated impact

## Safe harbour

Good-faith research against **your own** instance, following this policy, will not be pursued. Do not test against other operators' instances, and do not use the tool's probe engine as part of an exploit against a third party.
