package main

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"golang.org/x/term"
)

type backupSession struct {
	Category string    `json:"category"`
	Project  string    `json:"project"`
	ID       string    `json:"id"`
	Pinned   bool      `json:"pinned"`
	Modified time.Time `json:"modified"`
}

type backupManifest struct {
	Version  int             `json:"version"`
	Sessions []backupSession `json:"sessions"`
	Files    []string        `json:"files"`
}

func backupStores() map[string]string {
	return map[string]string{"live": config, "archive": archived, "trash": trash}
}

func backupName() string {
	return "./claude-resume-backup-" + time.Now().Format("20060102-150405") + ".zip"
}

// Store roots are trusted; session data below them must not redirect through links.
func backupNoLinks(p, store string) error {
	p, err := filepath.Abs(p)
	if err != nil {
		return err
	}
	store, err = filepath.Abs(store)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(store, p)
	if err != nil || rel != "." && !filepath.IsLocal(rel) {
		return fmt.Errorf("path is outside its store: %s", p)
	}
	for rel != "." {
		st, err := os.Lstat(p)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err == nil && st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("links are not supported: %s", p)
		}
		p = filepath.Dir(p)
		rel = filepath.Dir(rel)
	}
	return nil
}

// Resolve existing ancestors even when a store or output file does not exist yet.
func backupResolved(p string) (string, error) {
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return resolved, err
	}
	if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("cannot resolve path: %s", p)
	}
	if filepath.Dir(p) == p {
		return "", err
	}
	parent, err := backupResolved(filepath.Dir(p))
	return filepath.Join(parent, filepath.Base(p)), err
}

func backupPath(p string) (string, error) { return outputPath(p, filepath.Base(backupName())) }

func backupPool() ([]*session, error) {
	all, err := backupScan(config)
	if err != nil {
		return nil, err
	}
	if _, err := keepChecked(all); err != nil {
		return nil, fmt.Errorf("pin reconciliation failed: %w", err)
	}
	all, err = backupScan(config)
	if err != nil {
		return nil, err
	}
	if _, err := keepChecked(all); err != nil {
		return nil, fmt.Errorf("pin reconciliation failed: %w", err)
	}
	return all, nil
}

func backupScan(store string) ([]*session, error) {
	base := filepath.Join(store, "projects")
	if err := backupNoLinks(base, store); err != nil {
		return nil, err
	}
	st, err := os.Stat(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("session inventory is not a directory: %s", base)
	}
	dirs, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	var list []*session
	for _, dir := range dirs {
		if dir.Type()&os.ModeSymlink != 0 {
			return nil, errors.New("linked project folders are not supported")
		}
		if !dir.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(base, dir.Name()))
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			id := strings.TrimSuffix(entry.Name(), ".jsonl")
			if !strings.HasSuffix(entry.Name(), ".jsonl") || !isUUID(id) {
				continue
			}
			st, err := entry.Info()
			if err != nil {
				return nil, err
			}
			if !st.Mode().IsRegular() {
				return nil, errors.New("session transcript is not a regular file")
			}
			list = append(list, &session{path: filepath.Join(base, dir.Name(), entry.Name()), id: id, mod: st.ModTime()})
		}
	}
	return list, nil
}

func backupLiveWarning(list []*session) (string, error) {
	for _, s := range list {
		if pid := openPid(s.id); pid != "" {
			// shortcut: Windows readers can block Claude's compaction rename; require closure until real compaction is verified.
			if runtime.GOOS == "windows" {
				return "", fmt.Errorf("session %s is running (pid %s); close it before backup on Windows to avoid blocking compaction", s.id, pid)
			}
			return "Warning: running sessions are best-effort live copies, not consistent snapshots.", nil
		}
	}
	return "", nil
}

func backupAllowed(m backupManifest, name string) bool {
	name = strings.TrimSuffix(name, "/")
	for _, s := range m.Sessions {
		for _, p := range parts(s.Category, s.Project, s.ID) {
			p = filepath.ToSlash(p)
			if name == p || (p != s.Category+"/projects/"+s.Project+"/"+s.ID+".jsonl" && strings.HasPrefix(name, p+"/")) {
				return true
			}
		}
	}
	return false
}

