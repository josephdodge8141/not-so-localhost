# NSL enrollment broker

The broker is the only component holding Cloudflare Tunnel and DNS edit
permissions. It issues short-lived, single-use node enrollment tokens and
creates exact `app--node.joedodge.dev` records.

## Configure

Set the account and zone IDs in `wrangler.jsonc`, then add secrets:

```sh
npx wrangler secret put CLOUDFLARE_API_TOKEN
npx wrangler secret put BROKER_ADMIN_TOKEN
npx wrangler secret put NODE_CREDENTIAL_KEY
npx wrangler secret put AUTH_CLAIM_TOKEN
```

For first-time setup, `./setup-secrets.sh` prompts for the restricted
Cloudflare API token, generates the other secrets, uploads them, and stores the
operator-only values in the gitignored `../.broker-secrets.env` file.

The Cloudflare token needs Tunnel Edit for the account and DNS Edit for the
`joedodge.dev` zone.

`AUTH_CLAIM_TOKEN` is copied only to the designated auth node as
`NSL_AUTH_CLAIM_TOKEN`. It prevents an ordinary enrolled node from claiming
`auth.joedodge.dev`.

## Issue a token

```sh
curl -X POST https://<broker>/v1/admin/enrollment-tokens \
  -H "Authorization: Bearer $BROKER_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"node_name":"laptop3","ttl_seconds":600}'
```

The returned `nsl1...` value is supplied once as `NSL_ENROLLMENT_TOKEN` on the
new node. It is not a Cloudflare credential and expires after at most 15
minutes.

## Adopt existing infrastructure DNS

Migration from the original single tunnel may require explicitly adopting the
existing `apps.joedodge.dev` or `auth.joedodge.dev` CNAME. The admin-only
`POST /v1/admin/adopt-dns` endpoint accepts one of those hostnames and an NSL
tunnel name. Normal enrollment never overwrites unmanaged DNS.
