import { execFile } from "child_process";
import { delimiter } from "path";

export interface FetchResult {
  status: string;
  version: number;
  etag: string;
  modified: string;
  body: string;
  metadata: Record<string, string>;
}

export interface ListEntry {
  name: string;
  isDir: boolean;
}

export interface CliOptions {
  cliPath: string;
  insecure: boolean;
  token?: string;
}

function buildArgs(
  opts: CliOptions,
  verb: string,
  url: string,
  extra?: string[]
): string[] {
  const args = ["-v", "-X", verb];
  if (opts.insecure) args.push("-insecure");
  if (extra) args.push(...extra);
  args.push(url);
  return args;
}

function parseMetadataLine(line: string): {
  status: string;
  metadata: Record<string, string>;
} {
  const metadata: Record<string, string> = {};
  let status = "unknown";

  const statusMatch = line.match(/^\[([^\]]+)\]/);
  if (statusMatch) {
    status = statusMatch[1];
  }

  const rest = line.replace(/^\[[^\]]+\]\s*/, "").replace(/\s*\(cached\)\s*$/, "");
  for (const pair of rest.split(/\s+/)) {
    const eq = pair.indexOf("=");
    if (eq > 0) {
      metadata[pair.substring(0, eq)] = pair.substring(eq + 1);
    }
  }

  return { status, metadata };
}

function exec(
  cliPath: string,
  args: string[],
  stdin?: string,
  token?: string
): Promise<{ stdout: string; stderr: string }> {
  return new Promise((resolve, reject) => {
    const pathParts: string[] = [];
    if (process.env.PATH) pathParts.push(process.env.PATH);
    if (process.platform !== "win32") {
      pathParts.push("/usr/local/bin", "/opt/homebrew/bin");
    }
    const homeDir = process.env.HOME || process.env.USERPROFILE;
    if (homeDir) {
      pathParts.push(`${homeDir}/.local/bin`, `${homeDir}/go/bin`);
    }
    const env: Record<string, string> = {
      ...process.env,
      PATH: pathParts.join(delimiter),
    };
    if (token) {
      env.DEMARKUS_AUTH = token;
    }
    const child = execFile(
      cliPath,
      args,
      { maxBuffer: 2 * 1024 * 1024, env },
      (error, stdout, stderr) => {
        if (error) {
          reject(new Error(stderr.trim() || error.message));
          return;
        }
        resolve({ stdout, stderr });
      }
    );
    if (stdin && child.stdin) {
      child.stdin.write(stdin);
      child.stdin.end();
    }
  });
}

export async function fetch(
  opts: CliOptions,
  url: string
): Promise<FetchResult> {
  const args = buildArgs(opts, "FETCH", url);
  const { stdout, stderr } = await exec(opts.cliPath, args, undefined, opts.token);

  const { status, metadata } = parseMetadataLine(stderr.trim());

  return {
    status,
    version: parseInt(metadata["version"] || "0", 10),
    etag: metadata["etag"] || "",
    modified: metadata["modified"] || "",
    body: stdout,
    metadata,
  };
}

export async function list(
  opts: CliOptions,
  url: string
): Promise<ListEntry[]> {
  const args = buildArgs(opts, "LIST", url);
  const { stdout } = await exec(opts.cliPath, args, undefined, opts.token);

  const entries: ListEntry[] = [];
  for (const line of stdout.split("\n")) {
    const match = line.match(/^- \[([^\]]+)\]\(([^)]+)\)/);
    if (match) {
      const name = match[2];
      entries.push({ name, isDir: name.endsWith("/") });
    }
  }
  return entries;
}

export async function publish(
  opts: CliOptions,
  url: string,
  body: string,
  expectedVersion: number
): Promise<FetchResult> {
  const args = buildArgs(opts, "PUBLISH", url, [
    "-expected-version",
    String(expectedVersion),
  ]);
  const { stdout, stderr } = await exec(opts.cliPath, args, body, opts.token);

  const { status, metadata } = parseMetadataLine(stderr.trim());

  return {
    status,
    version: parseInt(metadata["version"] || "0", 10),
    etag: metadata["etag"] || "",
    modified: metadata["modified"] || "",
    body: stdout,
    metadata,
  };
}

export async function detectCli(): Promise<string | null> {
  const candidates = ["demarkus", "/usr/local/bin/demarkus"];
  for (const candidate of candidates) {
    try {
      await exec(candidate, ["--help"]);
      return candidate;
    } catch {
      continue;
    }
  }
  return null;
}
