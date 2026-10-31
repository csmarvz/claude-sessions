package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func backupTestStores(t *testing.T) string {
	t.Helper()
	oldHome, oldConfig, oldProjects, oldArchive, oldTrash, oldPinned := home, config, projects, archived, trash, pinned
	t.Cleanup(func() {
		home, config, projects, archived, trash, pinned = oldHome, oldConfig, oldProjects, oldArchive, oldTrash, oldPinned
	})
	dir := t.TempDir()
	home, config = dir, filepath.Join(dir, "config")
	projects = filepath.Join(config, "projects")
	archived, trash, pinned = filepath.Join(dir, ".claude-resume", "archive"), filepath.Join(dir, ".claude-resume", "trash"), filepath.Join(dir, ".claude-resume", "pinned")
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	return dir
}

func backupTestWrite(t *testing.T, p, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func backupTestSession(t *testing.T, category, folder, id, text string) *session {
	t.Helper()
	paths := parts(backupStores()[category], folder, id)
	for _, p := range []string{paths[0], filepath.Join(paths[1], "subagents", "agent.jsonl"), filepath.Join(paths[2], "checkpoint"), filepath.Join(paths[3], "env")} {
		backupTestWrite(t, p, text)
	}
	if err := os.MkdirAll(filepath.Join(paths[1], "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(0, 0, -40).Truncate(time.Second)
	if err := os.Chtimes(paths[0], old, old); err != nil {
		t.Fatal(err)
	}
	return &session{path: paths[0], id: id, mod: old}
}

func backupTestRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBackupRoundTrip(t *testing.T) {
	dir := backupTestStores(t)
	var list []*session
	for i, category := range []string{"live", "archive", "trash", "live"} {
		id := fmt.Sprintf("%08d-4af1-44da-a134-90f6fafa5d25", i)
		s := backupTestSession(t, category, "project", id, "opaque\n{unknown}\npartial")
		if i == 3 {
			if msg, ok := pin(s); !ok {
				t.Fatal(msg)
			}
		}
		list = append(list, s)
	}
	zipPath := filepath.Join(dir, "backup.zip")
	if err := backupWrite(list, zipPath, false); err != nil {
		t.Fatal(err)
	}
	backupTestStores(t)
	msg, err := backupRestore(zipPath, func([]line) string { t.Fatal("unexpected conflict"); return "cancel" })
	if err != nil || !strings.Contains(msg, "Restored 4 sessions; skipped 0.") {
		t.Fatal(msg, err)
	}
	for i, category := range []string{"live", "archive", "trash", "live"} {
		id := list[i].id
		paths := parts(backupStores()[category], "project", id)
		for _, p := range []string{paths[0], filepath.Join(paths[1], "subagents", "agent.jsonl"), filepath.Join(paths[2], "checkpoint"), filepath.Join(paths[3], "env")} {
			if got := backupTestRead(t, p); got != "opaque\n{unknown}\npartial" {
				t.Fatalf("raw bytes changed: %q", got)
			}
		}
		if st, err := os.Stat(filepath.Join(paths[1], "empty")); err != nil || !st.IsDir() {
			t.Fatal("empty directory lost", err)
		}
		st, _ := os.Stat(paths[0])
		if category == "archive" && !st.ModTime().Equal(list[i].mod) {
			t.Fatal("archive date changed")
		}
		if category != "archive" && time.Since(st.ModTime()) > time.Minute {
			t.Fatal("cleanup date was not renewed")
		}
		if i == 3 {
			p, err := os.Stat(parts(pinned, "project", id)[0])
			if err != nil || !os.SameFile(st, p) {
				t.Fatal("pin was not rebuilt", err)
			}
		}
	}
}

func TestBackupOverwrite(t *testing.T) {
	for _, action := range []string{"overwrite", "skip", "cancel", "trash-collision", "install-failure"} {
		t.Run(action, func(t *testing.T) {
			dir := backupTestStores(t)
			s := backupTestSession(t, "live", "project", exportID, "incoming")
			zipPath := filepath.Join(dir, "backup.zip")
			if err := backupWrite([]*session{s}, zipPath, false); err != nil {
				t.Fatal(err)
			}
			backupTestWrite(t, s.path, "original")
			backupTestWrite(t, filepath.Join(strings.TrimSuffix(s.path, ".jsonl"), "obsolete"), "obsolete")
			if action == "trash-collision" {
				backupTestSession(t, "trash", "project", exportID, "older recovery")
			}
			called := false
			msg, err := backupRestore(zipPath, func(lines []line) string {
				called = true
				if len(lines) < 8 {
					t.Fatal("comparison missing")
				}
				if action == "install-failure" {
					// Lose staged input after validation to force an installation-time I/O failure.
					stages, err := filepath.Glob(filepath.Join(home, ".claude-resume", "restore-*"))
					if err != nil || len(stages) != 1 {
						t.Fatal(stages, err)
					}
					if err := os.Remove(parts(filepath.Join(stages[0], "live"), "project", exportID)[0]); err != nil {
						t.Fatal(err)
					}
					return "overwrite"
				}
				return action
			})
			switch action {
			case "overwrite":
				if err != nil || !called || backupTestRead(t, s.path) != "incoming" || backupTestRead(t, parts(trash, "project", exportID)[0]) != "original" {
					t.Fatal(msg, err)
				}
				if _, err := os.Stat(filepath.Join(strings.TrimSuffix(s.path, ".jsonl"), "obsolete")); !errors.Is(err, fs.ErrNotExist) {
					t.Fatal("old files merged into restored session")
				}
				msg, err = backupRestore(zipPath, func([]line) string { t.Fatal("trash collision prompted overwrite"); return "overwrite" })
				if err != nil || !strings.Contains(msg, "trash") || backupTestRead(t, parts(trash, "project", exportID)[0]) != "original" {
					t.Fatal("repeated overwrite lost recovery", msg, err)
				}
			case "skip", "trash-collision":
				if err != nil || backupTestRead(t, s.path) != "original" || !strings.Contains(msg, "skipped 1") {
					t.Fatal(msg, err)
				}
			case "cancel":
				if err == nil || backupTestRead(t, s.path) != "original" {
					t.Fatal(msg, err)
				}
			case "install-failure":
				if err == nil || !strings.Contains(err.Error(), "preserved") || backupTestRead(t, parts(trash, "project", exportID)[0]) != "original" {
					t.Fatal(msg, err)
				}
				if _, err := os.Stat(s.path); !errors.Is(err, fs.ErrNotExist) {
					t.Fatal("partial new transcript left behind", err)
				}
			}
		})
	}
}

func TestBackupPinReconciliation(t *testing.T) {
	backupTestStores(t)
	s := backupTestSession(t, "live", "p", exportID, "protected")
	if msg, ok := pin(s); !ok {
		t.Fatal(msg)
	}
	for _, p := range parts(config, "p", exportID) {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
	list, err := backupPool()
	if err != nil || len(list) != 1 || !list[0].pinned || backupTestRead(t, list[0].path) != "protected" {
		t.Fatal(list, err)
	}
	if err := os.RemoveAll(filepath.Join(config, "file-history")); err != nil {
		t.Fatal(err)
	}
	backupTestWrite(t, filepath.Join(config, "file-history"), "blocked")
	if _, err := backupPool(); err == nil {
		t.Fatal("pin reconciliation error was swallowed")
	}
}

func TestBackupDuplicateIDs(t *testing.T) {
	for _, inZip := range []bool{true, false} {
		t.Run(fmt.Sprint(inZip), func(t *testing.T) {
			dir := backupTestStores(t)
			a := backupTestSession(t, "live", "a", exportID, "new")
			list := []*session{a}
			if inZip {
				list = append(list, backupTestSession(t, "live", "b", exportID, "new"))
			}
			file := filepath.Join(dir, "backup.zip")
			if err := backupWrite(list, file, false); err != nil {
				t.Fatal(err)
			}
			backupTestStores(t)
			if !inZip {
				backupTestSession(t, "live", "b", exportID, "untouched")
			}
			msg, err := backupRestore(file, func([]line) string { t.Fatal("ambiguous session prompted"); return "overwrite" })
			if err != nil || !strings.Contains(msg, "duplicate ID") || !strings.Contains(msg, "Restored 0") {
				t.Fatal(msg, err)
			}
			if !inZip && backupTestRead(t, filepath.Join(config, "file-history", exportID, "checkpoint")) != "untouched" {
				t.Fatal("shared destination data changed")
			}
		})
	}
}

func backupTestZip(t *testing.T, file string, manifest backupManifest, extra string, mode fs.FileMode) {
	t.Helper()
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	put := func(name, data string, mode fs.FileMode) {
		h := &zip.FileHeader{Name: name, Method: zip.Store}
		h.SetMode(mode)
		w, err := z.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.Marshal(manifest)
	put("manifest.json", string(b), 0o600)
	for _, name := range manifest.Files {
		put(name, "payload", 0o600)
	}
	if extra != "" {
		put(extra, "target", mode)
	}
	if err := errors.Join(z.Close(), f.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestBackupInvalidZIP(t *testing.T) {
	dir := backupTestStores(t)
	name := "live/projects/p/" + exportID + ".jsonl"
	for _, tc := range []struct {
		name string
		mode fs.FileMode
	}{
		{"../escape", 0o600}, {"/absolute", 0o600}, {"C:/escape", 0o600}, {`\\host\share`, 0o600},
		{"live/projects/p/link", os.ModeSymlink | 0o777}, {name, 0o600}, {strings.ToUpper(name), 0o600},
		{"live/projects/CON/data", 0o600}, {"undeclared", 0o600}, {"live/projects/p/trailing. /x", 0o600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := backupManifest{Version: 1, Sessions: []backupSession{{Category: "live", Project: "p", ID: exportID, Modified: time.Now()}}, Files: []string{name}}
			file := filepath.Join(dir, "bad.zip")
			backupTestZip(t, file, m, tc.name, tc.mode)
			if _, _, err := backupStage(file); err == nil {
				t.Fatal("unsafe ZIP accepted")
			}
			if _, err := os.Stat(config); !errors.Is(err, fs.ErrNotExist) {
				t.Fatal("validation created target config")
			}
		})
	}
	for _, version := range []int{0, 2} {
		m := backupManifest{Version: version, Sessions: []backupSession{{Category: "live", Project: "p", ID: exportID, Modified: time.Now()}}, Files: []string{name}}
		file := filepath.Join(dir, "bad.zip")
		backupTestZip(t, file, m, "", 0)
		if _, _, err := backupStage(file); err == nil {
			t.Fatal("unsupported version accepted")
		}
	}
	m := backupManifest{Version: 1, Sessions: []backupSession{{Category: "live", Project: "p", ID: exportID, Modified: time.Now()}}, Files: []string{name}}
	file := filepath.Join(dir, "corrupt.zip")
	backupTestZip(t, file, m, "", 0)
	b := []byte(backupTestRead(t, file))
	idx := bytes.Index(b, []byte("payload"))
	b[idx] ^= 0xff
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backupStage(file); err == nil {
		t.Fatal("bad CRC accepted")
	}
}

func TestBackupPublicationAndAliases(t *testing.T) {
	dir := backupTestStores(t)
	s := backupTestSession(t, "live", "p", exportID, "original")
	file := filepath.Join(dir, "old.zip")
	backupTestWrite(t, file, "previous")
	if err := backupWrite([]*session{s}, file, false); !errors.Is(err, fs.ErrExist) {
		t.Fatal(err)
	}
	if err := backupWrite([]*session{s}, s.path, true); err == nil || backupTestRead(t, s.path) != "original" {
		t.Fatal("source overwritten", err)
	}
	alias := filepath.Join(dir, "alias.zip")
	if err := os.Link(s.path, alias); err != nil {
		t.Fatal(err)
	}
	if err := backupWrite([]*session{s}, alias, true); err == nil || backupTestRead(t, s.path) != "original" {
		t.Fatal("source hard-link overwritten", err)
	}
	if err := os.Remove(s.path); err != nil {
		t.Fatal(err)
	}
	if err := backupWrite([]*session{s}, file, true); err == nil || backupTestRead(t, file) != "previous" {
		t.Fatal("failed output replaced previous ZIP", err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".claude-resume-backup-*"))
	if len(matches) != 0 {
		t.Fatal("partial output left behind", matches)
	}
}

func TestBackupRunningPolicy(t *testing.T) {
	dir := backupTestStores(t)
	if runtime.GOOS != "windows" {
		if _, err := exec.Command("ps", "-ww", "-p", fmt.Sprint(os.Getpid()), "-o", "args=").Output(); err != nil {
			t.Skip("ps does not support the process lookup used by isClaude:", err)
		}
	}
	s := backupTestSession(t, "live", "p", exportID, "running")
	// The subprocess name includes claude on Unix; Windows checks the current PID's existence.
	cmd := exec.Command(os.Args[0], "-test.run=^TestBackupRunningProcess$", "--", "claude")
	cmd.Env = append(os.Environ(), "CLAUDE_BACKUP_RUNNING_TEST=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	backupTestWrite(t, filepath.Join(config, "sessions", fmt.Sprint(cmd.Process.Pid)+".json"), fmt.Sprintf(`{"sessionId":%q}`, exportID))
	warning, err := backupLiveWarning([]*session{s})
	if runtime.GOOS == "windows" {
		if err == nil || !strings.Contains(err.Error(), "Windows") {
			t.Fatal("running Windows backup not refused", warning, err)
		}
	} else if err != nil || !strings.Contains(warning, "best-effort") {
		t.Fatal(warning, err)
	}
	// Build a backup before adding the running destination record to a fresh config.
	if err := os.RemoveAll(filepath.Join(config, "sessions")); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "backup.zip")
	if err := backupWrite([]*session{s}, file, false); err != nil {
		t.Fatal(err)
	}
	backupTestWrite(t, filepath.Join(config, "sessions", fmt.Sprint(cmd.Process.Pid)+".json"), fmt.Sprintf(`{"sessionId":%q}`, exportID))
	if _, err := backupRestore(file, func([]line) string { t.Fatal("running destination prompted"); return "overwrite" }); err == nil {
		t.Fatal("running destination allowed")
	}
}

func TestBackupRunningProcess(t *testing.T) {
	if os.Getenv("CLAUDE_BACKUP_RUNNING_TEST") == "1" {
		time.Sleep(30 * time.Second)
	}
}

func TestBackupCLI(t *testing.T) {
	dir := backupTestStores(t)
	backupTestSession(t, "live", "p", exportID, "raw")
	run := func(args ...string) (string, error) {
		cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestExportCLIProcess$", "--"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "CLAUDE_RESUME_EXPORT_TEST=1")
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
	file := filepath.Join(dir, "backup.zip")
	if out, err := run("--backup", file, exportID[:8]); err != nil || !strings.Contains(out, "Saved ") {
		t.Fatal(out, err)
	}
	if out, err := run("--backup", file); err == nil || !strings.Contains(out, "terminal") {
		t.Fatal("unattended ZIP overwrite", out, err)
	}
	if out, err := run("--restore-backup", file); err == nil || !strings.Contains(out, "terminal") {
		t.Fatal("unattended session overwrite", out, err)
	}
	if err := os.RemoveAll(config); err != nil {
		t.Fatal(err)
	}
	if out, err := run("--restore-backup", file); err != nil || !strings.Contains(out, "Restored 1") {
		t.Fatal("restore into missing config", out, err)
	}
	if out, err := run("--backup", filepath.Join(dir, "all.zip")); err != nil {
		t.Fatal("all live backup", out, err)
	}
	for _, args := range [][]string{{"--backup"}, {"--restore-backup"}, {"--backup", "x.zip", "nonsense"}, {"--restore-backup", file, exportID}} {
		if out, err := run(args...); err == nil || !strings.Contains(out, "usage:") {
			t.Fatal("invalid arguments accepted", args, out, err)
		}
	}
}

func TestBackupPickerPaths(t *testing.T) {
	for _, action := range []string{"backup", "backup-all", "restore-backup"} {
		t.Run(action, func(t *testing.T) {
			dir := backupTestStores(t)
			s := backupTestSession(t, "live", "p", exportID, "picker")
			file := filepath.Join(dir, "picker.zip")
			var selected *session
			if action == "backup" {
				selected = s
			}
			if action == "restore-backup" {
				if err := backupWrite([]*session{s}, file, false); err != nil {
					t.Fatal(err)
				}
				backupTestStores(t)
			}
			keys := make(chan []byte, 1)
			out := &exportConfirmOutput{keys: keys, input: "\x15" + file + "\r"}
			terminal := &tty{out: bufio.NewWriter(out), keys: keys, wake: make(chan struct{})}
			msg := terminal.backupDialog(action, selected)
			if !strings.HasPrefix(msg, "Saved ") && !strings.HasPrefix(msg, "Restored 1") {
				t.Fatal(msg)
			}
		})
	}
}

func TestBackupChooseIgnoresTyping(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"unmatched query\r", "skip"},
		{"unmatched query\x1b[B\r", "overwrite"},
		{"unmatched query\x1b[B\x1b[B\r", "cancel"},
	} {
		keys := make(chan []byte, 1)
		out := &exportConfirmOutput{keys: keys, input: tc.input}
		terminal := &tty{out: bufio.NewWriter(out), keys: keys, wake: make(chan struct{})}
		if got := backupChoose(terminal, []line{{"", "comparison"}}); got != tc.want {
			t.Fatalf("got %q, want %q", got, tc.want)
		}
	}
}

func TestBackupTrustedSymlinkRoots(t *testing.T) {
	dir := backupTestStores(t)
	s := backupTestSession(t, "live", "p", exportID, "raw")
	realConfig := config
	alias := filepath.Join(dir, "config-link")
	if err := os.Symlink(config, alias); err != nil {
		t.Skip("directory symlinks unavailable:", err)
	}
	config, projects = alias, filepath.Join(alias, "projects")
	s.path = parts(config, "p", exportID)[0]
	home = filepath.Join(dir, "home-link")
	if err := os.Symlink(dir, home); err != nil {
		t.Fatal(err)
	}
	archived, trash, pinned = filepath.Join(home, ".claude-resume", "archive"), filepath.Join(home, ".claude-resume", "trash"), filepath.Join(home, ".claude-resume", "pinned")
	if _, err := link(exportID, "p"); err != nil {
		t.Fatal("pin through linked roots:", err)
	}
	if err := os.Remove(s.path); err != nil {
		t.Fatal(err)
	}
	pool, err := backupPool()
	if err != nil || len(pool) != 1 || !pool[0].pinned {
		t.Fatal("pin recovery through linked roots:", pool, err)
	}
	output := filepath.Join(dir, "output-link")
	if err := os.Symlink(dir, output); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(output, "backup.zip")
	if err := backupWrite(pool, file, false); err != nil {
		t.Fatal("backup through linked output parent:", err)
	}
	if err := backupWrite(pool, filepath.Join(realConfig, "alias.zip"), false); err == nil {
		t.Fatal("physical output alias into config accepted")
	}
	inside := filepath.Join(parts(realConfig, "p", exportID)[1], "inside.zip")
	backupTestWrite(t, inside, backupTestRead(t, file))
	if _, err := backupRestore(inside, func([]line) string { t.Fatal("unsafe restore prompted"); return "overwrite" }); err == nil || !strings.Contains(err.Error(), "inside a restore destination") {
		t.Fatal("physical ZIP alias inside restore destination accepted:", err)
	}
	_, stage, err := backupStage(file)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(stage) != filepath.Join(home, ".claude-resume") {
		t.Fatal("restore staged outside application storage:", stage)
	}
	if err := os.RemoveAll(stage); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(realConfig); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(realConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(pinned); err != nil {
		t.Fatal(err)
	}
	msg, err := backupRestore(file, func([]line) string { return "cancel" })
	if err != nil || !strings.Contains(msg, "Restored 1") || backupTestRead(t, s.path) != "raw" {
		t.Fatal("restore through linked roots:", msg, err)
	}
}

func TestBackupRestoreTracksTrashChanges(t *testing.T) {
	for _, trashFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(trashFirst), func(t *testing.T) {
			dir := backupTestStores(t)
			live := backupTestSession(t, "live", "p", exportID, "new live")
			deleted := backupTestSession(t, "trash", "q", exportID, "new trash")
			list := []*session{live, deleted}
			if trashFirst {
				list = []*session{deleted, live}
			}
			file := filepath.Join(dir, "backup.zip")
			if err := backupWrite(list, file, false); err != nil {
				t.Fatal(err)
			}
			backupTestStores(t)
			original := backupTestSession(t, "live", "p", exportID, "original")
			msg, err := backupRestore(file, func([]line) string { return "overwrite" })
			if err != nil || !strings.Contains(msg, "Restored 1 session; skipped 1") {
				t.Fatal(msg, err)
			}
			if trashFirst {
				if backupTestRead(t, original.path) != "original" || backupTestRead(t, parts(trash, "q", exportID)[2]+"/checkpoint") != "new trash" {
					t.Fatal("trash install did not protect later overwrite")
				}
			} else if backupTestRead(t, parts(trash, "p", exportID)[2]+"/checkpoint") != "original" {
				t.Fatal("later trash install changed preserved data")
			}
		})
	}
}

func TestBackupSharedTrashProtection(t *testing.T) {
	dir := backupTestStores(t)
	s := backupTestSession(t, "live", "p", exportID, "incoming")
	file := filepath.Join(dir, "backup.zip")
	if err := backupWrite([]*session{s}, file, false); err != nil {
		t.Fatal(err)
	}
	backupTestWrite(t, s.path, "original")
	backupTestWrite(t, parts(trash, "other", exportID)[0], "older recovery")
	msg, err := backupRestore(file, func([]line) string { t.Fatal("shared trash collision prompted"); return "overwrite" })
	if err != nil || !strings.Contains(msg, "skipped 1") || backupTestRead(t, s.path) != "original" {
		t.Fatal(msg, err)
	}
	if _, err := os.Stat(parts(trash, "p", exportID)[2]); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("existing trash session acquired unrelated history", err)
	}
}

func TestBackupDuplicateAppearsDuringReview(t *testing.T) {
	dir := backupTestStores(t)
	a := backupTestSession(t, "live", "a", exportID, "a incoming")
	idB := "11111111-4af1-44da-a134-90f6fafa5d25"
	b := backupTestSession(t, "live", "b", idB, "b incoming")
	file := filepath.Join(dir, "backup.zip")
	if err := backupWrite([]*session{a, b}, file, false); err != nil {
		t.Fatal(err)
	}
	backupTestStores(t)
	backupTestSession(t, "live", "b", idB, "b original")
	msg, err := backupRestore(file, func([]line) string {
		backupTestWrite(t, parts(config, "other", exportID)[0], "concurrent session")
		return "overwrite"
	})
	if err != nil || !strings.Contains(msg, "skipped 1") || backupTestRead(t, parts(config, "other", exportID)[0]) != "concurrent session" {
		t.Fatal(msg, err)
	}
	if _, err := os.Stat(parts(config, "a", exportID)[2]); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("concurrent session acquired unrelated history", err)
	}
}

func TestBackupCheckedPinScan(t *testing.T) {
	backupTestStores(t)
	backupTestSession(t, "live", "p", exportID, "live")
	backupTestWrite(t, filepath.Join(pinned, "projects"), "not a directory")
	if _, err := backupPool(); err == nil {
		t.Fatal("unreadable pin inventory was ignored")
	}
}

func TestBackupCaseAliases(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a case-sensitive filesystem")
	}
	dir := backupTestStores(t)
	s := backupTestSession(t, "live", "p", exportID, "source")
	base := strings.TrimSuffix(s.path, ".jsonl")
	backupTestWrite(t, filepath.Join(base, "Output.txt"), "first")
	backupTestWrite(t, filepath.Join(base, "output.txt"), "second")
	if backupTestRead(t, filepath.Join(base, "Output.txt")) != "first" {
		t.Skip("filesystem is case insensitive")
	}
	if err := backupWrite([]*session{s}, filepath.Join(dir, "backup.zip"), false); err == nil || !strings.Contains(err.Error(), "case-colliding") {
		t.Fatal("source data silently omitted", err)
	}
}

type backupCompactionWriter struct {
	buffer bytes.Buffer
	file   string
	did    bool
}

func (w *backupCompactionWriter) String() string { return w.buffer.String() }

func (w *backupCompactionWriter) Write(p []byte) (int, error) {
	if !w.did {
		w.did = true
		f, err := os.OpenFile(w.file, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return 0, err
		}
		_, err = f.WriteString("appended\n")
		err = errors.Join(err, f.Close())
		if err != nil {
			return 0, err
		}
		if err := os.WriteFile(w.file+".new", []byte("compacted replacement\n"), 0o600); err != nil {
			return 0, err
		}
		if err := os.Rename(w.file+".new", w.file); err != nil {
			return 0, err
		}
	}
	return w.buffer.Write(p)
}

func TestBackupCopyDuringCompaction(t *testing.T) {
	p := filepath.Join(t.TempDir(), "transcript")
	backupTestWrite(t, p, "original\n")
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	w := &backupCompactionWriter{file: p}
	err = backupCopyFile(w, p, info)
	if runtime.GOOS == "windows" {
		if err == nil {
			t.Fatal("Windows compaction unexpectedly succeeded; reevaluate closed-session fallback")
		}
		if err := os.Rename(p+".new", p); err != nil {
			t.Fatal("backup left the reader open after an error", err)
		}
		return
	}
	if err != nil || !strings.Contains(w.String(), "original\n") || strings.Contains(w.String(), "compacted") || backupTestRead(t, p) != "compacted replacement\n" {
		t.Fatal("compaction changed the open transcript copy", w.String(), err)
	}
}

func TestBackupRequiredTranscript(t *testing.T) {
	dir := backupTestStores(t)
	s := backupTestSession(t, "live", "p", exportID, "original")
	if err := os.Remove(s.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := backupWrite([]*session{s}, filepath.Join(dir, "backup.zip"), false); err == nil {
		t.Fatal("directory accepted as a transcript")
	}
}

func TestBackupReconciliationRejectsLinks(t *testing.T) {
	backupTestStores(t)
	s := backupTestSession(t, "live", "p", exportID, "live data")
	if msg, ok := pin(s); !ok {
		t.Fatal(msg)
	}
	external := t.TempDir()
	sentinel := filepath.Join(external, exportID, "checkpoint")
	backupTestWrite(t, sentinel, "outside sentinel")
	linked := filepath.Join(pinned, "file-history")
	if err := os.RemoveAll(linked); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, linked); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if _, err := backupPool(); err == nil {
		t.Fatal("reconciliation accepted a linked ancestor")
	}
	if got := backupTestRead(t, sentinel); got != "outside sentinel" {
		t.Fatalf("reconciliation changed external data: %q", got)
	}
}

func TestBackupReconciliationRejectsNestedLinks(t *testing.T) {
	backupTestStores(t)
	s := backupTestSession(t, "live", "p", exportID, "live data")
	backupTestWrite(t, filepath.Join(config, "file-history", exportID, "subdir", "checkpoint"), "live nested")
	if msg, ok := pin(s); !ok {
		t.Fatal(msg)
	}
	external := t.TempDir()
	sentinel := filepath.Join(external, "checkpoint")
	backupTestWrite(t, sentinel, "outside nested sentinel")
	linked := filepath.Join(pinned, "file-history", exportID, "subdir")
	if err := os.RemoveAll(linked); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, linked); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if _, err := backupPool(); err == nil {
		t.Fatal("reconciliation accepted a nested link")
	}
	if got := backupTestRead(t, sentinel); got != "outside nested sentinel" {
		t.Fatalf("reconciliation changed external data: %q", got)
	}
	if st, err := os.Lstat(linked); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("reconciliation changed the nested link", err)
	}
}

func TestBackupHelpIntegration(t *testing.T) {
	for _, mode := range []string{"", "archive", "trash"} {
		var text strings.Builder
		for _, line := range help(mode) {
			text.WriteString(line.text + "\n")
		}
		for _, command := range []string{"/backup", "/backup-all", "/restore-backup", "/help", "/list-folders", "--backup <zip>", "--restore-backup <zip>", "--help"} {
			if !strings.Contains(text.String(), command) {
				t.Errorf("mode %q help is missing %q", mode, command)
			}
		}
		count := 0
		for _, command := range commands(mode) {
			if command.key == "backup" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("mode %q has %d selected-session backup commands", mode, count)
		}
		if strings.Contains(text.String(), "/fork") != (mode == "") {
			t.Errorf("mode %q has incorrect fork command visibility", mode)
		}
	}
}
