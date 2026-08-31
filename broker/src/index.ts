import { DurableObject } from "cloudflare:workers";
import { appHostname, parseEnrollmentToken, requireSlug } from "./validation";

interface Env {
  BROKER: DurableObjectNamespace<EnrollmentBroker>;
  DOMAIN: string;
  CLOUDFLARE_ACCOUNT_ID: string;
  CLOUDFLARE_ZONE_ID: string;
  CLOUDFLARE_API_TOKEN: string;
  BROKER_ADMIN_TOKEN: string;
  NODE_CREDENTIAL_KEY: string;
  AUTH_CLAIM_TOKEN: string;
}

interface CloudflareResponse<T> {
  success: boolean;
  errors: Array<{ code: number; message: string }>;
  result: T;
}

interface NodeRow {
  node_id: string;
  node_name: string;
  tunnel_id: string;
  tunnel_name: string;
  credential_hash: string;
}

const jsonHeaders = { "Content-Type": "application/json", "Cache-Control": "no-store" };

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    if (new URL(request.url).pathname === "/healthz") return new Response(null, { status: 204 });
    const id = env.BROKER.idFromName("broker-v1");
    return env.BROKER.get(id).fetch(request);
  },
};

export class EnrollmentBroker extends DurableObject<Env> {
	private queue: Promise<void> = Promise.resolve();

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
	if (!env.CLOUDFLARE_API_TOKEN || !env.BROKER_ADMIN_TOKEN || !env.NODE_CREDENTIAL_KEY || !env.AUTH_CLAIM_TOKEN) {
		throw new Error("required broker secrets are not configured");
	}
    this.ctx.storage.sql.exec(`
      CREATE TABLE IF NOT EXISTS enrollment_tokens (
        id TEXT PRIMARY KEY,
        secret_hash TEXT NOT NULL,
        node_name TEXT NOT NULL,
        expires_at INTEGER NOT NULL,
        claimed_at INTEGER,
        claimed_node_id TEXT
      );
      CREATE TABLE IF NOT EXISTS nodes (
        node_id TEXT PRIMARY KEY,
        node_name TEXT NOT NULL UNIQUE,
        tunnel_id TEXT NOT NULL UNIQUE,
        tunnel_name TEXT NOT NULL UNIQUE,
        credential_hash TEXT NOT NULL,
        auth_owner INTEGER NOT NULL DEFAULT 0,
        created_at INTEGER NOT NULL
      );
      CREATE TABLE IF NOT EXISTS apps (
        node_id TEXT NOT NULL,
        app_name TEXT NOT NULL,
        hostname TEXT NOT NULL UNIQUE,
        created_at INTEGER NOT NULL,
        PRIMARY KEY (node_id, app_name)
      );
    `);
	const nodeColumns = [...this.ctx.storage.sql.exec("PRAGMA table_info(nodes)")].map((row) => String(row.name));
	if (!nodeColumns.includes("auth_owner")) this.ctx.storage.sql.exec("ALTER TABLE nodes ADD COLUMN auth_owner INTEGER NOT NULL DEFAULT 0");
	const tokenColumns = [...this.ctx.storage.sql.exec("PRAGMA table_info(enrollment_tokens)")].map((row) => String(row.name));
	if (!tokenColumns.includes("claimed_node_id")) this.ctx.storage.sql.exec("ALTER TABLE enrollment_tokens ADD COLUMN claimed_node_id TEXT");
	this.ctx.storage.sql.exec("CREATE UNIQUE INDEX IF NOT EXISTS one_auth_owner ON nodes(auth_owner) WHERE auth_owner = 1");
  }

	fetch(request: Request): Promise<Response> {
		const result = this.queue.then(() => this.handle(request));
		this.queue = result.then(() => undefined, () => undefined);
		return result;
	}

  private async handle(request: Request): Promise<Response> {
    try {
      const url = new URL(request.url);
      if (request.method === "POST" && url.pathname === "/v1/admin/enrollment-tokens") {
        return await this.issueToken(request);
      }
      if (request.method === "POST" && url.pathname === "/v1/admin/adopt-dns") {
        return await this.adoptDNS(request);
      }
      if (request.method === "POST" && url.pathname === "/v1/enroll") {
        return await this.enroll(request);
      }
      const appsSyncMatch = url.pathname.match(/^\/v1\/nodes\/([^/]+)\/apps$/);
      if (request.method === "PUT" && appsSyncMatch) {
        return await this.syncApps(request, appsSyncMatch[1]);
      }
      const appMatch = url.pathname.match(/^\/v1\/nodes\/([^/]+)\/apps\/([^/]+)$/);
      if (request.method === "PUT" && appMatch) {
        return await this.provisionApp(request, appMatch[1], appMatch[2]);
      }
	  const authMatch = url.pathname.match(/^\/v1\/nodes\/([^/]+)\/auth$/);
	  if (request.method === "PUT" && authMatch) {
		return await this.provisionAuth(request, authMatch[1]);
	  }
      return this.error(404, "not_found", "endpoint not found");
    } catch (error) {
      const message = error instanceof Error ? error.message : "unexpected error";
      return this.error(400, "invalid_request", message);
    }
  }

  private async issueToken(request: Request): Promise<Response> {
    if (!await this.bearerMatches(request, this.env.BROKER_ADMIN_TOKEN)) {
      return this.error(401, "unauthorized", "admin authentication required");
    }
    const body = await this.body(request) as { node_name?: string; ttl_seconds?: number };
    const nodeName = requireSlug(body.node_name, "node_name");
    const existing = this.first<{ node_name: string }>(
      "SELECT node_name FROM nodes WHERE node_name = ?", nodeName,
    );
    if (existing) return this.error(409, "node_exists", "node is already enrolled");
    const ttl = Math.min(Math.max(body.ttl_seconds ?? 600, 60), 900);
    const id = crypto.randomUUID();
    const secret = randomToken(32);
    const expiresAt = Date.now() + ttl * 1000;
    this.ctx.storage.sql.exec(
      "INSERT INTO enrollment_tokens (id, secret_hash, node_name, expires_at) VALUES (?, ?, ?, ?)",
      id, await sha256(secret), nodeName, expiresAt,
    );
    return this.json({
      token_id: id,
      node_name: nodeName,
      enrollment_token: `nsl1.${id}.${secret}`,
      expires_at: new Date(expiresAt).toISOString(),
    }, 201);
  }

  private async adoptDNS(request: Request): Promise<Response> {
    if (!await this.bearerMatches(request, this.env.BROKER_ADMIN_TOKEN)) {
      return this.error(401, "unauthorized", "admin authentication required");
    }
    const body = await this.body(request) as { hostname?: string; tunnel_name?: string };
    if (typeof body.hostname !== "string" || typeof body.tunnel_name !== "string") {
      throw new Error("hostname and tunnel_name are required");
    }
    const allowedHostnames = new Set([`apps.${this.env.DOMAIN}`, `auth.${this.env.DOMAIN}`]);
    if (!allowedHostnames.has(body.hostname)) throw new Error("only NSL infrastructure hostnames may be adopted");
    if (body.tunnel_name !== "nsl-portal" && !body.tunnel_name.startsWith("nsl-")) {
      throw new Error("tunnel_name is not managed by NSL");
    }
    const tunnel = await this.createTunnel(body.tunnel_name);
    await this.adoptExactDNS(body.hostname, tunnel.id);
    return this.json({ hostname: body.hostname, tunnel_id: tunnel.id, status: "adopted" });
  }

  private async enroll(request: Request): Promise<Response> {
    const body = await this.body(request) as { node_id?: string; node_name?: string; enrollment_token?: string };
    if (typeof body.node_id !== "string" || !/^[0-9a-f-]{36}$/.test(body.node_id)) {
      return this.error(400, "invalid_node_id", "node_id must be a UUID");
    }
    const nodeName = requireSlug(body.node_name, "node_name");
    const parsed = parseEnrollmentToken(body.enrollment_token);
    const token = this.first<{ secret_hash: string; node_name: string; expires_at: number; claimed_at: number | null; claimed_node_id: string | null }>(
      "SELECT secret_hash, node_name, expires_at, claimed_at, claimed_node_id FROM enrollment_tokens WHERE id = ?", parsed.id,
    );
    if (!token || !timingSafeEqual(token.secret_hash, await sha256(parsed.secret))) {
      return this.error(401, "invalid_token", "enrollment token is invalid");
    }
    if (token.node_name !== nodeName) return this.error(409, "wrong_node", "token belongs to another node");

    let node = this.first<NodeRow>(
      "SELECT node_id, node_name, tunnel_id, tunnel_name, credential_hash FROM nodes WHERE node_name = ?", nodeName,
    );
    const credential = await derivedCredential(this.env.NODE_CREDENTIAL_KEY, body.node_id);
    if (node) {
      if (node.node_id !== body.node_id || token.claimed_node_id !== body.node_id) return this.error(409, "token_used", "token has already been used");
      return this.enrollmentResponse(node, credential);
    }
	if (token.claimed_node_id && token.claimed_node_id !== body.node_id) return this.error(409, "token_used", "token has already been used");
	if (!token.claimed_at && token.expires_at < Date.now()) return this.error(410, "token_expired", "enrollment token expired");
	const conflictingID = this.first<{ node_name: string }>("SELECT node_name FROM nodes WHERE node_id = ?", body.node_id);
	if (conflictingID) return this.error(409, "node_id_exists", `node ID is already assigned to ${conflictingID.node_name}`);
	if (!token.claimed_at) {
		this.ctx.storage.sql.exec(
			"UPDATE enrollment_tokens SET claimed_at = ?, claimed_node_id = ? WHERE id = ? AND claimed_at IS NULL",
			Date.now(), body.node_id, parsed.id,
		);
	}

    const tunnelName = `nsl-${nodeName}`;
    const tunnel = await this.createTunnel(tunnelName);
    await this.configureTunnel(tunnel.id, nodeName, []);
    await this.ensureDNS(`t--${nodeName}.${this.env.DOMAIN}`, tunnel.id);
    node = {
      node_id: body.node_id,
      node_name: nodeName,
      tunnel_id: tunnel.id,
      tunnel_name: tunnelName,
      credential_hash: await sha256(credential),
    };
    this.ctx.storage.sql.exec(
      "INSERT INTO nodes (node_id, node_name, tunnel_id, tunnel_name, credential_hash, created_at) VALUES (?, ?, ?, ?, ?, ?)",
      node.node_id, node.node_name, node.tunnel_id, node.tunnel_name, node.credential_hash, Date.now(),
    );
    return this.enrollmentResponse(node, credential);
  }

  private async enrollmentResponse(node: NodeRow, credential: string): Promise<Response> {
    const tunnelToken = await this.cloudflare<string>(
      `/accounts/${this.env.CLOUDFLARE_ACCOUNT_ID}/cfd_tunnel/${node.tunnel_id}/token`,
    );
    const portal = await this.ensurePortalTunnel();
    return this.json({
      node_id: node.node_id,
      node_name: node.node_name,
      node_credential: credential,
      tunnel: { id: node.tunnel_id, token: tunnelToken },
      portal_tunnel: portal,
      terminal_hostname: `t--${node.node_name}.${this.env.DOMAIN}`,
    });
  }

  private async provisionApp(request: Request, rawNode: string, rawApp: string): Promise<Response> {
    const nodeName = requireSlug(rawNode, "node_name");
    const appName = requireSlug(rawApp, "app_name");
    const node = this.first<NodeRow>(
      "SELECT node_id, node_name, tunnel_id, tunnel_name, credential_hash FROM nodes WHERE node_name = ?", nodeName,
    );
    if (!node || !await this.bearerHashMatches(request, node.credential_hash)) {
      return this.error(401, "unauthorized", "node authentication required");
    }
    const hostname = appHostname(appName, nodeName, this.env.DOMAIN);
    this.ctx.storage.sql.exec(
      "INSERT OR IGNORE INTO apps (node_id, app_name, hostname, created_at) VALUES (?, ?, ?, ?)",
      node.node_id, appName, hostname, Date.now(),
    );
	await this.configureNodeTunnel(node);
    await this.ensureDNS(hostname, node.tunnel_id);
    return this.json({ node_id: node.node_id, node_name: node.node_name, app_name: appName, hostname, status: "ready" });
  }

  private async syncApps(request: Request, rawNode: string): Promise<Response> {
    const nodeName = requireSlug(rawNode, "node_name");
    const node = this.first<NodeRow>(
      "SELECT node_id, node_name, tunnel_id, tunnel_name, credential_hash FROM nodes WHERE node_name = ?", nodeName,
    );
    if (!node || !await this.bearerHashMatches(request, node.credential_hash)) {
      return this.error(401, "unauthorized", "node authentication required");
    }
    const body = await this.body(request) as { apps?: unknown };
    if (!Array.isArray(body.apps) || body.apps.length > 200) {
      throw new Error("apps must be an array with at most 200 entries");
    }
    const desired = new Map<string, string>();
    for (const value of body.apps) {
      const appName = requireSlug(value, "app_name");
      desired.set(appName, appHostname(appName, nodeName, this.env.DOMAIN));
    }
    const existing = [...this.ctx.storage.sql.exec(
      "SELECT app_name, hostname FROM apps WHERE node_id = ?", node.node_id,
    )].map((row) => ({ appName: String(row.app_name), hostname: String(row.hostname) }));
    for (const app of existing) {
      if (desired.has(app.appName)) continue;
      await this.deleteDNS(app.hostname, node.tunnel_id);
      this.ctx.storage.sql.exec("DELETE FROM apps WHERE node_id = ? AND app_name = ?", node.node_id, app.appName);
    }
    for (const [appName, hostname] of desired) {
      this.ctx.storage.sql.exec(
        "INSERT OR IGNORE INTO apps (node_id, app_name, hostname, created_at) VALUES (?, ?, ?, ?)",
        node.node_id, appName, hostname, Date.now(),
      );
    }
    await this.configureNodeTunnel(node);
    for (const hostname of desired.values()) await this.ensureDNS(hostname, node.tunnel_id);
    return this.json({ node_id: node.node_id, node_name: node.node_name, apps: [...desired.keys()].sort(), status: "ready" });
  }

	private async provisionAuth(request: Request, rawNode: string): Promise<Response> {
		if (!timingSafeEqual(request.headers.get("X-NSL-Auth-Claim") ?? "", this.env.AUTH_CLAIM_TOKEN)) {
			return this.error(401, "auth_claim_unauthorized", "auth ownership requires operator authorization");
		}
		const nodeName = requireSlug(rawNode, "node_name");
		const node = this.first<NodeRow>(
			"SELECT node_id, node_name, tunnel_id, tunnel_name, credential_hash FROM nodes WHERE node_name = ?", nodeName,
		);
		if (!node || !await this.bearerHashMatches(request, node.credential_hash)) {
			return this.error(401, "unauthorized", "node authentication required");
		}
		const owner = this.first<{ node_name: string }>("SELECT node_name FROM nodes WHERE auth_owner = 1");
		if (owner && owner.node_name !== nodeName) {
			return this.error(409, "auth_owner_exists", `auth is already owned by ${owner.node_name}`);
		}
		this.ctx.storage.sql.exec("UPDATE nodes SET auth_owner = CASE WHEN node_id = ? THEN 1 ELSE 0 END", node.node_id);
		await this.configureNodeTunnel(node);
		const hostname = `auth.${this.env.DOMAIN}`;
		await this.ensureDNS(hostname, node.tunnel_id);
		return this.json({ node_id: node.node_id, node_name: node.node_name, hostname, status: "ready" });
	}

	private async configureNodeTunnel(node: NodeRow): Promise<void> {
		const apps = [...this.ctx.storage.sql.exec(
			"SELECT hostname FROM apps WHERE node_id = ? ORDER BY hostname", node.node_id,
		)].map((row) => String(row.hostname));
		const stored = this.first<{ auth_owner: number }>("SELECT auth_owner FROM nodes WHERE node_id = ?", node.node_id);
		await this.configureTunnel(node.tunnel_id, node.node_name, apps, stored?.auth_owner === 1);
	}

  private async createTunnel(name: string): Promise<{ id: string }> {
    const existing = await this.cloudflare<Array<{ id: string; name: string; config_src?: string }>>(
      `/accounts/${this.env.CLOUDFLARE_ACCOUNT_ID}/cfd_tunnel?name=${encodeURIComponent(name)}&is_deleted=false`,
    );
    if (existing.length === 1) {
		if (existing[0].config_src !== "cloudflare") throw new Error("existing tunnel is not remotely managed by NSL");
		return existing[0];
	}
    if (existing.length > 1) throw new Error("multiple tunnels use the requested node name");
    return this.cloudflare(`/accounts/${this.env.CLOUDFLARE_ACCOUNT_ID}/cfd_tunnel`, {
      method: "POST",
      body: JSON.stringify({ name, config_src: "cloudflare" }),
    });
  }

  private async configureTunnel(tunnelID: string, nodeName: string, appHostnames: string[], authOwner = false): Promise<void> {
    const hostnames = [`t--${nodeName}.${this.env.DOMAIN}`, ...appHostnames];
	if (authOwner) hostnames.push(`auth.${this.env.DOMAIN}`);
	hostnames.sort();
    await this.configureIngress(tunnelID, hostnames);
  }

  private async configureIngress(tunnelID: string, hostnames: string[]): Promise<void> {
    const ingress = hostnames.map((hostname) => ({ hostname, service: "http://traefik:80" }));
    ingress.push({ service: "http_status:404" } as { hostname: string; service: string });
    await this.cloudflare(`/accounts/${this.env.CLOUDFLARE_ACCOUNT_ID}/cfd_tunnel/${tunnelID}/configurations`, {
      method: "PUT",
      body: JSON.stringify({ config: { ingress } }),
    });
  }

  private async ensurePortalTunnel(): Promise<{ id: string; token: string }> {
    const tunnel = await this.createTunnel("nsl-portal");
    const hostname = `apps.${this.env.DOMAIN}`;
    await this.configureIngress(tunnel.id, [hostname]);
    await this.ensureDNS(hostname, tunnel.id);
    const token = await this.cloudflare<string>(
      `/accounts/${this.env.CLOUDFLARE_ACCOUNT_ID}/cfd_tunnel/${tunnel.id}/token`,
    );
    return { id: tunnel.id, token };
  }

  private async ensureDNS(hostname: string, tunnelID: string): Promise<void> {
    const records = await this.cloudflare<Array<{ id: string; type: string; content: string; comment?: string }>>(
      `/zones/${this.env.CLOUDFLARE_ZONE_ID}/dns_records?name=${encodeURIComponent(hostname)}`,
    );
    const desired = `${tunnelID}.cfargotunnel.com`;
    if (records.length > 0) {
      if (records.length === 1 && records[0].type === "CNAME" && records[0].content === desired && records[0].comment === "managed-by=nsl-broker") return;
      throw new Error(`DNS record ${hostname} already exists and is not managed by this node`);
    }
    await this.cloudflare(`/zones/${this.env.CLOUDFLARE_ZONE_ID}/dns_records`, {
      method: "POST",
      body: JSON.stringify({ type: "CNAME", name: hostname, content: desired, proxied: true, ttl: 1, comment: "managed-by=nsl-broker" }),
    });
  }

  private async deleteDNS(hostname: string, tunnelID: string): Promise<void> {
    const records = await this.cloudflare<Array<{ id: string; type: string; content: string; comment?: string }>>(
      `/zones/${this.env.CLOUDFLARE_ZONE_ID}/dns_records?name=${encodeURIComponent(hostname)}`,
    );
    const desired = `${tunnelID}.cfargotunnel.com`;
    for (const record of records) {
      if (record.type !== "CNAME" || record.content !== desired || record.comment !== "managed-by=nsl-broker") continue;
      await this.cloudflare(`/zones/${this.env.CLOUDFLARE_ZONE_ID}/dns_records/${record.id}`, { method: "DELETE" });
    }
  }

  private async adoptExactDNS(hostname: string, tunnelID: string): Promise<void> {
    const records = await this.cloudflare<Array<{ id: string; type: string; proxied?: boolean }>>(
      `/zones/${this.env.CLOUDFLARE_ZONE_ID}/dns_records?name=${encodeURIComponent(hostname)}`,
    );
    if (records.length !== 1 || records[0].type !== "CNAME" || records[0].proxied !== true) {
      throw new Error(`DNS record ${hostname} is not one proxied CNAME`);
    }
    await this.cloudflare(`/zones/${this.env.CLOUDFLARE_ZONE_ID}/dns_records/${records[0].id}`, {
      method: "PUT",
      body: JSON.stringify({
        type: "CNAME", name: hostname, content: `${tunnelID}.cfargotunnel.com`,
        proxied: true, ttl: 1, comment: "managed-by=nsl-broker",
      }),
    });
  }

  private async cloudflare<T>(path: string, init: RequestInit = {}): Promise<T> {
    const response = await fetch(`https://api.cloudflare.com/client/v4${path}`, {
      ...init,
      headers: { "Authorization": `Bearer ${this.env.CLOUDFLARE_API_TOKEN}`, "Content-Type": "application/json", ...init.headers },
    });
    const body = await response.json() as CloudflareResponse<T>;
    if (!response.ok || !body.success) {
      throw new Error(`Cloudflare operation failed (${response.status}, ${body.errors?.[0]?.code ?? "unknown"})`);
    }
    return body.result;
  }

  private first<T>(query: string, ...bindings: unknown[]): T | undefined {
    return [...this.ctx.storage.sql.exec(query, ...bindings)][0] as T | undefined;
  }

  private async body(request: Request): Promise<unknown> {
    if (!request.headers.get("Content-Type")?.startsWith("application/json")) throw new Error("Content-Type must be application/json");
    return request.json();
  }

  private async bearerMatches(request: Request, expected: string): Promise<boolean> {
    return this.bearerHashMatches(request, await sha256(expected));
  }

  private async bearerHashMatches(request: Request, expectedHash: string): Promise<boolean> {
    const header = request.headers.get("Authorization") ?? "";
    if (!header.startsWith("Bearer ")) return false;
    return timingSafeEqual(await sha256(header.slice(7)), expectedHash);
  }

  private json(value: unknown, status = 200): Response {
    return new Response(JSON.stringify(value), { status, headers: jsonHeaders });
  }

  private error(status: number, code: string, message: string): Response {
    return this.json({ error: { code, message } }, status);
  }
}

function randomToken(bytes: number): string {
  const value = crypto.getRandomValues(new Uint8Array(bytes));
  return btoa(String.fromCharCode(...value)).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
}

async function sha256(value: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value));
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}

async function derivedCredential(key: string, nodeID: string): Promise<string> {
  const cryptoKey = await crypto.subtle.importKey("raw", new TextEncoder().encode(key), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const signature = await crypto.subtle.sign("HMAC", cryptoKey, new TextEncoder().encode(nodeID));
  return `nsln_${[...new Uint8Array(signature)].map((byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

function timingSafeEqual(left: string, right: string): boolean {
  if (left.length !== right.length) return false;
  let difference = 0;
  for (let index = 0; index < left.length; index++) difference |= left.charCodeAt(index) ^ right.charCodeAt(index);
  return difference === 0;
}
