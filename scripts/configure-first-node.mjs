import { copyFileSync, existsSync, mkdirSync, readFileSync, writeFileSync, chmodSync } from "node:fs";
import { randomBytes } from "node:crypto";
import { dirname, resolve } from "node:path";

const root = resolve(import.meta.dirname, "..");
const nodeName = process.argv[2];
if (!nodeName || !/^[a-z0-9]([a-z0-9-]{0,28}[a-z0-9])?$/.test(nodeName)) {
  throw new Error("usage: node scripts/configure-first-node.mjs <lowercase-node-name>");
}

function readEnv(relativePath) {
  const path = resolve(root, relativePath);
  if (!existsSync(path)) return {};
  const values = {};
  for (const line of readFileSync(path, "utf8").split(/\r?\n/)) {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith("#")) continue;
    const separator = trimmed.indexOf("=");
    if (separator < 1) continue;
    let value = trimmed.slice(separator + 1).trim();
    if ((value.startsWith('"') && value.endsWith('"')) || (value.startsWith("'") && value.endsWith("'"))) {
      value = value.slice(1, -1);
    }
    values[trimmed.slice(0, separator).trim()] = value;
  }
  return values;
}

function encoded(value) {
  if (/^[A-Za-z0-9_./:+@=-]*$/.test(value)) return value;
  return JSON.stringify(value);
}

function writeEnv(relativePath, values) {
  const path = resolve(root, relativePath);
  mkdirSync(dirname(path), { recursive: true });
  if (existsSync(path)) {
    const suffix = new Date().toISOString().replaceAll(":", "").replaceAll(".", "");
    copyFileSync(path, `${path}.pre-distributed-${suffix}`);
  }
  const body = Object.entries(values).map(([key, value]) => `${key}=${encoded(String(value))}`).join("\n") + "\n";
  writeFileSync(path, body, { mode: 0o600 });
  chmodSync(path, 0o600);
}

function secret(bytes = 32) {
  return randomBytes(bytes).toString("hex");
}

function required(value, description) {
  if (!value || /^<.*>$/.test(value)) throw new Error(`${description} is missing from the existing configuration`);
  return value;
}

const currentRoot = readEnv(".env");
const currentRegistry = readEnv("registry/.env");
const currentBackup = readEnv("backup/.env");
const currentPostgres = readEnv("postgres/.env");
const currentKeycloak = readEnv("keycloak/.env");
const currentOAuth = readEnv("oauth2-proxy/.env");
const broker = readEnv(".broker-secrets.env");

const brokerURL = required(broker.NSL_BROKER_URL, "broker URL");
const brokerAdminToken = required(broker.NSL_BROKER_ADMIN_TOKEN, "broker admin token");
const authClaimToken = required(broker.NSL_AUTH_CLAIM_TOKEN, "auth claim token");
const awsAccessKey = required(currentRegistry.AWS_ACCESS_KEY_ID || currentBackup.AWS_ACCESS_KEY_ID, "AWS access key");
const awsSecretKey = required(currentRegistry.AWS_SECRET_ACCESS_KEY || currentBackup.AWS_SECRET_ACCESS_KEY, "AWS secret key");
const s3Bucket = required(currentRoot.REGISTRY_S3_BUCKET || currentBackup.BACKUP_S3_BUCKET, "S3 bucket");
const postgresPassword = required(currentPostgres.POSTGRES_PASSWORD, "PostgreSQL admin password");
const keycloakDBPassword = required(currentPostgres.KEYCLOAK_DB_PASSWORD || currentKeycloak.KC_DB_PASSWORD, "Keycloak database password");

const tokenResponse = await fetch(`${brokerURL}/v1/admin/enrollment-tokens`, {
  method: "POST",
  headers: { "Authorization": `Bearer ${brokerAdminToken}`, "Content-Type": "application/json" },
  body: JSON.stringify({ node_name: nodeName, ttl_seconds: 900 }),
});
if (!tokenResponse.ok) {
  throw new Error(`broker token request failed (${tokenResponse.status}): ${await tokenResponse.text()}`);
}
const enrollment = await tokenResponse.json();
if (!enrollment.enrollment_token) throw new Error("broker did not return an enrollment token");

