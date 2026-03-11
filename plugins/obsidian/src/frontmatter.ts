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
