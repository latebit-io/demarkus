import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, mkdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

test("packed memory plugin runs from a clean home without an installer or runtime dependencies", () => {
  const root = mkdtempSync(join(tmpdir(), "demarkus memory package "));
  const home = join(root, "home");
  mkdirSync(home);
  try {
    const packed = JSON.parse(execFileSync("npm", ["pack", "--json", "--ignore-scripts", "--pack-destination", root], {
      cwd: join(import.meta.dirname, ".."), encoding: "utf8",
    }))[0];
    const paths = packed.files.map((file: { path: string }) => file.path);
    for (const path of ["index.ts", "src/package.ts", "context/session-guidance.md", "commands/soul.md", "scripts/bootstrap.sh", "skills/remember/SKILL.md"]) {
      assert.ok(paths.includes(path), `package includes ${path}`);
    }
    assert.ok(!paths.includes("install.sh"));
    assert.ok(!paths.some((path: string) => path.startsWith("tests/")));
    execFileSync("tar", ["-xzf", join(root, packed.filename), "-C", root]);
    const childEnv = { ...process.env };
    delete childEnv.NODE_TEST_CONTEXT;
    const output = execFileSync(process.execPath, ["--test", "--test-reporter=tap", join(import.meta.dirname, "installed.ts")], {
      env: { ...childEnv, HOME: home, XDG_CONFIG_HOME: join(home, "config"), DEMARKUS_TEST_PLUGIN: join(root, "package", "index.ts") },
      encoding: "utf8", stdio: "pipe", timeout: 60_000,
    });
    assert.match(output, /^# tests [1-9]\d*$/m, "installed-package assertions must run");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