const domain = currentRoot.DOMAIN || "joedodge.dev";
const region = currentRoot.AWS_REGION || currentBackup.AWS_REGION || "us-east-1";
const registryAPIToken = currentRoot.REGISTRY_API_TOKEN || secret();
const registryProxyToken = currentRoot.REGISTRY_PROXY_TOKEN || secret();
const oauthClientSecret = currentRoot.OAUTH2_CLIENT_SECRET_REGISTRY || currentOAuth.OAUTH2_PROXY_CLIENT_SECRET || secret();
const oauthCookieSecret = currentRoot.OAUTH2_COOKIE_SECRET || currentOAuth.OAUTH2_PROXY_COOKIE_SECRET || randomBytes(32).toString("base64url");
const keycloakAdminPassword = currentRoot.KEYCLOAK_ADMIN_PASSWORD || currentKeycloak.KEYCLOAK_ADMIN_PASSWORD || secret(18);
const keycloakUserPassword = currentRoot.KEYCLOAK_USER_PASSWORD || secret(18);
const backupAPIToken = currentBackup.BACKUP_API_TOKEN || secret();
const corporateCA = existsSync(resolve(root, "cloudflared/ca-bundle.pem"))
  ? "./cloudflared/ca-bundle.pem"
  : "./cloudflared/ca-bundle.empty.pem";

writeEnv(".env", {
  DOMAIN: domain,
  NODE_NAME: nodeName,
  REGISTRY_S3_BUCKET: s3Bucket,
  REGISTRY_S3_PREFIX: "nsl/registry/v1",
  AWS_REGION: region,
  ENROLLMENT_BROKER_URL: brokerURL,
  NSL_ENROLLMENT_TOKEN: enrollment.enrollment_token,
  REGISTRY_VERSION: "dev",
  COMPOSE_PROFILES: "auth",
  NSL_AUTH_OWNER: "true",
  NSL_AUTH_CLAIM_TOKEN: authClaimToken,
  REGISTRY_API_TOKEN: registryAPIToken,
  REGISTRY_PROXY_TOKEN: registryProxyToken,
  OAUTH2_CLIENT_SECRET_REGISTRY: oauthClientSecret,
  OAUTH2_COOKIE_SECRET: oauthCookieSecret,
  KEYCLOAK_USER_PASSWORD: keycloakUserPassword,
  KEYCLOAK_ADMIN_PASSWORD: keycloakAdminPassword,
  CORPORATE_CA_FILE: corporateCA,
  BACKUP_TARGETS_FILE_HOST: "./backup/targets.empty.json",
});

writeEnv("registry/.env", {
  AWS_ACCESS_KEY_ID: awsAccessKey,
  AWS_SECRET_ACCESS_KEY: awsSecretKey,
});

writeEnv("backup/.env", {
  BACKUP_S3_BUCKET: s3Bucket,
  AWS_REGION: region,
  AWS_ACCESS_KEY_ID: awsAccessKey,
  AWS_SECRET_ACCESS_KEY: awsSecretKey,
  KEYCLOAK_DB_PASSWORD: keycloakDBPassword,
  POSTGRES_ADMIN_PASSWORD: postgresPassword,
  POSTGRES_ADMIN_USER: currentPostgres.POSTGRES_USER || "admin",
  BACKUP_API_TOKEN: backupAPIToken,
  BACKUP_INTERVAL: currentBackup.BACKUP_INTERVAL || "1h",
  BACKUP_DIR: currentBackup.BACKUP_DIR || "/backups",
  NODE_IDENTITY_FILE: "/var/lib/nsl/node.json",
  BACKUP_TARGETS_FILE: "/etc/nsl/backup-targets.json",
});

writeEnv("postgres/.env", {
  POSTGRES_USER: currentPostgres.POSTGRES_USER || "admin",
  POSTGRES_PASSWORD: postgresPassword,
  KEYCLOAK_DB_PASSWORD: keycloakDBPassword,
});

writeEnv("keycloak/.env", {
  KC_DB: "postgres",
  KC_DB_URL_HOST: "postgres",
  KC_DB_URL_PORT: "5432",
  KC_DB_URL_DATABASE: "keycloak",
  KC_DB_USERNAME: "keycloak",
  KC_DB_PASSWORD: keycloakDBPassword,
});

console.log(`Configured auth-owner node ${nodeName}. Existing environment files were backed up before replacement.`);
console.log("Enrollment token expires in 15 minutes; start Compose now.");
