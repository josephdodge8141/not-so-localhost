# Not-So-Localhost

Expose services from multiple personal machines through one shared
`joedodge.dev` app inventory.

## Architecture

```text
apps.joedodge.dev
  -> shared portal tunnel replicas
  -> any online registry
  -> one S3 JSON registry

<app>--<node>.joedodge.dev
  -> exact DNS record
  -> that node's Cloudflare Tunnel
  -> that node's Traefik
  -> the registered HTTP target
```

Each node has:

- a required human-readable `NODE_NAME`
- a generated UUID persisted in the `node_identity` volume
- its own remotely managed Cloudflare Tunnel
- a registry replica reading and writing the same S3 state
- a reconciler that renders only apps owned by its UUID

The registry stores immutable JSON snapshots in S3 and conditionally updates one
`current.json` pointer. Concurrent disjoint registrations retry and converge;
registry writes fail rather than fork when S3 is unavailable.

NSL does not launch applications or create Swagger UI/pgweb sidecars. It only
registers and routes HTTP services.

## Prerequisites

- Docker Desktop and Docker Compose
- An S3 bucket with versioning enabled
- AWS credentials allowed to read/write `nsl/registry/v1/*`
- A deployed enrollment broker
- `ttyd` on macOS for host terminal access
- Keycloak/PostgreSQL credentials only on the auth-owner node

Homebrew's ttyd formula has no service definition. Start a writable login shell
on each macOS node and keep it running while NSL is in use:

```sh
/opt/homebrew/bin/ttyd -W -p 7681 zsh -l
```

Persistent ttyd startup is not currently managed by this repository.

The default `CORPORATE_CA_FILE` is an empty tracked placeholder. On a network
with TLS inspection, export the corporate CA bundle and point this variable to
that file before building.

## Deploy The Enrollment Broker

The Worker is the only service holding Cloudflare Tunnel and DNS edit
permissions.

1. Set the Cloudflare account and zone IDs in `broker/wrangler.jsonc`.
2. Create a Cloudflare API token limited to Tunnel Edit for the account and DNS
   Edit for the `joedodge.dev` zone.
3. Configure Worker secrets:

```sh
cd broker
npm install
npx wrangler secret put CLOUDFLARE_API_TOKEN
npx wrangler secret put BROKER_ADMIN_TOKEN
npx wrangler secret put NODE_CREDENTIAL_KEY
npx wrangler secret put AUTH_CLAIM_TOKEN
npm test
npx wrangler deploy
```

Alternatively, run `broker/setup-secrets.sh` after deployment. It prompts for
the Cloudflare API token without echoing it and generates the remaining secrets.

Keep `BROKER_ADMIN_TOKEN` on the operator machine. Nodes never receive the
Cloudflare account token.

## First Node

Install the current `nsl` CLI from its checkout; the latest tagged release
predates enrollment:

```sh
cd /path/to/nsl
go install ./cmd/nsl
export PATH="$HOME/go/bin:$PATH"
nsl enrollment-token --help
```

Copy the environment templates and provide secrets:

```sh
cp .env.example .env
cp registry/.env.example registry/.env
cp backup/.env.example backup/.env
cp postgres/.env.example postgres/.env
cp keycloak/.env.example keycloak/.env
```

Root `.env` for the first/auth node, initially without an enrollment token:

```dotenv
DOMAIN=joedodge.dev
NODE_NAME=macbook
ENROLLMENT_BROKER_URL=https://<broker-host>
NSL_ENROLLMENT_TOKEN=
REGISTRY_S3_BUCKET=not-so-localhost-backups
AWS_REGION=us-east-1
COMPOSE_PROFILES=auth
NSL_AUTH_OWNER=true
NSL_AUTH_CLAIM_TOKEN=<same value as broker AUTH_CLAIM_TOKEN>
REGISTRY_API_TOKEN=<node-local CLI API token>
REGISTRY_PROXY_TOKEN=<local proxy trust token>
OAUTH2_CLIENT_SECRET_REGISTRY=<shared OAuth client secret>
OAUTH2_COOKIE_SECRET=<shared OAuth cookie secret>
KEYCLOAK_USER_PASSWORD=<initial joe password>
KEYCLOAK_ADMIN_PASSWORD=<bootstrap admin password>
```

Set the S3 writer credentials in `registry/.env`. Configure `backup/.env`,
PostgreSQL, and Keycloak as described by their examples. Prebuild the images
before requesting the short-lived token:

```sh
docker compose build
```

Load the operator values created by `broker/setup-secrets.sh`, export them for
the CLI, and issue the token:

```sh
source .broker-secrets.env
export NSL_BROKER_URL NSL_BROKER_ADMIN_TOKEN
nsl enrollment-token --node-name macbook
```

