export function slugify(value: string): string {
  return value.trim().toLowerCase().replace(/[^a-z0-9-]+/g, "-").replace(/^-+|-+$/g, "");
}

export function requireSlug(value: unknown, field: string): string {
  if (typeof value !== "string") throw new Error(`${field} is required`);
  const slug = slugify(value);
  if (!slug || slug !== value || slug.length > 30) {
    throw new Error(`${field} must be a lowercase DNS label`);
  }
  return slug;
}

export function parseEnrollmentToken(value: unknown): { id: string; secret: string } {
  if (typeof value !== "string") throw new Error("enrollment_token is required");
  const parts = value.split(".");
  if (parts.length !== 3 || parts[0] !== "nsl1" || !parts[1] || !parts[2]) {
    throw new Error("enrollment_token is invalid");
  }
  return { id: parts[1], secret: parts[2] };
}

export function appHostname(app: string, node: string, domain: string): string {
  const appSlug = requireSlug(app, "app_name");
  const nodeSlug = requireSlug(node, "node_name");
  if (appSlug === "t") throw new Error("app_name t is reserved for the node terminal");
  if (appSlug.length + nodeSlug.length + 2 > 63) throw new Error("app and node names exceed the DNS label limit");
  return `${appSlug}--${nodeSlug}.${domain}`;
}
