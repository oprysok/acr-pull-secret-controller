# acr-pull-secret-controller

Keeps a short-lived Azure Container Registry pull secret in every Kubernetes namespace you select, using **workload identity federation** instead of a stored password. Works when the registry lives in a **different Microsoft Entra tenant** than the cluster, which AKS can't do natively.

```
cluster tenant                                     registry tenant
┌──────────────────────────────────────┐           ┌──────────────────────────────────┐
│ controller (SA acr-pull-secret-…)    │           │                                  │
│   │ projected SA token               │           │                                  │
│   ▼                                  │  FIC      │                                  │
│ app registration (multi-tenant) ◄────┼───────────┼── enterprise app ──AcrPull──► ACR│
│                                      │           │        ▲                         │
└──────────────────────────────────────┘           └────────┼─────────────────────────┘
                                                            │
  1. SA token  ──► login.microsoftonline.com/<registry tenant>  ──► Entra access token
  2. Entra token ──► https://<registry>/oauth2/exchange          ──► ACR refresh token (~3h)
  3. refresh token ──► Secret <secretName> (dockerconfigjson) in every selected namespace
```

## Why

AKS pulls images as the node pool's managed identity. A managed identity exists in one tenant only, so it can't be granted `AcrPull` on a registry in another tenant. The usual workaround is a service principal with a client secret baked into an image pull secret, which then has to be rotated by hand and redistributed to every namespace.

This controller replaces that secret with tokens that are minted on demand and expire within hours:

- **One identity per cluster.** One federated credential per cluster covers every namespace, so the 20-credentials-per-app limit isn't a concern.
- **No secrets to rotate.** Tokens are renewed at half their lifetime (configurable).
- **Opt-in per namespace** through a label, including namespaces created on the fly.
- **Self-healing.** Deleted or edited pull secrets are restored. A namespace that opts out loses its secret.

Alternatives considered:

