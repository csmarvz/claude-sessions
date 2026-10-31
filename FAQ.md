# FAQ

## Where does claude-resume find my sessions?

Claude Code saves every session as a file named after the session ID, in a folder named after the directory you started it in: `~/.claude/projects/<folder>/<id>.jsonl`. claude-resume reads those files. If `CLAUDE_CONFIG_DIR` is set, it reads `$CLAUDE_CONFIG_DIR/projects` instead.

Next to the transcript, a session can have a folder of subagent and tool output (`projects/<folder>/<id>/`) and two more entries: `~/.claude/file-history/<id>`, the file backups behind rewinding, and `~/.claude/session-env/<id>`. claude-resume treats these four as one session: delete, archive and restore always move or remove all of them together.

## Does claude-resume change my sessions?

Only when you ask it to. Listing, searching, previewing and resuming only read the files. Renaming appends one title line to the transcript, the same line `/rename` writes in Claude Code. Archiving, deleting and restoring move the session's files. Sessions that are open in a running Claude Code process are refused for all of these.

## Why do old sessions disappear?

Claude Code deletes them. Its cleanup removes sessions whose transcript hasn't changed for 30 days, and `file-history` and `session-env` entries that are as old. The `cleanupPeriodDays` setting in `~/.claude/settings.json` changes the number of days. claude-resume doesn't delete anything on its own, apart from the trash described below.

## How does archiving keep a session?

`/archive` moves the session's files out of Claude Code's folder into `~/.claude-resume/archive`, keeping the same layout. Claude Code's cleanup only looks in its own folders, so it never sees an archived session. Restoring moves the files back and sets their date to today, so the 30 days start again; without that, Claude Code would delete an old session again the next time its cleanup runs.

## How does pinning protect a session?

A pinned session stays in Claude Code's folder, so Claude Code can resume it as usual, and Claude Code's cleanup can still delete it. claude-resume makes sure the data survives that.

When you pin a session, claude-resume creates a hard link to each of its files in `~/.claude-resume/pinned`, with the same layout. A hard link is a second name for the same data on disk: nothing is copied, no extra space is used, and whatever Claude Code appends to the session shows up under both names. Deleting one name leaves the data in place as long as the other exists, and Claude Code's cleanup deletes names without noticing the second one.

Every time claude-resume runs, it checks each pinned session:

- Files that are missing from Claude Code's folder are linked back from `~/.claude-resume/pinned` and dated today, so the cleanup doesn't delete them again at once.
- Files Claude Code added since the last run are linked into `~/.claude-resume/pinned`.
- When Claude Code has rewritten a file, which it does when it compacts a large transcript, the link is renewed to the new file.
- When Claude Code has moved the session to another project folder, the links follow it.

Between the cleanup and your next run of claude-resume, a deleted pinned session is missing from Claude Code's own `/resume`; it comes back on the next run. If Claude Code rewrote the transcript and then deleted it before claude-resume ran again, the restored transcript is the version from before the rewrite.

Hard links only work within one disk, which `~/.claude-resume` and the Claude Code config folder already have to share. Unpinning removes the links in `~/.claude-resume/pinned`. If the session was last used more than 30 days ago, it is dated today, so that Claude Code's next cleanup doesn't delete it. Archiving or deleting a pinned session unpins it.

## What happens when I delete a session?

The session moves to `~/.claude-resume/trash`, dated with the time of the delete. `/list-deleted` shows the trash, and a session in it can be restored for 30 days. Every time claude-resume runs, it deletes the sessions that have been in the trash longer than that. `/delete` in the trash list and `/empty-trash` delete for good.

`~/.claude/history.jsonl`, the prompt history behind the up arrow in Claude Code, is never touched.

## Why is the archive in `~/.claude-resume` and not in `~/.claude`?

So that it doesn't depend on Claude Code. Wiping or reinstalling `~/.claude` leaves it alone, and a future Claude Code version that cleans up folders it doesn't know about can't reach it.

Two things follow from that. Moving a session is a rename, which only works within one disk, so `~/.claude-resume` and the Claude Code config folder have to be on the same disk. And every config shares one archive and trash: a session restored while `CLAUDE_CONFIG_DIR` points to another config goes into that config.

## I use several Claude Code configs. What does claude-resume show?

The sessions of the config it runs with: `~/.claude`, or `CLAUDE_CONFIG_DIR` when it is set. The archive and the trash are shared between configs, as described above.

## Does a backup contain the whole session?

A ZIP backup includes the same four session parts described above, with raw bytes and saved category/pin state. It does not include global prompt history or the source project's working files. A backup of a running session is a best-effort live copy, not a coordinated snapshot of all files. Windows requires detected running sessions to be closed to avoid blocking compaction. Pin maintenance runs before backup and can recreate missing live files.

## What happens if backup restore fails?

Validation and conflict decisions happen before changing sessions. Before an approved overwrite, the old session moves to trash for 30 days; an occupied recovery location prevents that overwrite. If installing the new data fails, the old content remains in trash and the error reports how to recover it. A trash restore returns an ordinary live session, without its former archive/pin state. The program does not promise automatic recovery from a crash or power loss.

## Does it work on macOS and Windows?

The releases include builds for both, but only the Linux build has been tested so far. On Windows, claude-resume stays open while the resumed Claude Code runs and exits when it ends. If you try either, please [open an issue](https://github.com/csmarvz/claude-resume/issues) to say whether it works.
