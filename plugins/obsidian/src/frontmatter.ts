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

  return {
    url: fm[MARK_URL_KEY],
    version: parseInt(fm[MARK_VERSION_KEY] || "0", 10),
    etag: fm[MARK_ETAG_KEY] || "",
  };
}

export function buildFrontmatter(url: string, version: number, etag: string): string {
  const lines = [
    "---",
    `${MARK_URL_KEY}: "${url}"`,
    `${MARK_VERSION_KEY}: ${version}`,
  ];
  if (etag) {
    lines.push(`${MARK_ETAG_KEY}: "${etag}"`);
  }
  lines.push("---", "");
  return lines.join("\n");
}

export function stripExistingFrontmatter(content: string): string {
  if (!content.startsWith("---")) return content;
  const end = content.indexOf("\n---", 3);
  if (end === -1) return content;
  return content.substring(end + 4).replace(/^\n/, "");
}
