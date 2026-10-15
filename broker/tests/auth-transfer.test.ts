import { DatabaseSync } from "node:sqlite";
import { describe, expect, it, vi } from "vitest";
vi.mock("cloudflare:workers", () => ({ DurableObject: class { constructor(public ctx: unknown, public env: unknown) {} } }));
import { EnrollmentBroker } from "../src/index";

describe("auth relocation", () => {
  it("guards owners and DNS, retries interrupted transfers, and rolls back", async () => {
    const db = new DatabaseSync(":memory:");
    const sql = { exec(query: string, ...bindings: unknown[]) {
      if (!bindings.length && /^\s*CREATE/.test(query)) { db.exec(query); return []; }
      return db.prepare(query).all(...bindings as (string | number)[]);
    } };
    const state = { storage: { sql, transactionSync: (fn: () => void) => fn() } };
    const env = { CLOUDFLARE_API_TOKEN: "cf", BROKER_ADMIN_TOKEN: "operator", NODE_CREDENTIAL_KEY: "node", AUTH_CLAIM_TOKEN: "claim", DOMAIN: "example.test", CLOUDFLARE_ZONE_ID: "zone", CLOUDFLARE_ACCOUNT_ID: "account" };
    const broker = new EnrollmentBroker(state as never, env as never);
    for (const id of ["source", "destination"]) sql.exec("INSERT INTO nodes VALUES (?, ?, ?, ?, ?, ?, ?)", id, id, id+"-tunnel", id, "hash", id === "source" ? 1 : 0, 0);
    let content = "source-tunnel.cfargotunnel.com";
    let fail = false;
    vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => {
      if (fail && !url.includes("dns_records?")) { fail = false; throw new Error("interrupted"); }
      if (url.includes("dns_records?")) return Response.json({ success: true, result: [{ id: "dns", type: "CNAME", content, proxied: true, comment: "managed-by=nsl-broker" }] });
      if (url.endsWith("dns_records/dns")) content = JSON.parse(init?.body as string).content;
      return Response.json({ success: true, result: {} });
    }));
    const transfer = (expected: string, destination: string, token = "operator") => broker.fetch(new Request("https://broker/v1/admin/auth-owner", { method: "POST", headers: { Authorization: "Bearer "+token, "Content-Type": "application/json" }, body: JSON.stringify({ expected_owner: expected, destination }) }));
    try {
      expect((await transfer("source", "destination", "wrong")).status).toBe(401);
      expect((await transfer("stale", "destination")).status).toBe(409);
      content = "unmanaged.cfargotunnel.com";
      expect((await transfer("source", "destination")).status).toBe(409);
      content = "source-tunnel.cfargotunnel.com";
      fail = true;
      expect((await transfer("source", "destination")).status).toBe(400);
      expect((await transfer("source", "destination")).status).toBe(200);
      expect(content).toBe("destination-tunnel.cfargotunnel.com");
      expect((await transfer("source", "destination")).status).toBe(200);
      expect((await transfer("destination", "source")).status).toBe(200);
      expect(content).toBe("source-tunnel.cfargotunnel.com");
    } finally { vi.unstubAllGlobals(); db.close(); }
  });
});