Put the returned value in `NSL_ENROLLMENT_TOKEN`, then start the already-built
stack promptly:

```sh
docker compose up -d
```

Startup does the following:

1. Generates and persists the machine UUID.
2. Redeems the enrollment token.
3. Creates the node and shared portal tunnels.
4. Creates `t--macbook.joedodge.dev` and `apps.joedodge.dev`.
5. Registers the node in S3.
6. Claims the global auth-owner role.
7. Creates `auth.joedodge.dev` for this node.
8. Starts local routing and the shared portal replica.
9. Starts PostgreSQL and Keycloak only after the auth-owner guard passes.

Remove `NSL_ENROLLMENT_TOKEN` from `.env` after the first successful startup.
The durable node credentials remain in the `node_identity` volume.

For an existing single-node installation, `node scripts/configure-first-node.mjs
<node-name>` backs up and migrates the ignored environment files, reuses current
AWS/PostgreSQL/OAuth credentials, and requests the first enrollment token
without printing secrets.

## Add Another Laptop

On the new laptop, configure the same S3 registry and broker but do not enable
the auth profile. Leave the token empty while preparing the machine:

```dotenv
DOMAIN=joedodge.dev
NODE_NAME=laptop3
ENROLLMENT_BROKER_URL=https://<broker-host>
NSL_ENROLLMENT_TOKEN=
REGISTRY_S3_BUCKET=not-so-localhost-backups
AWS_REGION=us-east-1
NSL_AUTH_OWNER=false
REGISTRY_API_TOKEN=<new node-local CLI API token>
REGISTRY_PROXY_TOKEN=<new local proxy trust token>
OAUTH2_CLIENT_SECRET_REGISTRY=<same shared OAuth client secret>
OAUTH2_COOKIE_SECRET=<same shared OAuth cookie secret>
```

The new laptop still needs `registry/.env` with S3 credentials and `backup/.env`
with its node-local backup credentials. It does not need PostgreSQL or Keycloak
environment files. Prebuild before requesting the short-lived token:

```sh
docker compose build
```

On the configured operator machine, load `.broker-secrets.env` as shown above
and issue a new token:

```sh
nsl enrollment-token --node-name laptop3
```

Put it in the new laptop's root `.env`, then start the already-built stack
before the token expires:

```sh
docker compose up -d
```

The new registry replica immediately displays every existing app. It only
renders routes for apps assigned to its UUID.

## Register Applications

Register a native process from the machine running it:

```sh
nsl add \
  --name landing \
  --target-url http://host.docker.internal:7310
```

The resulting URL is:

```text
https://landing--<node-name>.joedodge.dev
```

For LiteLLM:

```sh
nsl add \
  --name litellm \
  --target-url http://host.docker.internal:4000 \
  --policy litellm
```

This creates two routes to the same LiteLLM process:

- `/v1` relies on LiteLLM virtual keys.
- Browser and management routes use Keycloak and LiteLLM authorization.

LiteLLM and its PostgreSQL database are app-owned and must already be running.
NSL does not provision either one.

## Shared Portal

Every registry replica reads the complete state, so `apps.joedodge.dev` shows:

- every registered app across every node
- each owning node and auth role
- direct `app--node.joedodge.dev` links
- the terminal link for each node

Registrations can be made from either laptop. S3 conditional writes serialize
concurrent updates, so adding one app on each laptop preserves both.

Set `NSL_API_TOKEN` to the root `REGISTRY_API_TOKEN` before using the local
`nsl` CLI. The browser portal uses a separate proxy-only trust header after
Keycloak authentication.

## Backups

The registry itself does not need `pg_dump`; immutable S3 snapshots are its
history. The backup service discovers history from object keys and writes:

```text
backups/v2/<node-uuid>/<target-id>/<timestamp>.sql.gz
```

Keycloak is backed up only on the auth node. Additional app-owned databases are
configured locally using `backup/targets.json.example`; secrets are referenced
through environment variable names rather than stored in the global registry.

Backup trigger and restore endpoints require:

```text
Authorization: Bearer <BACKUP_API_TOKEN>
```

Automated restore is currently disabled. A safe restore must validate into a
temporary database before replacing the live database.

## Auth Ownership

Keycloak and its local PostgreSQL remain on exactly one machine. The global S3
state records that machine's UUID. Compose's `auth` profile starts Keycloak only
after `/api/v2/auth-owner` confirms local ownership.

Moving auth is intentionally manual: stop and fence the old node, back up and
restore Keycloak PostgreSQL, change the recorded owner through a controlled
administrative migration, and then repoint the broker-managed auth hostname.
