# Threat model

Lightweight [STRIDE](https://learn.microsoft.com/en-us/azure/security/develop/threat-modeling-tool-threats) review.
Revisit it when you add an external integration, a data store, or a new trust boundary.

- Owner: Muhammad Rashid ([@codewithmuh](https://github.com/codewithmuh))
- Last reviewed: 2026-10-14 (P1 in progress)

Tasuki is fictional and every person in it is simulated, but the controls are built as if the data were real.

## Assets

| Asset | Why it matters |
|---|---|
| Customer personal data: name, phone, drop-off address | APPI (Japan's personal-information law); design §11 |
| Courier identities and live positions | A person's location in real time |
| Order and money state (orders DB, outbox, events) | Never lose an order, never double-charge (design G1) |
| Signing secrets: `COURIER_TOKEN_SECRET`, `TRACKING_TOKEN_SECRET`, `ADMIN_TOKEN`, merchant API keys | Whoever holds them can act as a courier, read a delivery, or create tenants |
| The AWS account and its Terraform state | Bills money; holds the platform |
| Build and release pipeline: images, signing identity, CI roles | Everything in the cluster runs what it produces |
| Service availability | SLOs in [observability/slo](../observability/slo/README.md) |

## Trust boundaries

1. **Internet → services**: merchants (API key), couriers (JWT), customers (tracking link), the ops console.
2. **Services → data stores**: PostgreSQL, Kafka, Valkey inside one namespace, behind default-deny NetworkPolicies.
3. **GitHub Actions → AWS**: OIDC federation, no stored keys.
4. **Git → cluster**: Argo CD applies whatever is on `main` under `deploy/`.
5. **Laptop → AWS / EKS API**: an AWS profile for the Tasuki account; the EKS API from allowed CIDRs only.

## Threats and mitigations

| STRIDE | Threat | Mitigation | Status |
|---|---|---|---|
| Spoofing | Forged release images | Keyless cosign signatures from `release.yml` on `refs/heads/main`; Kyverno `verify-image-signatures` checks them | ⚠️ policy in Audit until every image is signed for a full release cycle ([deploy/README.md](../deploy/README.md#policies)) |
| Spoofing | A caller impersonates a courier | HS256 JWTs (`role=courier`, 24 h), HS256 only (no `alg` confusion), courier ID taken from the token, never the body | ✅ |
| Spoofing | Forged or shared tracking links | Signed JWT per order (`role=tracking`, expires 24 h after the promised time); 401 on a bad token; the stream ends at delivery | ✅ |
| Spoofing | Admin (seeding) API abused | `ADMIN_TOKEN` bearer; unset means the admin API is off; in the cluster it comes from Secrets Manager. It is one static token with full tenant-creation rights | ⚠️ accepted for P1 (below) |
| Spoofing | CI assumes AWS roles it should not | GitHub OIDC: the read-only plan role trusts this repo's pull requests and `main`; the apply role trusts only jobs in the `aws-tokyo` environment (required reviewer, `main` only). Fork PRs get no OIDC token | ✅ |
| Tampering | Compromised GitHub Action or dependency | Actions pinned to SHAs, Dependabot (+ Renovate for Helm charts in `deploy/`), Trivy scans, SBOM and provenance on every image | ✅ |
| Tampering | Terraform state altered or lost | Private, versioned, encrypted, TLS-only S3 bucket with native locking, created by script outside Terraform | ✅ |
| Tampering | Replayed or duplicated orders and events | `Idempotency-Key` with a body hash (24 h); outbox in the order's transaction; consumers dedupe on `event_id` | ✅ |
| Repudiation | Unknown who changed what | Protected `main`, required reviews and checks, signed release provenance, apply runs approved in the `aws-tokyo` environment | ✅ |
| Information disclosure | Secrets committed to git | gitleaks in pre-commit and CI; `.env` ignored; `.env.example` holds obviously fake placeholders only | ✅ |
| Information disclosure | Account ID or allowed IPs leak through public plan comments | `terraform.yml` replaces the account ID and allowed CIDRs in PR comments and masks them in logs | ✅ |
| Information disclosure | Customer personal data leaks | Per-field Fernet encryption at rest; never in events (coordinates only), logs, API responses or idempotency records (tests check all four); quotes deleted after 24 h; data stays in ap-northeast-1 | ⚠️ 90-day deletion and KMS envelope encryption are designed, not built |
| Information disclosure | Tracking tokens leak through logs or Referer | Access logs record route patterns, never paths; the console logs `/track/[redacted]` and sends `Referrer-Policy: no-referrer` | ✅ |
| Information disclosure | The ops fleet stream exposes every courier's position | Locally `FLEET_STREAM_PUBLIC=true` (no auth); in the cluster one shared `FLEET_STREAM_TOKEN`, no per-user identity | ⚠️ accepted for P1 (below) |
| Denial of service | Request floods exhaust a service | Server timeouts, body limits, bounded queues; fleet-ingest sheds with 503 + `Retry-After`; tracking caps viewers per replica | ⚠️ no edge rate limiting or WAF yet (no ingress in P1) |
| Denial of service | Kafka or Valkey outage stops the business | Outbox waits in PostgreSQL; Valkey is rebuilt from Kafka; tracking never reads Valkey (design §10) | ✅ |
| Elevation of privilege | Container breakout | Non-root, read-only root filesystem, all capabilities dropped, no privilege escalation, distroless or slim images; Pod Security `restricted` and Kyverno Deny policies in the cluster | ✅ |
| Elevation of privilege | Kubernetes API reached from the internet | Public endpoint limited to `api_allowed_cidrs` (your current IP by default; `0.0.0.0/0` is rejected); nodes use the private endpoint; admins listed explicitly | ✅ |
| Elevation of privilege | The CI apply role escalates to administrator | PowerUserAccess plus IAM only on `tasuki-*` names, explicit denies on editing the CI roles. It can still create a `tasuki-*` role with any policy and assume it: there is no permissions boundary | ⚠️ accepted (below) |

## Accepted risks

| Risk | Why accepted | Owner | Review |
|---|---|---|---|
| The apply role can escalate to admin through a new `tasuki-*` IAM role (no permissions boundary) | Personal single-purpose account; the role runs only after a reviewer approves in `aws-tokyo`; a boundary would have to cover every role the EKS modules create | Muhammad Rashid | Before any second person gets access, or 2027-01-14 |
| The EKS API endpoint is public (CIDR-restricted) instead of private behind a VPN | Ephemeral platform used from one laptop; IAM authentication still required | Muhammad Rashid | 2027-01-14 |
| `ADMIN_TOKEN` is one static bearer token with full tenant-creation rights | Seeding for a simulated city; replaced by OAuth2 client credentials and real onboarding in P3 | Muhammad Rashid | P3 |
| The ops fleet stream has no user authentication (none locally, one shared token in the cluster) | Simulated couriers only; the console has no login yet | Muhammad Rashid | Before any real data, or P2 |
| Local secrets in `.env.example` are public placeholders | They only work against a stack bound to 127.0.0.1; the cluster's secrets come from Secrets Manager | Muhammad Rashid | — |
| No PostgreSQL backups yet (CloudNativePG to S3 not configured) | Data is synthetic and re-seeded by the simulator | Muhammad Rashid | Before the first game day on data loss |
