# Privacy hooks

The git hooks in this directory refuse a commit or a push that adds a private value of your machine, such as an account email from your subrouter pool, a tailnet address or host name, or your login name. On a hit, the hook prints only where the value is: `file:line`, a new file's path with the value written as `<private>`, or `commit message`. It never prints the value.

The hooks read the values from your own machine each time they run, so the repo keeps no list of them.

## Install the hooks

`core.hooksPath` is one setting for every worktree of a clone. Point it at `ops/hooks` in the clone's main checkout, the directory that `git clone` made, and keep that checkout on a branch that has `ops/hooks`, such as `main`. In the main checkout, run:

```sh
git config core.hooksPath "$(git rev-parse --show-toplevel)/ops/hooks"
```

Do not run this command in a worktree that `git worktree add` made. When you remove that worktree, the path stops working.

If `core.hooksPath` names a directory that does not exist, Git runs no hooks at all, and that includes the hooks in `.git/hooks`. To see the directory that Git uses, run `git rev-parse --git-path hooks`. That directory must hold `pre-commit`, `commit-msg` and `pre-push`.

If your clone has only one worktree, a relative path also works:

```sh
git config core.hooksPath ops/hooks
```

Git resolves a relative path in each worktree, so in a clone with more than one worktree, a worktree whose branch has no `ops/hooks` runs no hooks.

The hooks need `python3` and Git 2.31 or later. To remove them, run `git config --unset core.hooksPath`.

## Keep your hooks in .git/hooks

`core.hooksPath` stops Git from running the hooks in `.git/hooks`. So each hook here first runs the clone's own `.git/hooks/pre-commit`, `.git/hooks/commit-msg` or `.git/hooks/pre-push`, when that file exists and is executable, with the same arguments and input. If that hook fails, Git stops and the privacy check does not run.

Other hooks in `.git/hooks`, such as `post-commit`, do not run while `core.hooksPath` is set.

A hook in `.git/hooks` can also run a hook here, as a link to it or as a script that runs it, with or without `core.hooksPath`. The hooks run once either way. Without `core.hooksPath`, Git has already run the hook in `.git/hooks`, so the hook here does not run it again. With `core.hooksPath`, the hook here runs the hook in `.git/hooks` with `SUBROUTER_PRIVACY_HOOK` set to that hook's path. A hook here that finds that path in the variable does not run the hook in `.git/hooks` again.

## What each hook checks

| Hook | What it checks |
|---|---|
| `pre-commit` | The lines that the staged diff adds, and the path of each new file. For a merge, only the lines and new files that none of its parents has. |
| `commit-msg` | The commit message as Git stores it. After an editor, Git removes comment lines and the text below the scissors line (`>8`), so the hook skips them. With `git commit -m` or `-F`, Git keeps them, so the hook checks them. If `commit.cleanup` or `--cleanup` makes Git keep a line that the hook skips, `pre-push` checks it in the stored message. |
| `pre-push` | Each pushed commit that the remote does not have: the lines it adds, the path of each new file, and its message. The message of each pushed annotated tag. The name of each branch or tag that the push adds to the remote. |

`pre-push` treats a commit as on the remote when the remote's tip of the pushed branch holds it, or when one of your remote-tracking branches of that remote holds it, such as `origin/main`. So a rebased branch's push checks only the branch's own commits. If the remote drops a commit, run `git fetch --prune` to update your remote-tracking branches. `pre-push` checks each commit separately, so it refuses a value that a later commit removes, because the value stays in the history. To fix that, rewrite the commit.

A merge adds only the lines that none of its parents has, such as the lines that resolve a conflict. So when you merge the remote's `main` into your branch, `pre-push` does not check the lines of that `main` again. It checks the commits of each parent that the remote does not have.

The hooks skip the lines of a binary file, one whose added lines hold a NUL byte, as `public-scrub --check` skips a file that holds one. They still check a binary file's path, and they check a text file that `.gitattributes` marks as binary.

A trailer that names the commit's own author or committer, such as `Signed-off-by: Name <you@example.com>`, passes. The commit header already holds that email.

## Where the values come from

Each source is optional. If a source is missing or fails, the hooks use the others. A source that does not answer in time also counts as missing. The hooks wait up to 10 seconds for `tailscale`. For the daemon, they wait up to 1 second for a connection, then up to 5 seconds each for the accounts and for usage-status, and up to 8 seconds for the peers. With no values at all, the hooks pass and print nothing.

`$SUBROUTER_ADDR` can be `host:port`, the form that the systemd environment file and the scripts in `ops/deploy/macos` use, or a URL, the form that `ops/bin/srq` reads.

