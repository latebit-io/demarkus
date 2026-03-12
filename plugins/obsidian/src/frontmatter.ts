import { App, TFile } from "obsidian";

const MARK_URL_KEY = "mark-url";
const MARK_VERSION_KEY = "mark-version";
const MARK_ETAG_KEY = "mark-etag";

export interface MarkFrontmatter {
  url: string;
  version: number;
  etag: string;
}

export function getMarkFrontmatter(
  app: App,
  file: TFile
): MarkFrontmatter | null {
  const cache = app.metadataCache.getFileCache(file);
  const fm = cache?.frontmatter;
  if (!fm || !fm[MARK_URL_KEY]) return null;

  const rawVersion = fm[MARK_VERSION_KEY];
  const parsedVersion = parseInt(
    rawVersion !== undefined && rawVersion !== null && rawVersion !== ""
      ? String(rawVersion)
      : "0",
    10
  );
  const version = Number.isFinite(parsedVersion) ? parsedVersion : 0;

  return {
    url: fm[MARK_URL_KEY],
    version,
    etag: fm[MARK_ETAG_KEY] || "",
  };
}

export function buildFrontmatter(url: string, version: number, etag: string): string {
  const lines = [
    "---",
    `${MARK_URL_KEY}: ${JSON.stringify(url)}`,
    `${MARK_VERSION_KEY}: ${version}`,
  ];
  if (etag) {
    lines.push(`${MARK_ETAG_KEY}: ${JSON.stringify(etag)}`);
  }
  lines.push("---", "");
  return lines.join("\n");
}

export function stripExistingFrontmatter(content: string): string {
  if (!content.startsWith("---")) return content;
  const end = content.indexOf("\n---", 3);
  if (end === -1) return content;
  return content.substring(end + 4).replace(/^\r?\n/, "");
}

const YAML_UNSAFE_KEY = /[:#\[\]{}&*!|>'"%@`?,\-]|^\s|\s$/;

function formatYamlKey(key: string): string {
  if (YAML_UNSAFE_KEY.test(key)) return JSON.stringify(key);
  return key;
}

function formatYamlValue(value: any): string {
  if (typeof value === "string") return JSON.stringify(value);
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  if (Array.isArray(value)) return JSON.stringify(value);
  if (typeof value === "object") return JSON.stringify(value);
  return JSON.stringify(String(value));
}

export function buildMergedFrontmatter(
  url: string,
  version: number,
  etag: string,
  existingFrontmatter?: Record<string, any>
): string {
  // Start with existing frontmatter or empty object
  const merged: Record<string, any> = existingFrontmatter
    ? { ...existingFrontmatter }
    : {};

  // Update Demarkus keys (remove old ones that shouldn't exist)
  delete merged[MARK_URL_KEY];
  delete merged[MARK_VERSION_KEY];
  delete merged[MARK_ETAG_KEY];

  // Add new Demarkus keys
  merged[MARK_URL_KEY] = url;
  merged[MARK_VERSION_KEY] = version;
  if (etag) {
    merged[MARK_ETAG_KEY] = etag;
  }

  // Obsidian's metadata cache includes internal keys we shouldn't write back
  delete merged["position"];

  const lines = ["---"];
  for (const [key, value] of Object.entries(merged)) {
    if (value === null || value === undefined) continue;
    lines.push(`${formatYamlKey(key)}: ${formatYamlValue(value)}`);
  }
  lines.push("---", "");
  return lines.join("\n");
}
