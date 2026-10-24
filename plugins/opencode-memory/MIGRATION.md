# Move script installs to OpenCode packages

Only needed if you previously ran either OpenCode plugin's `install.sh`. Fresh installs use `opencode plugin add` directly.

Close OpenCode first. Archive the old adapters and skills outside its discovery directories. The following Bash block handles both plugins and respects `XDG_CONFIG_HOME`. It preserves all moved files in a new backup directory; it does not touch your soul, binaries, joined systems or OAuth credentials.

```bash
set -e
config="${XDG_CONFIG_HOME:-$HOME/.config}/opencode"
mkdir -p "$HOME/.demarkus"
backup=$(mktemp -d "$HOME/.demarkus/opencode-script-backup.XXXXXX")
mkdir -p "$backup/plugins" "$backup/skills" "$backup/commands"
for name in demarkus-memory demarkus-knowledge; do
  file="$config/plugins/$name.ts"
  if [ -e "$file" ] || [ -L "$file" ]; then mv "$file" "$backup/plugins/"; fi
done
for name in remember knowledge-promote; do
  dir="$config/skills/$name"
  if [ -d "$dir" ]; then mv "$dir" "$backup/skills/"; fi
done
for source in "$HOME/.demarkus/opencode-memory/commands/"*.md; do
  [ -f "$source" ] || continue
  file="$config/commands/$(basename "$source")"
  if [ -f "$file" ] && cmp -s "$source" "$file"; then
    mv "$file" "$backup/commands/"
  fi
done
echo "Old integration files saved in $backup"
```

Edited commands remain active and override package commands. If you customized either archived skill, review the backup and restore your customized skill after migration; user skills take precedence. Very old installs may also have `skills/soul-memory` or `skills/memory`; review and archive those separately if they belong to demarkus.

Add the plugins you use:

```bash
opencode plugin add 'github:latebit-io/demarkus#main::path:plugins/opencode-memory'
opencode plugin add 'github:latebit-io/demarkus#main::path:plugins/opencode-knowledge'
```

Start OpenCode and verify `/soul-status` and `/knowledge` as applicable. The package detects leftover flat-file adapters and reports their path instead of knowingly activating duplicate hooks. Old asset directories under `~/.demarkus/opencode-memory` and `~/.demarkus/opencode-knowledge` are no longer used; keep them with your backup until migration is verified.

Package removal uses `opencode plugin remove` with the configured package target. Restore the archived integration files only after removing the packages if you need to roll back.
