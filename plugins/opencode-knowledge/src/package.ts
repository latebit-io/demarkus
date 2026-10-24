// Canonical copy; scripts/check-identical-copies.sh checks the knowledge copy.
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import type { Plugin, Skill } from "@opencode/plugin";

export function assertPackageInstall(): void {
  const config = join(process.env.XDG_CONFIG_HOME || join(homedir(), ".config"), "opencode");
  for (const name of ["demarkus-memory", "demarkus-knowledge"]) {
    const legacy = join(config, "plugins", `${name}.ts`);
    if (existsSync(legacy)) {
      throw new Error(`Script-installed adapter still present: ${legacy}. Move it outside OpenCode's plugins directory before loading the package; see the plugin README's migration instructions.`);
    }
  }
}

function markdown(path: string) {
  const raw = readFileSync(path, "utf8");
  const match = raw.match(/^---\r?\n([\s\S]*?)\r?\n---\r?\n/);
  if (!match) throw new Error(`Missing generated prompt frontmatter: ${path}`);
  const field = (name: string) => match[1].match(new RegExp(`^${name}:\\s*(.+)$`, "m"))?.[1]
    ?.trim().replace(/^(["'])(.*)\1$/, "$2");
  return { field, body: raw.slice(match[0].length).trim() };
}

export function loadCommands(root: string): Record<string, { description: string; template: string }> {
  const commands: Record<string, { description: string; template: string }> = {};
  for (const file of readdirSync(join(root, "commands")).filter((file) => file.endsWith(".md"))) {
    commands[file.slice(0, -3)] = loadCommand(file, join(root, "commands"), join(root, "scripts"));
  }
  return commands;
}

export function loadCommand(file: string, commandsDir: string, scriptsDir: string) {
  const { field, body } = markdown(join(commandsDir, file));
  const description = field("description");
  if (!description) throw new Error(`Missing command description: ${file}`);
  return {
    description,
    template: `${body.replaceAll("${DEMARKUS_SCRIPTS}", () => scriptsDir)}\n\nUser arguments (may be empty): $ARGUMENTS\n`,
  };
}

export async function registerPrompts(ctx: Plugin.Context, root: string): Promise<void> {
  const commands = loadCommands(root);
  const existing = new Set((await ctx.command.list()).data.map((command) => command.name));
  await ctx.command.transform((editor) => {
    for (const [name, command] of Object.entries(commands)) {
      if (existing.has(name)) continue;
      editor.add({
        name,
        description: command.description,
        execute: async ({ sessionID, prompt, delivery }) => {
          // Template expansion invalidates composer mention offsets.
          await ctx.session.prompt({
            ...prompt,
            files: prompt.files?.map(({ mention: _mention, ...file }) => file),
            agents: prompt.agents?.map(({ mention: _mention, ...agent }) => agent),
            skills: prompt.skills?.map(({ mention: _mention, ...skill }) => skill),
            sessionID,
            text: command.template.replaceAll("$ARGUMENTS", () => prompt.text),
            delivery,
          });
        },
      });
    }
  });
  const skills = readdirSync(join(root, "skills"), { withFileTypes: true })
    .filter((entry) => entry.isDirectory()).map((entry): Skill.Info => {
      const path = join(root, "skills", entry.name, "SKILL.md");
      const { field, body } = markdown(path);
      const name = field("name");
      const description = field("description");
      if (!name || !description) throw new Error(`Missing skill name or description: ${path}`);
      return { id: name as Skill.ID, name: name as Skill.Name, description, path: path as Skill.Info["path"], content: body };
    });
  await ctx.skill.transform((editor) => {
    for (const skill of skills) {
      if (!editor.list().some((existing) => existing.name === skill.name)) editor.add(skill);
    }
  });
}
