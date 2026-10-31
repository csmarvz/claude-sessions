# Changelog

## [Unreleased]

### Changed

- The search line has a border above and below it and a `❯` in place of the `search>`, `folder>`, `rename>` and `export>` labels. The count moved into the bottom border, which frees the row it used (#32).
- The folder and session ID columns are aligned to the right edge of the terminal.

## [0.0.12-alpha] - 2026-10-31

### Added

- `/help` shows every command, keyboard shortcut and command-line option in the preview box (#13).

## [0.0.11-alpha] - 2026-10-31

### Added

- `/fork` resumes a copy of the selected session with Claude Code's `--fork-session`, leaving the original as it is (#21).

### Changed

- `/folders` is now `/list-folders`, like the other lists (#24). Typing `/folders` still finds it.
- A session whose folder no longer exists resumes in the current folder, as Claude Code does, instead of stopping with an error (#15).

## [0.0.10-alpha] - 2026-10-30

### Added

- `/export` saves the selected session's prompts and replies as a Markdown file, from the session list, the archive or the trash, without changing the session (#22). The path is suggested and can be edited before saving, and an existing file is only replaced after a confirmation. `claude-resume --export <id> [path]` does the same from the command line. Thanks to @lborba.

## [0.0.9-alpha] - 2026-10-29

### Changed

- `claude-resume update` shows each step: the installed version, the release it found, a progress line while downloading, the checksum check and the install. When something fails, it says what went wrong in plain words (no connection, a stalled or broken download, a damaged file, no permission to write) and confirms that nothing was changed.

## [0.0.8-alpha] - 2026-10-29

### Added

- Pinning (#7): `/pin` or CTRL-T keeps a session at the top of the list, marked with ★ and shown in bold. claude-resume keeps hard links to a pinned session's files in `~/.claude-resume/pinned`, so if Claude Code's cleanup deletes the session, the next run of claude-resume puts it back. `/unpin` undoes it, and `claude-resume --pin <id>` and `--unpin <id>` do the same from the command line.
- `/exit` quits the picker.

## [0.0.7-alpha] - 2026-10-29

### Added

- Deleted sessions go to a trash in `~/.claude-resume/trash` for 30 days instead of being removed (#8). `/list-deleted` shows them: ENTER restores and resumes one, `/restore` only restores it, `/delete` deletes it for good and `/empty-trash` deletes them all. Each run of claude-resume deletes sessions that have been in the trash for more than 30 days.
- `/list-sessions` returns to the session list, and every list offers the commands to open the other two.

### Changed

- The archive moved from `~/.claude/claude-resume/archive` to `~/.claude-resume/archive`, outside Claude Code's folder. Sessions archived with 0.0.6-alpha are not moved; move that folder by hand if you have any.
- The picker's delete confirmation is orange instead of red, since the delete can be undone. Deleting from the trash keeps the red confirmation. All confirmations say "Y confirm".
- `claude-resume -d` moves the session to the trash, and `--restore` also finds sessions there.

## [0.0.6-alpha] - 2026-10-29

### Added

- Commands in the picker: type `/` on an empty search line, or after moving through the results with the arrow keys, to list commands for the selected session. Commands match anywhere in their name.
- Archiving (#9): `/archive` moves a session into `~/.claude/claude-resume/archive`, where Claude Code's cleanup can't delete it. `/list-archives` shows the archived sessions; ENTER restores and resumes one, `/restore` only restores it. A restored session is dated today, so Claude Code's 30 days start again.
- `claude-resume --archive <id>` and `--restore <id>`.

### Changed

- Empty lists stay on screen: deleting or archiving the last session no longer exits the picker or switches lists.

## [0.0.5-alpha] - 2026-10-28

### Fixed

- Session text is stripped of control characters before it is shown, so a title, prompt, reply or folder name can't send escape sequences to the terminal.
- Only session files named with a real UUID are listed.
- The delete dialog no longer crashes on very narrow terminals, and `-r` without arguments prints the usage.
- Update downloads give up after 1 minute instead of hanging.
- When the same session ID exists in two project folders, the picker acts on the selected copy, and the command line refuses the ID.
- Versions are compared by order, so a newer build is no longer "updated" to an older release.
- The installers check the download against the release checksums, and `install.ps1` keeps PATH entries such as `%USERPROFILE%` unexpanded.
- Chinese, Japanese and Korean characters and emoji no longer misalign rows.

## [0.0.4-alpha] - 2026-10-28

### Added

- Renaming with Ctrl-R in the picker and `claude-resume -r <id> <title>` (#3).
- The preview shows the git branch, the session's size on disk and the context tokens of the last turn (#2).

### Changed

- Ctrl-D asks for confirmation inside the list instead of opening a full-screen dialog (#4).
- The search cursor hides while you move through the list (#6).
- The footer is aligned with the left edge.

## [0.0.3-alpha] - 2026-10-28

### Changed

- The version and the update notice are shown in the footer.
- The picker redraws only when something changes.

## [0.0.2-alpha] - 2026-10-28

### Added

- `claude-resume -v` (#5).
- `claude-resume update` and a daily update notice in the picker (#1).

## [0.0.1-alpha] - 2026-10-28

First release of the Go version: a single binary to list, preview, search, resume and delete Claude Code sessions.
