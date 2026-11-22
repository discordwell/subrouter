# Agent skills

Two Claude Code skills:

- `subrouter/SKILL.md` is for agents that inspect or operate a subrouter install. It covers the traps in `sr`, the JSON routes and `srq`, how to read the meters, held turns, and saved resets.
- `revive-workflow-agents/SKILL.md` continues Claude Code workflow agents that died mid-work, for example when the pool was unreachable, from their saved transcripts.

## Install a skill

From inside a checkout of this repository, link the skill into your Claude Code skills directory. For the other skill, replace `subrouter` with `revive-workflow-agents` throughout:

```bash
mkdir -p ~/.claude/skills
skill="$(git rev-parse --show-toplevel)/ops/skills/subrouter"
if [ ! -f "$skill/SKILL.md" ]; then
  echo "Run this from inside a checkout of this repository."
elif [ -e ~/.claude/skills/subrouter ] || [ -L ~/.claude/skills/subrouter ]; then
  echo "~/.claude/skills/subrouter already exists. Move it aside, then run this again."
else
  ln -s "$skill" ~/.claude/skills/subrouter
fi
```

The checks matter. Outside a checkout, the link would point at a path that does not exist. On an existing directory, or on a link to one, `ln -s` puts the new link inside that directory and exits 0.

The link follows the checkout, so a `git pull` updates the skill. Start a new Claude Code session. The skill loads when a task touches subrouter, `sr`, `srq`, or the `/_subrouter/*` routes, and `/subrouter` runs it directly.

## Test the skills

```bash
(cd ops && go test -count=1 ./skills)
python3 ops/skills/revive-workflow-agents/test_wf_revive.py
```

The Go test checks the subrouter skill. It fails when the skill names a repository path, a PATCHES section, a route, an environment variable, an `srq` subcommand or flag, an `sr` flag, a table column, or a quoted string that the tree does not have. Each `sr` verb must appear quoted in the CLI's source. A verb path such as `sr claude reset` must start a line of a string literal there, as a help listing does, or follow `usage:`. A comment or an error message that mentions it does not count. The test reads text only, so it cannot prove that a verb path dispatches.
