> [!WARNING]
> This is alpha software. It reads Claude Code's internal session files, whose format can change with any Claude Code update, and it moves and deletes those files. It may break, show wrong data or delete more than you expect. Back up `~/.claude` before deleting sessions you care about.

# claude-resume
List all your claude sessions and resume them in the folder they were started in.

![claude-resume picker: moving through sessions with a preview, filtering, and deleting a session](demo/demo.gif)

## Install

macOS and Linux:

```
curl -fsSL https://raw.githubusercontent.com/csmarvz/claude-resume/main/install.sh | bash
```

Windows (PowerShell):

```
irm https://raw.githubusercontent.com/csmarvz/claude-resume/main/install.ps1 | iex
```

`claude-resume` is a single program with no dependencies. The installers download the build for your system from the latest GitHub release: on macOS and Linux into `~/.local/bin` (set `PREFIX` to install into `$PREFIX/bin` instead), on Windows into `%LOCALAPPDATA%\Programs\claude-resume`, which they add to your PATH. You can also download a binary from the [releases page](https://github.com/csmarvz/claude-resume/releases) yourself, or build it with Go 1.26 or later: `go install github.com/csmarvz/claude-resume@latest`.

Only the Linux build has been tested so far. The macOS and Windows builds are released with each version, but nobody has run them on a real machine yet. If you try one, please [open an issue](https://github.com/csmarvz/claude-resume/issues) to say whether it works.

The only other requirement is the `claude` CLI, to resume sessions. Updates use the system `curl`.

## Usage

```
claude-resume                       pick a session, or list the last 10 outside a terminal
claude-resume -a                    list all sessions (--all)
claude-resume <id>                  resume a session in its folder (a unique ID prefix is enough)
claude-resume -r <id> <title>       rename a session (--rename)
claude-resume --export <id> [path]  save a session's prompts and replies as Markdown
claude-resume --backup <zip> [<id>...]  back up explicit sessions, or all live sessions
claude-resume --restore-backup <zip>   restore sessions from a backup
claude-resume -d <id>               move a session to the trash after a confirmation (--delete)
claude-resume -d -y <id>            move it without asking; needs the full session ID (--yes)
claude-resume --archive <id>        archive a session so that it never expires
claude-resume --restore <id>        move an archived or deleted session back to the session list
claude-resume --pin <id>            keep a session at the top of the list and safe from Claude Code's cleanup
claude-resume --unpin <id>          unpin a session
claude-resume update                install the latest release
claude-resume -v                    show the installed version (--version)
claude-resume -h                    show help (--help)
```

The title is the name you gave the session with `/rename`, otherwise the title claude generated, otherwise the first prompt. Sessions are read from `$CLAUDE_CONFIG_DIR/projects`, or `~/.claude/projects` if that variable is unset.

You can:
* Resume sessions
* Preview sessions
* Search sessions
* Rename sessions
* Export sessions as Markdown
* Pin sessions
* Archive sessions
* Delete sessions

In the picker, type `/` to open the commands for the selected session: `/pin`, `/archive`, `/rename`, `/fork`, `/export`, `/delete`, `/list-folders`, `/list-archives`, `/list-deleted`, `/help` and `/exit`. `/help` shows every command, keyboard shortcut and command-line option in the preview box. `/fork` resumes a copy of the session with Claude Code's `--fork-session`, leaving the original as it is. In the archived and deleted lists, `/list-sessions` takes you back to the session list. The `/` opens them when the search line is empty or after you move through the results with the arrow keys; while you type a search, it stays part of the search.

Renaming writes the same title record as `/rename` in Claude Code. Sessions that are open in a running Claude Code process are refused, because it could write its own title back.

## Exporting sessions

`/export` saves the selected live, archived, or deleted session as Markdown without opening or changing it. Edit the suggested `./<title>--<id>.md` path, relative to where claude-resume was started, then press ENTER to save or ESC to cancel. An existing file requires Y to overwrite; any other key returns to the path. The picker keeps its search and selection, and shows the saved path in its footer.

`claude-resume --export <id> [path]` does the same from the command line; omit the path to use the suggested filename. Both entry points expand `~` to your home directory and accept a folder to save the suggested filename inside it. Overwriting requires an interactive terminal. Missing or ambiguous IDs are refused.

Exports contain user prompts and Claude's text replies in order, without shortening or removing repeated messages. Consecutive text from the same role shares a heading. Tool calls/results, thinking, images and internal records (including task notifications, slash-command/skill records, and shell-command records) are left out. Malformed transcript lines are skipped. Running sessions can be exported; the export reads through one open transcript handle and can include messages appended while it is reading. On Unix, exports use owner-only read/write permissions (0600), including when replacing an existing file, because transcripts can contain private prompts.

## Backing up and restoring sessions

`/backup` saves the selected live, archived, or deleted session in a ZIP. `/backup-all` backs up every live session, including pinned sessions, regardless of search or folder filters. Both suggest a timestamped filename in the current folder and accept a different filename or an existing destination folder. Existing ZIPs require confirmation before replacement.

`claude-resume --backup <zip> <id>...` backs up explicit IDs; omit the IDs to back up all live sessions. Ambiguous IDs are refused. ZIPs contain raw transcripts, subagent/tool-output files, file history, session environment data, and a versioned manifest recording category and pin state. They are different from readable Markdown exports. Pin reconciliation runs first and may recover missing live files; its errors stop the backup. Backup commands do not purge the trash.

`/restore-backup` or `claude-resume --restore-backup <zip>` validates and stages the ZIP before changing sessions. Sessions return to their saved categories; live sessions go into the current Claude config. Pins are rebuilt. Live and deleted sessions receive fresh cleanup dates; archived sessions keep their dates. Duplicate IDs across project folders are skipped and reported so shared auxiliary data is not overwritten.

Before overwriting, compare both sessions' date, size, and last reply, then choose Skip, Overwrite, or Cancel. All choices are collected before installation; Cancel leaves sessions unchanged. Approved originals move to the trash for 30 days. If that would overwrite another trash copy, including an existing trash-category destination, the replacement is skipped. If installation fails, the original stays in trash and the error explains recovery through `/list-deleted`. That recovers ordinary live content, not its previous category or pin state. Earlier completed restores remain in place if a later one fails. Noninteractive commands refuse overwrites that require confirmation.

On Linux and macOS, detected running sessions produce a best-effort live-copy warning: the files may come from different moments, and raw bytes may include an incomplete final record. No snapshot consistency is promised. Close sessions for a reliable backup. On Windows, detected running sessions must be closed because an open reader can block Claude Code's compaction rename. Restoring over detected running sessions is always refused. Process detection is advisory, not a lock.

## Pinning sessions

`/pin` (CTRL-T) keeps a session at the top of the list, marked with ★ and shown in bold, and protects it from Claude Code's cleanup while it stays in the session list. If Claude Code deletes a pinned session anyway, because claude-resume didn't run for 30 days, claude-resume puts it back the next time it runs. The [FAQ](FAQ.md#how-does-pinning-protect-a-session) explains how. `/unpin` makes it an ordinary session again; a session last used more than 30 days ago is dated today, so Claude Code doesn't delete it at its next cleanup.

## Archiving sessions

`/archive` moves a session's files into `~/.claude-resume/archive`, outside Claude Code's folder. The session leaves the list, and Claude Code's cleanup can't reach it, so it never expires. `/list-archives` shows the archived sessions: ENTER restores one and resumes it, `/restore` only restores it. A restored session gets today's date, so Claude Code's 30 days start again from the restore. Sessions that are open in a running Claude Code process are refused.

The archive and the trash are shared by every Claude Code config: a session restored while `CLAUDE_CONFIG_DIR` points elsewhere goes into that config. Archiving and deleting move files, so `~/.claude-resume` has to be on the same disk as the config folder.

## Deleting sessions

A delete moves the session's transcript, its folder of subagent and tool output, and its `file-history` and `session-env` entries into `~/.claude-resume/trash`. It leaves `~/.claude/history.jsonl`, the prompt history behind the up arrow in Claude Code, untouched. Sessions that are open in a running Claude Code process are refused.

Deleted sessions stay in the trash for 30 days; claude-resume deletes older ones for good each time it runs. `/list-deleted` shows them: ENTER restores one and resumes it, `/restore` only restores it, `/delete` deletes it for good, and `/empty-trash` deletes them all. A restored session gets today's date.

`-y` skips the dialog and works without a terminal, so you can script it. It only accepts a full session ID, never a prefix.

Claude Code also deletes old transcripts on its own: by default, sessions untouched for 30 days (the `cleanupPeriodDays` setting). Pin or archive a session to keep it.

## Updating

Run `claude-resume update`. It downloads the latest release for your system, checks it against the release's SHA-256 checksums and replaces itself; if anything fails, the installed version stays as it was.

Once a day, the picker asks GitHub in the background whether a newer release is out, and shows it in yellow at the bottom right when there is one. Set `CLAUDE_RESUME_NO_UPDATE_CHECK=1` to turn that check off.

Version 0.0.1-alpha has no `update` command: to leave it, run the install command once more.

## Uninstall

```
rm "${PREFIX:-$HOME/.local}/bin/claude-resume"
```

On Windows, delete the `%LOCALAPPDATA%\Programs\claude-resume` folder and remove it from your PATH.

## How it works

The [FAQ](FAQ.md) explains where sessions are stored, what each command moves or deletes, and how Claude Code's own cleanup works.

## Caveat

`claude-resume` reads Claude Code's session files directly. Their format is not a public API, so a Claude Code update can break it.

## License

MIT