| Source | Values |
|---|---|
| The subrouter daemon at `$SUBROUTER_ADDR`, default `127.0.0.1:31415` | The `id`, `label` and `email` of each account in `GET /_subrouter/accounts`. The `account_identity` of each row in `GET /_subrouter/usage-status?snapshot=1`. The name, URL host and accounts of each peer in `GET /_subrouter/peers/usage-status`. |
| `tailscale status --json` | The addresses, `HostName`, `DNSName` and first `DNSName` label of this node and of each peer. The MagicDNS suffix, the tailnet's name, and the `LoginName` of each user. |
| `git config user.email` | The email and its local part. |
| Your login | `$LOGNAME`, `$USER`, `$HOME`, and the login name and home directory from the password database. |
| `~/.config/subrouter/private-values` | One value on each line. A line that starts with `#` is a comment. Add the values that no other source knows, such as the name of a former employer or of a person. |
| The `ops/bin/public-scrub` map, at `$SUBROUTER_PUBLIC_SCRUB_MAP` or else `~/.config/subrouter-public-scrub/map.tsv` | The real value of each row (`kind TAB real TAB placeholder`). |

The hooks run the `tailscale` CLI that `$SUBROUTER_TAILSCALE_BIN` names. If that variable is not set, they take the first of `tailscale` on `PATH`, `/opt/homebrew/bin/tailscale`, `/usr/local/bin/tailscale` and the CLI in `/Applications/Tailscale.app`, the places where subrouter itself looks. A Git client that an app starts often has a short `PATH`, so the fixed places matter. If `$SUBROUTER_TAILSCALE_BIN` names a CLI that does not run, the hooks leave tailscale out and do not look elsewhere.

The hooks keep the answers of the daemon and of tailscale for 10 minutes in `~/.cache/subrouter-hooks`, in files of mode 600. They also keep the words that the base holds there, until the base or the values change. To read the sources again at once, delete that directory.

## How a value matches

The hooks use the matcher of `ops/bin/public-scrub`, so they find what `public-scrub --check` finds: a value in any case, a value at a camelCase boundary, the local part of an email on its own, and an IP address written with hyphens or URL-encoded separators.

Any case includes letters outside ASCII, so `É` in a value matches `é` in the text, and the reverse. A letter whose lower case has another length in UTF-8, such as `İ`, matches only in the case that the value gives.

The hooks find the kind of each value from a live source or from `private-values` by its form. The kind is an IP address, an email (the value holds `@`), a domain name, or a label.

A value that names no one would refuse ordinary text. So the hooks skip each of these:

- A value shorter than 4 characters.
- A word in the `GENERIC` list in `privacy-check`, such as `user`, `admin`, `localhost` and `laptop`, also with a number after a hyphen. Tailscale tells devices of the same name apart that way, as in `macbook-pro-2`.
- A home directory whose last part is one of those, such as `/root`, `/home/ubuntu` or `/home/user`.
- A value of one word, letters and digits only, that the base already holds. Such a word is common in this repo, or it is public already.

For `pre-commit` and `commit-msg`, the base is `origin`'s default branch (`refs/remotes/origin/HEAD`), else the current branch's upstream, else `HEAD`. For `pre-push`, the base comes only from the pushed remote, because another remote, such as a private one, can hold words that the pushed remote does not. It is that remote's default branch, else the remote's tip of a pushed branch, else the current branch's upstream if that upstream is on the pushed remote. If none of these exists, a push has no base, so no word passes for being in it. To give a remote a default branch, run `git remote set-head <remote> --auto`. A push never takes the local branch as its base, because the local branch holds the commits that the push checks.

The hooks apply these rules to each form of a value separately. For example, if your email is `user@` followed by a private domain, the word `user` passes, but the full email does not.

## Placeholders pass

The hooks refuse only the values that a source reports, so a placeholder such as `100.64.0.1` or `fd7a:115c:a1e0::10:1` passes unless a source reports that address. A tailnet address that `tailscale` reports is always refused.

A value that is never real passes even when a source reports it:

- Addresses in `192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24` and `2001:db8::/32`, and loopback, link-local and unspecified addresses.
- Emails and domain names under `example.com`, `example.net`, `example.org`, `.example`, `.invalid`, `.localhost` and `.test`.
- The machine names `box` and `laptop`.

When a hook names a location, write a placeholder in place of the value there.

## Skip the hooks once

`git commit --no-verify` and `git push --no-verify` skip the hooks for one command.

If a hook fails with an error, it refuses the change and names only the type of the error, because a traceback can quote a value.

## Test the hooks

```sh
cd ops && go test -count=1 ./hooks
```

The tests run real git commands in a sandbox with a fake HOME and git config, a fake daemon, a fake `tailscale` at `$SUBROUTER_TAILSCALE_BIN`, and a bare remote. No test reads the daemon, tailscale, `private-values` file or map of the machine that runs it. The hooks always read the login name and home directory from the password database of the machine that runs them, so both are values in every test, and the tests need a temporary directory whose path holds neither.