| Option | Cross-tenant | Federated credentials needed |
|---|---|---|
| AKS `--attach-acr` (kubelet identity) | ❌ | – |
| AKS service-account image pull (identity bindings) | ❌ token proxy only serves bound managed identities, in their own tenant | – |
| [Azure/msi-acrpull](https://github.com/Azure/msi-acrpull) | ✅ | one per namespace × workload service account × cluster |
| **this controller** | ✅ | **one per cluster** |

## Requirements

- Kubernetes 1.30+ (the chart uses `ValidatingAdmissionPolicy`)
- [Azure workload identity](https://learn.microsoft.com/azure/aks/workload-identity-overview) on the cluster: OIDC issuer + mutating webhook (`--enable-oidc-issuer --enable-workload-identity` on AKS)
- The controller image must be pullable without this controller, for example from a public registry or one the node identity can already pull from

## Azure setup

Replace the placeholders. Run the app-registration steps in the **cluster's** tenant and the role assignment in the **registry's** tenant.

```bash
# --- cluster tenant ---------------------------------------------------------
APP_ID=$(az ad app create --display-name acr-pull-secret-controller \
  --sign-in-audience AzureADMultipleOrgs --query appId -o tsv)

ISSUER=$(az aks show -g <cluster-rg> -n <cluster> --query oidcIssuerProfile.issuerUrl -o tsv)

# one per cluster; subject = the controller's service account
az ad app federated-credential create --id $APP_ID --parameters "{
  \"name\": \"<cluster>\",
  \"issuer\": \"$ISSUER\",
  \"subject\": \"system:serviceaccount:acr-pull:acr-pull-secret-controller\",
  \"audiences\": [\"api://AzureADTokenExchange\"]
}"

# --- registry tenant ------------------------------------------------------
az login --tenant <registry-tenant-id>
SP_ID=$(az ad sp create --id $APP_ID --query id -o tsv)   # needs Application Administrator or similar
az role assignment create --assignee-object-id $SP_ID --assignee-principal-type ServicePrincipal \
  --role AcrPull --scope $(az acr show -n <registry> --query id -o tsv)
```

The app **must** be multi-tenant. A single-tenant app fails in the registry tenant with `AADSTS700016`.

## Install

```bash
helm install acr-pull-secret-controller charts/acr-pull-secret-controller \
  --namespace acr-pull --create-namespace \
  --set 'registries[0]=<registry>.azurecr.io' \
  --set azure.clientId=$APP_ID \
  --set azure.tenantId=<registry-tenant-id>
```

Opt a namespace in and reference the secret from workloads:

```bash
kubectl label namespace my-app acr-pull-secret-controller/enabled=true
```

```yaml
spec:
  imagePullSecrets:
    - name: acr-pull
```

## Configuration

| Value | Default | Description |
|---|---|---|
| `registries` | `[]` (required) | ACR login servers to issue credentials for. All go into one secret. |
| `azure.clientId` | required | Client ID of the multi-tenant app registration |
| `azure.tenantId` | required | Tenant that owns the registries |
| `secretName` | `acr-pull` | Name of the pull secret in each namespace |
| `namespaceSelector` | `acr-pull-secret-controller/enabled=true` | Label selector for namespaces that receive the secret |
| `refreshFraction` | `0.5` | Renew tokens after this share of their lifetime |
| `allowedRegistrySuffixes` | `[azurecr.io]` | Tokens are only sent to registries with these suffixes |
| `restrictSecretWrites.enabled` | `true` | Admission policy limiting the controller to the one secret name |
| `replicas` | `2` | Leader election is enabled automatically when > 1 |
| `metrics.serviceMonitor.enabled` | `false` | Create a Prometheus Operator `ServiceMonitor` |

## Operations

**Failure behaviour.** If Entra or the registry is unavailable, reconciliation backs off and existing secrets are left untouched. Pulls keep working until the current refresh token expires, roughly 3 hours after the last successful renewal. With the default `refreshFraction` that leaves about 1.5 hours to notice and fix the problem.

**Metrics** (`:8080/metrics`, plus the standard controller-runtime metrics):

| Metric | Use |
|---|---|
| `acr_pull_secret_token_refresh_total{result}` | Alert on `result="error"` increasing |
| `acr_pull_secret_token_expiry_timestamp_seconds{registry}` | Alert when `… - time() < 3600`: renewal is overdue |
| `acr_pull_secret_sync_total{result}` | `updated` / `unchanged` / `removed` / `token_error` / `error` |

Each managed secret carries `acr-pull-secret-controller/refreshed-at` and `…/expires-at` annotations, and failures are recorded as events on the namespace:

```bash
kubectl get secret acr-pull -n my-app -o jsonpath='{.metadata.annotations}'
kubectl get events -n my-app --field-selector involvedObject.kind=Namespace
```

**Existing secrets.** The controller never overwrites a secret it didn't create (one without the label `app.kubernetes.io/managed-by=acr-pull-secret-controller`). Delete the old secret to hand it over.

## Troubleshooting

| Symptom | Cause |
|---|---|
| `AADSTS700016` | App not multi-tenant, or no enterprise app (`az ad sp create`) in the registry tenant |
| `AADSTS700213` / `AADSTS70021` | Federated credential doesn't match the token: compare issuer (including the trailing `/`), subject and audience. A new credential can take a minute to become effective. |
| `HTTP 401` from `/oauth2/exchange` | Missing `AcrPull` on the registry, or the role assignment hasn't propagated yet |
| `secret … exists but is not managed` | A same-named secret pre-exists. Delete it. |
| Pod `ImagePullBackOff` right after opt-in | The pod was created before the secret. It retries on its own, or you can restart it. |

Decode the token the controller presents:

```bash
kubectl -n acr-pull create token acr-pull-secret-controller --audience api://AzureADTokenExchange \
  | cut -d. -f2 | tr '_-' '/+' | base64 -d 2>/dev/null | jq '{iss, sub, aud}'
```

## Security

- The controller can read and write secrets cluster-wide (required to create the pull secret in any namespace). Its cache only holds secrets carrying its own label, and the bundled `ValidatingAdmissionPolicy` denies any write to a secret with a different name.
- Refresh tokens grant pull access only (`AcrPull`) and expire within hours.
- Tokens are sent only to hosts matching `allowedRegistrySuffixes`.

## Development

```bash
make test        # unit tests (go test -race)
make vet
make image       # docker build
make helm-lint
```

Layout:

```
cmd/manager/            flags, wiring, manager setup
internal/token/         Entra token → ACR exchange, caching, renewal schedule
internal/controller/    namespace reconciler, dockerconfigjson rendering
charts/acr-pull-secret-controller/
```

### CI and releases

| Workflow | When | What |
|---|---|---|
| `ci` | Every PR and push to `main` | golangci-lint, `go test -race`, `helm lint`, multi-arch image build, Trivy scan (fails on fixable HIGH/CRITICAL) |
| `codeql` | PRs, `main`, weekly | CodeQL static analysis |
| `release` | `v*` tag | Tests, pushes `ghcr.io/oprysok/acr-pull-secret-controller:<version>` (amd64 + arm64) with SBOM and SLSA provenance, creates a GitHub release with the packaged chart |
| `image-scan` | Weekly | Trivy scan of the latest published image, results in the Security tab |

To release, merge to `main`, then tag:

```bash
git tag -a v0.2.0 -m v0.2.0 && git push origin v0.2.0
```

`main` only accepts changes through pull requests with passing checks, and only maintainers can create `v*` tags. Actions are pinned to commit SHAs, and Dependabot keeps Go modules, the base image and actions current.

## License

[MIT](LICENSE)