func backupWrite(list []*session, destination string, overwrite bool) (err error) {
	if len(list) == 0 {
		return errors.New("no sessions selected")
	}
	if _, err := backupLiveWarning(list); err != nil {
		return err
	}
	if err := backupNoLinks(destination, filepath.Dir(destination)); err != nil {
		return err
	}
	abs, err := backupResolved(destination)
	if err != nil {
		return err
	}
	for _, store := range append([]string{pinned}, config, archived, trash) {
		r, err := backupResolved(store)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(r, abs)
		if err == nil && (rel == "." || filepath.IsLocal(rel)) {
			return errors.New("choose a backup destination outside the session stores")
		}
	}
	var previous fs.FileInfo
	if previous, err = os.Lstat(destination); err == nil {
		if !previous.Mode().IsRegular() {
			return errors.New("choose a regular backup destination")
		}
		if !overwrite {
			return fs.ErrExist
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(destination), ".claude-resume-backup-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	z := zip.NewWriter(f)
	m := backupManifest{Version: 1}
	seen := map[string]string{}
	seenSessions := map[string]bool{}
	for _, s := range list {
		category := ""
		for c, r := range backupStores() {
			if filepath.Clean(root(s)) == filepath.Clean(r) {
				category = c
			}
		}
		if category == "" || !isUUID(s.id) || !backupComponent(folder(s)) {
			return errors.New("unexpected session location")
		}
		sessionKey := strings.ToLower(category + "/" + folder(s) + "/" + s.id)
		if seenSessions[sessionKey] {
			return errors.New("session selected more than once")
		}
		seenSessions[sessionKey] = true
		if err := backupNoLinks(s.path, root(s)); err != nil {
			return err
		}
		st, err := os.Stat(s.path)
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return errors.New("session transcript is not a regular file")
		}
		m.Sessions = append(m.Sessions, backupSession{category, folder(s), s.id, s.pinned, st.ModTime()})
		for partIndex, top := range parts(root(s), folder(s), s.id) {
			if err := backupNoLinks(top, root(s)); err != nil {
				return err
			}
			if _, err := os.Lstat(top); errors.Is(err, fs.ErrNotExist) && partIndex != 0 {
				continue
			} else if err != nil {
				return err
			}
			err = filepath.WalkDir(top, func(p string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				rel, err := filepath.Rel(root(s), p)
				if err != nil {
					return err
				}
				name := category + "/" + filepath.ToSlash(rel)
				for _, part := range strings.Split(name, "/") {
					if !backupComponent(part) {
						return fmt.Errorf("unsupported portable backup name: %s", p)
					}
				}
				key := strings.ToLower(name)
				if prior := seen[key]; prior != "" {
					if prior != name {
						return fmt.Errorf("case-colliding backup paths: %s and %s", prior, name)
					}
					return nil
				}
				info, err := d.Info()
				if err != nil {
					return err
				}
				if !info.IsDir() && !info.Mode().IsRegular() {
					return fmt.Errorf("unsupported session file: %s", p)
				}
				if previous != nil && os.SameFile(previous, info) {
					return errors.New("backup destination aliases session data")
				}
				h, err := zip.FileInfoHeader(info)
				if err != nil {
					return err
				}
				h.Name, h.Method = name, zip.Deflate
				if info.IsDir() {
					h.Name += "/"
				}
				w, err := z.CreateHeader(h)
				if err != nil {
					return err
				}
				seen[key], m.Files = name, append(m.Files, h.Name)
				if info.IsDir() {
					return nil
				}
				return backupCopyFile(w, p, info)
			})
			if err != nil {
				return err
			}
		}
	}
	w, err := z.Create("manifest.json")
	if err == nil {
		err = json.NewEncoder(w).Encode(m)
	}
	if err = errors.Join(err, z.Close()); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := backupNoLinks(destination, filepath.Dir(destination)); err != nil {
		return err
	}
	if previous != nil {
		now, err := os.Lstat(destination)
		if err != nil || !os.SameFile(previous, now) || now.Size() != previous.Size() || !now.ModTime().Equal(previous.ModTime()) {
			return errors.New("backup destination changed; choose the path again")
		}
	} else {
		reserved, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if err := reserved.Close(); err != nil {
			os.Remove(destination)
			return err
		}
	}
	if err := os.Rename(f.Name(), destination); err != nil {
		if previous == nil {
			os.Remove(destination)
		}
		return err
	}
	return nil
}

func backupCopyFile(w io.Writer, p string, info fs.FileInfo) error {
	in, err := os.Open(p)
	if err != nil {
		return err
	}
	opened, statErr := in.Stat()
	if statErr != nil || !os.SameFile(info, opened) {
		in.Close()
		return fmt.Errorf("session file changed before copying: %s", p)
	}
	_, copyErr := io.Copy(w, in)
	return errors.Join(copyErr, in.Close())
}

func backupComponent(p string) bool {
	if p == "" || p == "." || p == ".." || strings.ContainsAny(p, `/\:<>"|?*`) || strings.TrimRight(p, " .") != p {
		return false
	}
	for _, r := range p {
		if r < 32 || r == 127 {
			return false
		}
	}
	base := strings.ToUpper(strings.Split(p, ".")[0])
	return base != "CON" && base != "PRN" && base != "AUX" && base != "NUL" && !(len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9')
}

func backupStage(file string) (backupManifest, string, error) {
	m := backupManifest{}
	z, err := zip.OpenReader(file)
	if err != nil {
		return m, "", err
	}
	defer z.Close()
	entries := map[string]*zip.File{}
	aliases := map[string]bool{}
	for _, f := range z.File {
		clean := strings.TrimSuffix(f.Name, "/")
		if clean == "" || path.Clean(clean) != clean || strings.HasPrefix(clean, "/") {
			return m, "", fmt.Errorf("unsafe ZIP path: %q", f.Name)
		}
		for _, part := range strings.Split(clean, "/") {
			if !backupComponent(part) {
				return m, "", fmt.Errorf("unsafe ZIP path: %q", f.Name)
			}
		}
		if aliases[strings.ToLower(clean)] || (!f.Mode().IsRegular() && !f.Mode().IsDir()) || f.Mode().IsDir() != strings.HasSuffix(f.Name, "/") {
			return m, "", fmt.Errorf("duplicate or unsupported ZIP entry: %q", f.Name)
		}
		if f.Mode().IsDir() && f.UncompressedSize64 != 0 {
			return m, "", errors.New("ZIP directory contains unexpected data")
		}
		aliases[strings.ToLower(clean)], entries[f.Name] = true, f
	}
	f := entries["manifest.json"]
	if f == nil || f.UncompressedSize64 > 16<<20 || !f.Mode().IsRegular() {
		return m, "", errors.New("missing or oversized backup manifest")
	}
	in, err := f.Open()
	if err != nil {
		return m, "", err
	}
	b, err := io.ReadAll(io.LimitReader(in, (16<<20)+1))
	err = errors.Join(err, in.Close())
	if err != nil || len(b) > 16<<20 {
		return m, "", fmt.Errorf("cannot read backup manifest: %v", err)
	}
	if err := json.Unmarshal(b, &m); err != nil || m.Version != 1 || len(m.Sessions) == 0 {
		return m, "", errors.New("invalid or unsupported backup manifest")
	}
	sessions := map[string]bool{}
	for _, s := range m.Sessions {
		key := strings.ToLower(s.Category + "/" + s.Project + "/" + s.ID)
		if backupStores()[s.Category] == "" || !backupComponent(s.Project) || !isUUID(s.ID) || (s.Pinned && s.Category != "live") || s.Modified.IsZero() || sessions[key] {
			return m, "", errors.New("invalid or duplicate session in backup manifest")
		}
		sessions[key] = true
		if entries[s.Category+"/projects/"+s.Project+"/"+s.ID+".jsonl"] == nil {
			return m, "", errors.New("backup is missing a session transcript")
		}
	}
	wanted := map[string]bool{"manifest.json": true}
	for _, name := range m.Files {
		if wanted[name] || entries[name] == nil || !backupAllowed(m, name) {
			return m, "", fmt.Errorf("invalid backup inventory: %q", name)
		}
		wanted[name] = true
	}
	if len(wanted) != len(entries) {
		return m, "", errors.New("ZIP contains undeclared data")
	}
	stagingRoot := filepath.Join(home, ".claude-resume")
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		return m, "", err
	}
	dir, err := os.MkdirTemp(stagingRoot, "restore-*")
	if err != nil {
		return m, "", err
	}
	err = func() error {
		r, err := os.OpenRoot(dir)
		if err != nil {
			return err
		}
		defer r.Close()
		for _, name := range m.Files {
			f := entries[name]
			native := filepath.FromSlash(strings.TrimSuffix(name, "/"))
			if f.Mode().IsDir() {
				if err := r.MkdirAll(native, 0o700); err != nil {
					return err
				}
				continue
			}
			if err := r.MkdirAll(filepath.Dir(native), 0o700); err != nil {
				return err
			}
			out, err := r.OpenFile(native, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			in, err := f.Open()
			if err != nil {
				out.Close()
				return err
			}
			_, copyErr := io.Copy(out, in)
			if err := errors.Join(copyErr, in.Close(), out.Close()); err != nil {
				return err
			}
			if err := r.Chtimes(native, f.Modified, f.Modified); err != nil {
				return err
			}
		}
		return nil
	}()
	if err != nil {
		os.RemoveAll(dir)
		return m, "", err
	}
	return m, dir, nil
}

func backupSummary(s *session) ([]line, error) {
	s.load()
	var total int64
	for _, top := range parts(root(s), folder(s), s.id) {
		err := filepath.WalkDir(top, func(_ string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			st, err := d.Info()
			if err == nil && st.Mode().IsRegular() {
				total += st.Size()
			}
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	reply := "(no readable reply)"
	err := eachLine(s.path, func(int, []byte) bool { return true }, func(_ int, b []byte) bool {
		var r record
		if json.Unmarshal(b, &r) == nil && r.Type == "assistant" && !r.IsApiErrorMessage {
			if t := exportText(r.Message.Content); t != "" {
				reply = printable(t)
			}
		}
		return true
	})
	out := []line{{"1", name(s)}, {"2", date(s) + " · " + human(total) + "B · " + s.id}, {"2", tilde(s.cwd)}, {"1", "Last reply"}}
	for _, l := range strings.Split(reply, "\n") {
		out = append(out, line{"", l})
	}
	return out, err
}

// choose returns overwrite, skip, or cancel; all decisions are collected before installation.
func backupRestore(file string, choose func([]line) string) (string, error) {
	if err := backupNoLinks(file, filepath.Dir(file)); err != nil {
		return "", err
	}
	zipPath, err := backupResolved(file)
	if err != nil {
		return "", err
	}
	m, stage, err := backupStage(file)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	type job struct {
		entry backupSession
		old   *session
		info  fs.FileInfo
	}
	var jobs []job
	var skipped []string
	trashClaims := map[string]bool{}
	stores := backupStores()
	snapshot := func() (map[string]map[string][]*session, error) {
		inventory := map[string]map[string][]*session{}
		for _, store := range []string{config, archived, trash, pinned} {
			list, err := backupScan(store)
			if err != nil {
				return nil, err
			}
			inventory[store] = map[string][]*session{}
			for _, s := range list {
				inventory[store][s.id] = append(inventory[store][s.id], s)
			}
		}
		return inventory, nil
	}
	inventory, err := snapshot()
	if err != nil {
		return "", err
	}
	incomingProjects := map[string]map[string]bool{}
	for _, s := range m.Sessions {
		key := s.Category + "/" + s.ID
		if incomingProjects[key] == nil {
			incomingProjects[key] = map[string]bool{}
		}
		incomingProjects[key][s.Project] = true
	}
	for _, s := range m.Sessions {
		r := stores[s.Category]
		skip := ""
		projectsForID := map[string]bool{s.Project: true}
		for project := range incomingProjects[s.Category+"/"+s.ID] {
			projectsForID[project] = true
		}
		existing := slices.Clone(inventory[r][s.ID])
		if s.Category == "live" {
			existing = append(existing, inventory[pinned][s.ID]...)
		}
		for _, other := range existing {
			if other.id == s.ID {
				projectsForID[folder(other)] = true
			}
		}
		if len(projectsForID) > 1 {
			skip = "duplicate ID across project folders"
		}
		destination := parts(r, s.Project, s.ID)
		var old *session
		var oldInfo fs.FileInfo
		for i, p := range append(slices.Clone(destination), parts(pinned, s.Project, s.ID)...) {
			store := r
			if i >= len(destination) {
				store = pinned
			}
			if err := backupNoLinks(p, store); err != nil {
				return "", err
			}
			st, err := os.Lstat(p)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", err
			}
			if err == nil && p == destination[0] {
				oldInfo = st
				old = &session{path: p, id: s.ID, mod: st.ModTime()}
				if _, err := os.Lstat(parts(pinned, s.Project, s.ID)[0]); s.Category == "live" && err == nil {
					old.pinned = true
				}
			}
			if err == nil && old == nil && skip == "" && slices.Contains(destination, p) && p != destination[0] {
				skip = "destination has orphaned associated data"
			}
		}
		if old != nil {
			if len(inventory[trash][s.ID]) > 0 {
				skip = "trash already contains a session with this ID"
			}
			if trashClaims[s.ID] {
				skip = "another replacement needs this trash recovery location"
			}
			if s.Category == "trash" {
				skip = "existing session is already in trash"
			}
			for _, p := range parts(trash, s.Project, s.ID) {
				if err := backupNoLinks(p, trash); err != nil {
					return "", err
				}
				if _, err := os.Lstat(p); err == nil {
					skip = "trash already contains a recovery copy"
				} else if !errors.Is(err, fs.ErrNotExist) {
					return "", err
				}
			}
		}
		if old == nil && s.Category == "live" {
			for _, p := range parts(pinned, s.Project, s.ID) {
				if _, err := os.Lstat(p); err == nil {
					skip = "pin-backed destination needs reconciliation first"
				}
			}
		}
		for _, p := range destination {
			resolved, err := backupResolved(p)
			if err != nil {
				return "", err
			}
			if rel, err := filepath.Rel(resolved, zipPath); err == nil && (rel == "." || filepath.IsLocal(rel)) {
				return "", errors.New("backup ZIP is inside a restore destination")
			}
		}
		if skip != "" {
			skipped = append(skipped, s.ID+": "+skip)
			continue
		}
		if pid := openPid(s.ID); pid != "" {
			return "", fmt.Errorf("session %s is running (pid %s); close it before restore", s.ID, pid)
		}
		if old != nil {
			incoming := &session{path: parts(filepath.Join(stage, s.Category), s.Project, s.ID)[0], id: s.ID, mod: s.Modified}
			a, err := backupSummary(old)
			if err != nil {
				return "", err
			}
			b, err := backupSummary(incoming)
			if err != nil {
				return "", err
			}
			lines := append([]line{{"1", "Existing session (" + s.Category + ")"}}, a...)
			lines = append(lines, line{}, line{"1", "Backup session"})
			lines = append(lines, b...)
			switch choose(lines) {
			case "overwrite":
			case "skip":
				skipped = append(skipped, s.ID+": skipped")
				continue
			case "terminal":
				return "", errors.New("overwriting needs a terminal to confirm")
			default:
				return "", errors.New("cancelled, no sessions changed")
			}
		}
		jobs = append(jobs, job{s, old, oldInfo})
		if old != nil {
			trashClaims[s.ID] = true
		}
	}
	installed := 0
	result := func() string {
		noun := "sessions"
		if installed == 1 {
			noun = "session"
		}
		msg := fmt.Sprintf("Restored %d %s; skipped %d.", installed, noun, len(skipped))
		if len(skipped) > 0 {
			msg += "\n" + strings.Join(skipped, "\n")
		}
		return msg
	}
	// Decisions can take time; refresh once before installing, then track our own changes.
	inventory, err = snapshot()
	if err != nil {
		return result(), err
	}
	for _, j := range jobs {
		s, r := j.entry, stores[j.entry.Category]
		for _, p := range parts(r, s.Project, s.ID) {
			if err := backupNoLinks(p, r); err != nil {
				return result(), err
			}
		}
		if pid := openPid(s.ID); pid != "" {
			return result(), fmt.Errorf("session %s started running; restore stopped", s.ID)
		}
		currentPool := slices.Clone(inventory[r][s.ID])
		if s.Category == "live" {
			currentPool = append(currentPool, inventory[pinned][s.ID]...)
		}
		ambiguous := false
		for _, current := range currentPool {
			if current.id == s.ID && folder(current) != s.Project {
				ambiguous = true
			}
			if j.old == nil && s.Category == "live" && current.id == s.ID && root(current) == pinned {
				ambiguous = true
			}
		}
		if ambiguous {
			skipped = append(skipped, s.ID+": duplicate or pin-backed destination appeared after review")
			continue
		}
		if j.old != nil {
			if len(inventory[trash][s.ID]) > 0 {
				skipped = append(skipped, s.ID+": trash recovery location became occupied")
				continue
			}
			for _, p := range parts(trash, s.Project, s.ID) {
				if err := backupNoLinks(p, trash); err != nil {
					return result(), err
				}
			}
			st, err := os.Stat(j.old.path)
			if err != nil || !os.SameFile(st, j.info) || st.Size() != j.info.Size() || !st.ModTime().Equal(j.info.ModTime()) {
				return result(), errors.New("destination changed after review; restore stopped")
			}
			if msg, ok := remove([]*session{j.old}, j.old, true, nil, trash); !ok {
				return result(), errors.New(msg)
			}
			inventory[trash][s.ID] = append(inventory[trash][s.ID], &session{path: parts(trash, s.Project, s.ID)[0], id: s.ID})
			if j.old.pinned || s.Category == "live" {
				for _, p := range parts(pinned, s.Project, s.ID) {
					if err := os.RemoveAll(p); err != nil {
						return result(), fmt.Errorf("pin cleanup failed; original is in %s: %w", trash, err)
					}
				}
				delete(inventory[pinned], s.ID)
			}
		}
		err = backupInstall(stage, s, r)
		if err != nil {
			if j.old == nil && errors.Is(err, fs.ErrExist) {
				skipped = append(skipped, s.ID+": destination appeared during restore")
				continue
			}
			if j.old != nil {
				err = fmt.Errorf("%w; original %s is preserved in %s; recover through /list-deleted", err, s.ID, trash)
			}
			return result(), err
		}
		installed++
		st := &session{path: parts(r, s.Project, s.ID)[0], id: s.ID}
		inventory[r][s.ID] = []*session{st}
		if s.Pinned {
			inventory[pinned][s.ID] = []*session{{path: parts(pinned, s.Project, s.ID)[0], id: s.ID}}
		}
	}
	return result(), nil
}

func backupInstall(stage string, s backupSession, destination string) error {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	r, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer r.Close()
	var made []string
	undo := func(cause error) error {
		for _, p := range made {
			cause = errors.Join(cause, r.RemoveAll(p))
		}
		return cause
	}
	for i, src := range parts(filepath.Join(stage, s.Category), s.Project, s.ID) {
		if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) && i != 0 {
			continue
		} else if err != nil {
			return undo(err)
		}
		rel, _ := filepath.Rel(filepath.Join(stage, s.Category), src)
		if _, err := r.Lstat(rel); err == nil {
			return undo(fmt.Errorf("destination appeared during restore (%s): %w", rel, fs.ErrExist))
		} else if !errors.Is(err, fs.ErrNotExist) {
			return undo(err)
		}
		info, err := os.Stat(src)
		if err != nil {
			return undo(err)
		}
		if info.IsDir() {
			if err := r.MkdirAll(filepath.Dir(rel), 0o700); err != nil {
				return undo(err)
			}
			if err := r.Mkdir(rel, 0o700); err != nil {
				return undo(err)
			}
			made = append(made, rel)
		}
		err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			suffix, _ := filepath.Rel(src, p)
			target := filepath.Join(rel, suffix)
			if d.IsDir() {
				return r.MkdirAll(target, 0o700)
			}
			if err := r.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			out, err := r.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			if p == src {
				made = append(made, rel)
			}
			in, err := os.Open(p)
			if err != nil {
				out.Close()
				return err
			}
			_, copyErr := io.Copy(out, in)
			if err := errors.Join(copyErr, in.Close(), out.Close()); err != nil {
				return err
			}
			st, err := d.Info()
			if err != nil {
				return err
			}
			return r.Chtimes(target, st.ModTime(), st.ModTime())
		})
		if err != nil {
			return undo(err)
		}
	}
	st := &session{path: parts(destination, s.Project, s.ID)[0], id: s.ID}
	when := s.Modified
	if s.Category != "archive" {
		when = time.Now()
	}
	for _, p := range made {
		if err := r.Chtimes(p, when, when); err != nil {
			return undo(err)
		}
	}
	if s.Pinned {
		if _, err := link(s.ID, s.Project); err != nil {
			unpin(st)
			return undo(err)
		}
	}
	return nil
}

func backupChoose(t *tty, lines []line) string {
	v := &view{fixed: true, items: []string{"Skip", "Overwrite", "Cancel"},
		preview: func(int) []line { return lines }, foot: "↑↓ choose · ENTER confirm · ESC cancel"}
	k, i := t.run(v)
	if k != "enter" {
		return "cancel"
	}
	return []string{"skip", "overwrite", "cancel"}[i]
}

func backupCLI(action, file string, ids []string) error {
	interactive := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	if action == "--restore-backup" {
		var t *tty
		defer func() {
			if t != nil {
				t.close()
			}
		}()
		msg, err := backupRestore(file, func(lines []line) string {
			if !interactive {
				return "terminal"
			}
			if t == nil {
				t = openTTY()
			}
			return backupChoose(t, lines)
		})
		if t != nil {
			t.close()
			t = nil
		}
		if msg != "" {
			fmt.Println(printable(msg))
		}
		return err
	}
	all, err := backupPool()
	if err != nil {
		return err
	}
	list := all
	if len(ids) > 0 {
		pool := slices.Clone(all)
		for _, store := range []string{archived, trash} {
			sessions, err := backupScan(store)
			if err != nil {
				return err
			}
			pool = append(pool, sessions...)
		}
		list = nil
		for _, id := range ids {
			s, err := find(pool, id)
			if err != nil {
				return err
			}
			if !slices.Contains(list, s) {
				list = append(list, s)
			}
		}
	}
	warning, err := backupLiveWarning(list)
	if err != nil {
		return err
	}
	if warning != "" {
		fmt.Fprintln(os.Stderr, warning)
	}
	file, err = backupPath(file)
	if err != nil {
		return err
	}
	err = backupWrite(list, file, false)
	if errors.Is(err, fs.ErrExist) && interactive {
		t := openTTY()
		confirmed := t.confirmDialog("Backup sessions", []line{{"1", "Overwrite this backup?"}, {"", printable(file)}})
		t.close()
		if !confirmed {
			return errors.New("cancelled, nothing was backed up")
		}
		err = backupWrite(list, file, true)
	} else if errors.Is(err, fs.ErrExist) {
		return errors.New("overwriting a backup needs a terminal to confirm")
	}
	if err == nil {
		fmt.Println("Saved " + printable(file))
	}
	return err
}

func (t *tty) backupDialog(action string, selected *session) string {
	suggested := backupName()
	if action == "restore-backup" {
		suggested = ""
	}
	v := &view{items: []string{"Session ZIP"}, edit: true, query: []rune(suggested), foot: "/" + action + " · Type the ZIP path · ENTER continue · ESC cancel"}
	v.cur = len(v.query)
	for {
		if k, _ := t.run(v); k != "enter" {
			return "Cancelled."
		}
		if action == "restore-backup" {
			file := string(v.query)
			if strings.TrimSpace(file) == "" {
				v.header = []string{"Enter a ZIP path."}
				continue
			}
			msg, err := backupRestore(file, func(lines []line) string { return backupChoose(t, lines) })
			if err != nil {
				return printable(msg + " " + err.Error())
			}
			return printable(msg)
		}
		all, err := backupPool()
		if err != nil {
			return printable(err.Error())
		}
		list := all
		if selected != nil {
			list = []*session{selected}
		}
		warning, err := backupLiveWarning(list)
		if err != nil {
			return printable(err.Error())
		}
		if warning != "" {
			if !t.confirmDialog("Live backup", []line{{"", warning}}) {
				return "Cancelled."
			}
		}
		file, err := backupPath(string(v.query))
		if err == nil {
			err = backupWrite(list, file, false)
			if errors.Is(err, fs.ErrExist) && t.confirmDialog("Backup sessions", []line{{"1", "Overwrite this backup?"}, {"", printable(file)}}) {
				err = backupWrite(list, file, true)
			}
		}
		if err != nil {
			v.header = []string{printable(err.Error())}
			continue
		}
		return "Saved " + printable(file)
	}
}
