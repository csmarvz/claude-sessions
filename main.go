package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/term"
)

const usageText = `usage: claude-resume                           pick a session, or list the last 10 outside a terminal
       claude-resume -a                        list all sessions (--all)
       claude-resume <id>                      resume a session in its folder (a unique ID prefix is enough)
       claude-resume -r <id> <title>           rename a session (--rename)
       claude-resume --export <id> [path]      save a session's prompts and replies as Markdown
       claude-resume --backup <zip> [<id>...]  back up explicit sessions, or all live sessions
       claude-resume --restore-backup <zip>    restore sessions from a backup
       claude-resume -d [-y] <id>              move a session to the trash for 30 days; asks first unless -y (--delete, --yes)
       claude-resume --archive <id>            archive a session so that it never expires
       claude-resume --restore <id>            move an archived or deleted session back to the session list
       claude-resume --pin <id>                keep a session at the top of the list and safe from Claude Code's cleanup
       claude-resume --unpin <id>              unpin a session
       claude-resume update                    install the latest release
       claude-resume -v                        show the installed version (--version)
       claude-resume -h                        show the command-line help (--help)
`

var (
	home     string
	config   string
	projects string
	archived string
	trash    string
	pinned   string
	cleanup  = func() {}

	customMark     = []byte(`"type":"custom-title"`)
	aiMark         = []byte(`"type":"ai-title"`)
	userMark       = []byte(`"type":"user"`)
	assistantMark  = []byte(`"type":"assistant"`)
	toolResultMark = []byte(`"type":"tool_result"`)
)

type session struct {
	path, id   string
	mod        time.Time
	cwd, title string
	loaded     bool
	pinned     bool
}

type record struct {
	Type              string `json:"type"`
	Cwd               string `json:"cwd"`
	CustomTitle       string `json:"customTitle"`
	AiTitle           string `json:"aiTitle"`
	IsMeta            bool   `json:"isMeta"`
	IsCompactSummary  bool   `json:"isCompactSummary"`
	IsApiErrorMessage bool   `json:"isApiErrorMessage"`
	GitBranch         string `json:"gitBranch"`
	Message           struct {
		Content json.RawMessage `json:"content"`
		Usage   struct {
			Input       int `json:"input_tokens"`
			CacheCreate int `json:"cache_creation_input_tokens"`
			CacheRead   int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

type line struct{ style, text string }

func isHex(s string) bool { return s != "" && strings.Trim(s, "0123456789abcdef-") == "" }

func isUUID(s string) bool {
	return len(s) == 36 && isHex(s) && strings.Count(s, "-") == 4 && s[8] == '-' && s[13] == '-' && s[18] == '-' && s[23] == '-'
}

func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if r != '\n' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

func squeeze(s string) string {
	var b strings.Builder
	space := false
	for _, r := range printable(s) {
		if unicode.IsSpace(r) {
			if !space {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

func usage(code int) {
	fmt.Fprint(os.Stderr, usageText)
	os.Exit(code)
}

func die(format string, a ...any) {
	cleanup()
	fmt.Fprintf(os.Stderr, "claude-resume: "+format+"\n", a...)
	os.Exit(1)
}

func main() {
	args := append(os.Args[1:], "")
	arg, target, title, yes := args[0], "", "", false
	var output string
	switch {
	case arg == "-h" || arg == "--help":
		usage(0)
	case arg == "-v" || arg == "--version":
		fmt.Println("claude-resume " + currentVersion())
		return
	case arg == "update":
		update()
		return
	case arg == "" || arg == "-a" || arg == "--all":
	case arg == "--backup" || arg == "--restore-backup":
		if len(args) < 3 || args[1] == "" || (arg == "--restore-backup" && len(args) != 3) {
			usage(1)
		}
		for _, id := range args[2 : len(args)-1] {
			if !isHex(id) {
				usage(1)
			}
		}
	case arg == "--export":
		if target = args[1]; (len(args) != 3 && len(args) != 4) || !isHex(target) {
			usage(1)
		}
		if len(args) == 4 {
			output = args[2]
		}
	case arg == "-d" || arg == "--delete":
		for _, a := range args[1 : len(args)-1] {
			if a == "-y" || a == "--yes" {
				yes = true
				continue
			}
			if target != "" {
				usage(1)
			}
			target = a
		}
		if !isHex(target) {
			usage(1)
		}
		if yes && !isUUID(target) {
			die("-y needs the full session ID")
		}
	case arg == "-r" || arg == "--rename":
		if len(args) < 4 {
			usage(1)
		}
		target, title = args[1], strings.Join(args[2:len(args)-1], " ")
		if !isHex(target) || strings.TrimSpace(title) == "" {
			usage(1)
		}
	case arg == "--archive" || arg == "--restore" || arg == "--pin" || arg == "--unpin":
		if target = args[1]; len(args) != 3 || !isHex(target) {
			usage(1)
		}
	case strings.HasPrefix(arg, "-") || !isHex(arg):
		usage(1)
	}

	removeOld()
	home, _ = os.UserHomeDir()
	config = os.Getenv("CLAUDE_CONFIG_DIR")
	if config == "" {
		config = filepath.Join(home, ".claude")
	}
	shown := filepath.Join(config, "projects")
	if abs, err := filepath.Abs(config); err == nil {
		config = abs
	}
	if st, err := os.Stat(config); (err != nil || !st.IsDir()) && !((arg == "--export" || arg == "--backup" || arg == "--restore-backup" || arg == "") && errors.Is(err, fs.ErrNotExist)) {
		die("no sessions found in %s", shown)
	}
	projects = filepath.Join(config, "projects")
	archived = filepath.Join(home, ".claude-resume", "archive")
	trash = filepath.Join(home, ".claude-resume", "trash")
	pinned = filepath.Join(home, ".claude-resume", "pinned")
	if arg == "--backup" || arg == "--restore-backup" {
		if err := backupCLI(arg, args[1], args[2:len(args)-1]); err != nil {
			die("%s", printable(err.Error()))
		}
		return
	}
	if arg == "--export" {
		all := append(scan(projects), scan(filepath.Join(archived, "projects"))...)
		all = append(all, scan(filepath.Join(trash, "projects"))...)
		s := mustFind(all, target)
		if output == "" {
			output = exportName(s)
		}
		output, err := exportPath(s, output)
		if err != nil {
			die("not exported: %s", printable(err.Error()))
		}
		err = exportSession(s, output, false)
		if errors.Is(err, fs.ErrExist) {
			if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
				die("overwriting needs a terminal to confirm; choose another export path")
			}
			t := openTTY()
			confirmed := t.confirmDialog("Export session", []line{{"1", "Overwrite this export?"}, {}, {"", printable(output)}})
			t.close()
			if !confirmed {
				die("cancelled, nothing was exported")
			}
			err = exportSession(s, output, true)
		}
		if err != nil {
			die("not exported: %s", printable(err.Error()))
		}
		fmt.Println("Saved " + printable(output))
		return
	}
	purge(scan(filepath.Join(trash, "projects")), time.Now().AddDate(0, 0, -30))
	all := scan(projects)
	if keep(all) {
		all = scan(projects)
		keep(all)
	}
	order(all)
	if len(all) == 0 && arg != "--restore" && arg != "" {
		die("no sessions found in %s", projects)
	}

	stdin, stdout := term.IsTerminal(int(os.Stdin.Fd())), term.IsTerminal(int(os.Stdout.Fd()))
	switch arg {
	case "-a", "--all":
		list(all, len(all))
	case "-d", "--delete":
		if !yes && !stdin {
			die("deleting needs a terminal to confirm; add -y to skip the confirmation")
		}
		s := mustFind(all, target)
		var t *tty
		if !yes {
			t = openTTY()
		}
		msg, ok := remove(all, s, yes, t.confirm, trash)
		if t != nil {
			t.close()
		}
		if !ok {
			die("%s", msg)
		}
		fmt.Println(msg)
	case "--archive":
		msg, ok := archive(all, mustFind(all, target))
		if !ok {
			die("%s", msg)
		}
		fmt.Println(msg)
	case "--pin", "--unpin":
		s, msg, ok := mustFind(all, target), "", true
		if arg == "--pin" {
			msg, ok = pin(s)
		} else {
			msg = unpin(s)
		}
		if !ok {
			die("%s", msg)
		}
		fmt.Println(msg)
	case "--restore":
		pool := append(scan(filepath.Join(archived, "projects")), scan(filepath.Join(trash, "projects"))...)
		msg, ok := restore(pool, mustFind(pool, target))
		if !ok {
			die("%s", msg)
		}
		fmt.Println(msg)
	case "-r", "--rename":
		msg, ok := rename(mustFind(all, target), title)
		if !ok {
			die("%s", msg)
		}
		fmt.Println(msg)
	case "":
		if stdin && stdout {
			pick(all)
		} else {
			list(all, 10)
		}
	default:
		resume(mustFind(all, arg))
	}
}

func scan(projects string) []*session {
	var list []*session
	dirs, _ := os.ReadDir(projects)
	for _, d := range dirs {
		files, _ := os.ReadDir(filepath.Join(projects, d.Name()))
		for _, f := range files {
			name := f.Name()
			id := strings.TrimSuffix(name, ".jsonl")
			if !strings.HasSuffix(name, ".jsonl") || !isUUID(id) || !f.Type().IsRegular() {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			list = append(list, &session{path: filepath.Join(projects, d.Name(), name), id: id, mod: info.ModTime()})
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].mod.After(list[j].mod) })
	return list
}

func eachLine(path string, want func(n int, head []byte) bool, fn func(n int, line []byte) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	for n := 0; ; n++ {
		chunk, err := r.ReadSlice('\n')
		if len(chunk) == 0 && err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		keep := want(n, chunk)
		line := chunk
		if err == bufio.ErrBufferFull {
			var full []byte
			if keep {
				full = append(full, chunk...)
			}
			for err == bufio.ErrBufferFull {
				chunk, err = r.ReadSlice('\n')
				if keep {
					full = append(full, chunk...)
				}
			}
			line = full
		}
		if keep && !fn(n, line) {
			return nil
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func text(raw json.RawMessage, sep string) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var parts []struct{ Type, Text string }
	if json.Unmarshal(raw, &parts) != nil {
		return "", false
	}
	var out []string
	for _, p := range parts {
		if p.Type == "tool_result" {
			return "", false
		}
		if p.Type == "text" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, sep), true
}

func prompt(r *record) string {
	if r.Type != "user" {
		return ""
	}
	t, ok := text(r.Message.Content, " ")
	if !ok || strings.HasPrefix(t, "<") {
		return ""
	}
	return t
}

func (s *session) load() {
	var custom, ai, first, cwd string
	eachLine(s.path, func(n int, head []byte) bool {
		return n < 100 || bytes.Contains(head, customMark) || bytes.Contains(head, aiMark)
	}, func(n int, b []byte) bool {
		var r record
		json.Unmarshal(b, &r)
		if r.CustomTitle != "" {
			custom = r.CustomTitle
		}
		if r.AiTitle != "" {
			ai = r.AiTitle
		}
		if n < 100 {
			if cwd == "" {
				cwd = r.Cwd
			}
			if first == "" {
				first = prompt(&r)
			}
		}
		return true
	})
	if cwd == "" {
		cwd = "?"
	}
	title := custom
	if title == "" {
		title = ai
	}
	if title == "" {
		title = first
	}
	s.cwd, s.title, s.loaded = cwd, squeeze(title), true
}

func loadAll(list []*session) {
	ch := make(chan *session)
	var wg sync.WaitGroup
	for i := 0; i < runtime.NumCPU()*2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range ch {
				s.load()
			}
		}()
	}
	for _, s := range list {
		if !s.loaded {
			ch <- s
		}
	}
	close(ch)
	wg.Wait()
}

func find(all []*session, prefix string) (*session, error) {
	var m []*session
	for _, s := range all {
		if strings.HasPrefix(s.id, prefix) {
			m = append(m, s)
		}
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("no session %s", prefix)
	}
	for _, s := range m {
		if s.id != m[0].id {
			return nil, fmt.Errorf("%s matches %d sessions, use more of the ID", prefix, len(m))
		}
	}
	if len(m) > 1 {
		return nil, fmt.Errorf("session %s is in %d project folders; pick one in the claude-resume picker", m[0].id, len(m))
	}
	return m[0], nil
}

func mustFind(all []*session, prefix string) *session {
	s, err := find(all, prefix)
	if err != nil {
		die("%s", err)
	}
	return s
}

func tilde(p string) string {
	if home != "" && (p == home || strings.HasPrefix(p, home+string(filepath.Separator))) {
		p = "~" + p[len(home):]
	}
	return squeeze(p)
}

func date(s *session) string { return s.mod.Format("2006-01-02 15:04") }

// ponytail: wide ranges cover CJK and common emoji only; emoji sequences joined with U+200D count as several glyphs.
var wide = [][2]rune{
	{0x1100, 0x115F}, {0x231A, 0x231B}, {0x2329, 0x232A}, {0x23E9, 0x23EC}, {0x23F0, 0x23F0}, {0x23F3, 0x23F3},
	{0x25FD, 0x25FE}, {0x2614, 0x2615}, {0x2648, 0x2653}, {0x267F, 0x267F}, {0x2693, 0x2693}, {0x26A1, 0x26A1},
	{0x26AA, 0x26AB}, {0x26BD, 0x26BE}, {0x26C4, 0x26C5}, {0x26CE, 0x26CE}, {0x26D4, 0x26D4}, {0x26EA, 0x26EA},
	{0x26F2, 0x26F3}, {0x26F5, 0x26F5}, {0x26FA, 0x26FA}, {0x26FD, 0x26FD}, {0x2705, 0x2705}, {0x270A, 0x270B},
	{0x2728, 0x2728}, {0x274C, 0x274C}, {0x274E, 0x274E}, {0x2753, 0x2755}, {0x2757, 0x2757}, {0x2795, 0x2797},
	{0x27B0, 0x27B0}, {0x27BF, 0x27BF}, {0x2B1B, 0x2B1C}, {0x2B50, 0x2B50}, {0x2B55, 0x2B55}, {0x2E80, 0x303E},
	{0x3041, 0x33FF}, {0x3400, 0x4DBF}, {0x4E00, 0x9FFF}, {0xA000, 0xA4CF}, {0xA960, 0xA97F}, {0xAC00, 0xD7A3},
	{0xF900, 0xFAFF}, {0xFE10, 0xFE19}, {0xFE30, 0xFE6F}, {0xFF00, 0xFF60}, {0xFFE0, 0xFFE6}, {0x1B000, 0x1B2FF},
	{0x1F004, 0x1F004}, {0x1F0CF, 0x1F0CF}, {0x1F18E, 0x1F18E}, {0x1F191, 0x1F19A}, {0x1F200, 0x1F251},
	{0x1F300, 0x1F64F}, {0x1F680, 0x1F6FF}, {0x1F7E0, 0x1F7EB}, {0x1F90C, 0x1F9FF}, {0x1FA70, 0x1FAFF},
	{0x20000, 0x3FFFD},
}

func runeWidth(r rune) int {
	if r < 0x300 {
		return 1
	}
	if r == 0x200D || unicode.In(r, unicode.Mn, unicode.Me) || unicode.Is(unicode.Variation_Selector, r) {
		return 0
	}
	for _, w := range wide {
		if r >= w[0] && r <= w[1] {
			return 2
		}
	}
	return 1
}

func cut(s string, n int) string {
	w, esc := 0, false
	for i, r := range s {
		if esc = esc || r == 0x1b; esc {
			esc = r != 'm'
			continue
		}
		if w += runeWidth(r); w > n {
			return s[:i]
		}
	}
	return s
}

func width(s string) int {
	w, esc := 0, false
	for _, r := range s {
		if esc = esc || r == 0x1b; esc {
			esc = r != 'm'
			continue
		}
		w += runeWidth(r)
	}
	return w
}

func rows(list []*session) []string {
	tw, cw, mark := 0, 0, slices.ContainsFunc(list, func(s *session) bool { return s.pinned })
	for _, s := range list {
		tw = max(tw, width(cut(s.title, 50)))
		cw = max(cw, width(tilde(s.cwd)))
	}
	out := make([]string, len(list))
	for i, s := range list {
		t, c, p := cut(s.title, 50), tilde(s.cwd), ""
		if mark {
			p = "  "
		}
		if s.pinned {
			p = "★ "
		}
		out[i] = fmt.Sprintf("%s%s  %s%*s  %s%*s  %s", p, date(s), t, tw-width(t), "", c, cw-width(c), "", s.id)
	}
	return out
}

func list(all []*session, n int) {
	sub := all[:min(n, len(all))]
	loadAll(sub)
	w := bufio.NewWriter(os.Stdout)
	for _, r := range rows(sub) {
		fmt.Fprintln(w, r)
	}
	w.Flush()
}

func preview(s *session) []line {
	reply, branch, tokens := "", "", 0
	var prompts []string
	seen := map[string]bool{}
	eachLine(s.path, func(int, []byte) bool { return true }, func(_ int, b []byte) bool {
		isUser := bytes.Contains(b, userMark) && !bytes.Contains(b, toolResultMark)
		if !isUser && !bytes.Contains(b, assistantMark) {
			return true
		}
		var r record
		json.Unmarshal(b, &r)
		if r.GitBranch != "" {
			branch = squeeze(r.GitBranch)
		}
		if u := r.Message.Usage; u.Input+u.CacheCreate+u.CacheRead > 0 {
			tokens = u.Input + u.CacheCreate + u.CacheRead
		}
		if r.Type == "assistant" && bytes.HasPrefix(r.Message.Content, []byte("[")) {
			if t, _ := text(r.Message.Content, "\n"); t != "" {
				reply = printable(t)
			}
		}
		if isUser && !r.IsMeta && !r.IsCompactSummary {
			if p := cut(squeeze(prompt(&r)), 300); p != "" && !seen[p] {
				seen[p] = true
				prompts = append(prompts, p)
			}
		}
		return true
	})
	title := s.title
	if title == "" {
		title = "(untitled)"
	}
	if reply == "" {
		reply = "(none)"
	}
	var n int64
	for _, p := range files(s) {
		n += size(p)
	}
	meta := []string{human(n) + "B"}
	if branch != "" && branch != "HEAD" {
		meta = append([]string{branch}, meta...)
	}
	if tokens > 0 {
		meta = append(meta, fmt.Sprintf("%dk tokens", (tokens+500)/1000))
	}
	out := []line{{"1", title}, {"2", tilde(s.cwd) + "  ·  " + date(s) + "  ·  " + s.id}, {"2", strings.Join(meta, "  ·  ")}, {}, {"1", "Last reply"}}
	for _, l := range strings.Split(cut(reply, 1500), "\n") {
		out = append(out, line{"", l})
	}
	out = append(out, line{}, line{"1", "Prompts"})
	if len(prompts) == 0 {
		out = append(out, line{"", "(none)"})
	}
	for _, p := range prompts {
		out = append(out, line{"", "› " + p})
	}
	return out
}

func openPid(id string) string {
	files, _ := filepath.Glob(filepath.Join(config, "sessions", "*.json"))
	for _, f := range files {
		var s struct{ SessionId string }
		b, _ := os.ReadFile(f)
		if json.Unmarshal(b, &s) != nil || s.SessionId != id {
			continue
		}
		pid := strings.TrimSuffix(filepath.Base(f), ".json")
		var n int
		if _, err := fmt.Sscan(pid, &n); err == nil && isClaude(n) {
			return pid
		}
	}
	return ""
}

func root(s *session) string { return filepath.Dir(filepath.Dir(filepath.Dir(s.path))) }

func folder(s *session) string { return filepath.Base(filepath.Dir(s.path)) }

func parts(root, folder, id string) []string {
	return []string{filepath.Join(root, "projects", folder, id+".jsonl"), filepath.Join(root, "projects", folder, id), filepath.Join(root, "file-history", id), filepath.Join(root, "session-env", id)}
}

func files(s *session) []string {
	var paths []string
	for _, p := range parts(root(s), folder(s), s.id) {
		if _, err := os.Lstat(p); err == nil {
			paths = append(paths, p)
		}
	}
	return paths
}

func own(pool []*session, s *session) []string {
	paths := files(s)
	for _, o := range pool {
		if o != s && o.id == s.id {
			paths = slices.DeleteFunc(paths, func(p string) bool { return filepath.Dir(p) != filepath.Dir(s.path) })
		}
	}
	return paths
}

func size(p string) int64 {
	var n int64
	filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err == nil {
			if i, err := d.Info(); err == nil && !d.IsDir() {
				n += i.Size()
			}
		}
		return nil
	})
	return n
}

func human(n int64) string {
	f, i := float64(n), 0
	for f >= 1024 && i < 4 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprint(n)
	}
	if f < 10 {
		return fmt.Sprintf("%.1f%c", math.Ceil(f*10)/10, "KMGT"[i-1])
	}
	return fmt.Sprintf("%.0f%c", math.Ceil(f), "KMGT"[i-1])
}

func remove(all []*session, s *session, yes bool, ask func(string, []line) bool, to string) (string, bool) {
	if !isUUID(s.id) {
		return "Not deleted: unexpected session file " + s.path, false
	}
	if pid := openPid(s.id); pid != "" {
		return fmt.Sprintf("Not deleted: session %s is open in Claude Code (pid %s). Close it first.", s.id[:8], pid), false
	}
	if !s.loaded {
		s.load()
	}
	paths := own(all, s)
	if len(paths) == 0 {
		return "Not deleted: no files found for session " + s.id, false
	}
	if !yes {
		title := s.title
		if title == "" {
			title = "(untitled)"
		}
		warn, list, q := line{"1;31", "WARNING: this permanently deletes the session below. It cannot be undone."}, "Files to delete in ", "Delete this session for good? %sB · Y confirm · any other key cancels"
		if to != "" {
			warn, list, q = line{"", "This moves the session below to the trash, where it stays for 30 days."}, "Files to move to the trash from ", "Move this session to the trash? %sB · Y confirm · any other key cancels"
		}
		lines := []line{
			warn, {}, {"1", title}, {"2", tilde(s.cwd) + "  ·  " + date(s) + "  ·  " + s.id}, {},
			{"", list + tilde(root(s)) + ":"},
		}
		var total int64
		for _, p := range paths {
			n := size(p)
			total += n
			rel, _ := filepath.Rel(root(s), p)
			lines = append(lines, line{"", fmt.Sprintf("  %6s  %s", human(n), squeeze(rel))})
		}
		if !ask(fmt.Sprintf(q, human(total)), lines) {
			return "Cancelled, nothing was deleted.", false
		}
	}
	if pid := openPid(s.id); pid != "" {
		return fmt.Sprintf("Not deleted: session %s was opened in Claude Code (pid %s). Close it first.", s.id[:8], pid), false
	}
	if to != "" {
		dsts, err := move(all, s, to)
		if errors.Is(err, fs.ErrExist) {
			return "Not deleted: the trash already has a session with this ID.", false
		}
		if err != nil {
			return "Not deleted: " + err.Error(), false
		}
		renew(s, dsts)
		if s.pinned {
			unpin(s)
		}
		return "Moved \"" + name(s) + "\" to the trash.", true
	}
	failed := false
	for _, p := range paths {
		if err := os.RemoveAll(p); err != nil {
			fmt.Fprintln(os.Stderr, err)
			failed = true
		}
	}
	if failed {
		return "Only partly deleted \"" + name(s) + "\", see the errors above.", false
	}
	return "Deleted \"" + name(s) + "\".", true
}

func rename(s *session, title string) (string, bool) {
	if title = strings.TrimSpace(squeeze(title)); title == "" {
		return "Not renamed: the title is empty.", false
	}
	if pid := openPid(s.id); pid != "" {
		return fmt.Sprintf("Not renamed: session %s is open in Claude Code (pid %s). Close it first.", s.id[:8], pid), false
	}
	st, err := os.Stat(s.path)
	if err != nil {
		return err.Error(), false
	}
	f, err := os.OpenFile(s.path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		return err.Error(), false
	}
	b, _ := json.Marshal(struct {
		Type        string `json:"type"`
		CustomTitle string `json:"customTitle"`
		SessionId   string `json:"sessionId"`
	}{"custom-title", title, s.id})
	last := make([]byte, 1)
	if n, _ := f.ReadAt(last, st.Size()-1); n == 1 && last[0] != '\n' {
		b = append([]byte("\n"), b...)
	}
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err.Error(), false
	}
	os.Chtimes(s.path, st.ModTime(), st.ModTime())
	s.title = title
	return "Renamed to \"" + title + "\".", true
}

func move(pool []*session, s *session, to string) ([]string, error) {
	from := root(s)
	var done, dsts []string
	for _, p := range own(pool, s) {
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if _, err := os.Lstat(dst); err == nil {
			return nil, fs.ErrExist
		}
		dsts = append(dsts, dst)
	}
	for _, dst := range dsts {
		rel, _ := filepath.Rel(to, dst)
		err := os.MkdirAll(filepath.Dir(dst), 0o700)
		if err == nil {
			err = os.Rename(filepath.Join(from, rel), dst)
		}
		if err != nil {
			for _, d := range done {
				rel, _ := filepath.Rel(to, d)
				os.Rename(d, filepath.Join(from, rel))
			}
			return nil, err
		}
		done = append(done, dst)
	}
	rel, _ := filepath.Rel(from, s.path)
	s.path = filepath.Join(to, rel)
	return dsts, nil
}

func name(s *session) string {
	if !s.loaded {
		s.load()
	}
	if s.title == "" {
		return s.id
	}
	return s.title
}

func archive(pool []*session, s *session) (string, bool) {
	if pid := openPid(s.id); pid != "" {
		return fmt.Sprintf("Not archived: session %s is open in Claude Code (pid %s). Close it first.", s.id[:8], pid), false
	}
	if _, err := move(pool, s, archived); errors.Is(err, fs.ErrExist) {
		return "Not archived: the archive already has a session with this ID.", false
	} else if err != nil {
		return "Not archived: " + err.Error(), false
	}
	if s.pinned {
		unpin(s)
	}
	return "Archived \"" + name(s) + "\".", true
}

func restore(pool []*session, s *session) (string, bool) {
	dsts, err := move(pool, s, config)
	if errors.Is(err, fs.ErrExist) {
		return "Not restored: the session list already has a session with this ID.", false
	}
	if err != nil {
		return "Not restored: " + err.Error(), false
	}
	renew(s, dsts)
	return "Restored \"" + name(s) + "\".", true
}

func renew(s *session, paths []string) {
	s.mod = time.Now()
	for _, p := range paths {
		os.Chtimes(p, s.mod, s.mod)
	}
}

func purge(pool []*session, before time.Time) (int, error) {
	n, failed := 0, error(nil)
	for _, s := range pool {
		if !s.mod.Before(before) {
			continue
		}
		for _, p := range own(pool, s) {
			if err := os.RemoveAll(p); err != nil && failed == nil {
				failed = err
			}
		}
		n++
	}
	return n, failed
}

func order(list []*session) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].pinned != list[j].pinned {
			return list[i].pinned
		}
		return list[i].mod.After(list[j].mod)
	})
}

func link(id, folder string) (bool, error) {
	live, pin, back := parts(config, folder, id), parts(pinned, folder, id), false
	for i, p := range append(slices.Clone(live), pin...) {
		store := config
		if i >= len(live) {
			store = pinned
		}
		if err := backupNoLinks(p, store); err != nil {
			return false, err
		}
	}
	for i := range live {
		rels := map[string]bool{}
		for _, top := range []string{live[i], pin[i]} {
			err := filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				if err != nil {
					return err
				}
				if d.Type()&os.ModeSymlink != 0 {
					return fmt.Errorf("linked pin data is not supported: %s", p)
				}
				if err == nil && !d.IsDir() {
					info, err := d.Info()
					if err != nil {
						return err
					}
					if !info.Mode().IsRegular() {
						return fmt.Errorf("unsupported pin data: %s", p)
					}
					rel, _ := filepath.Rel(top, p)
					rels[rel] = true
				}
				return nil
			})
			if err != nil {
				return back, err
			}
		}
		restored := false
		for rel := range rels {
			l, p := filepath.Join(live[i], rel), filepath.Join(pin[i], rel)
			li, lerr := os.Lstat(l)
			pi, perr := os.Lstat(p)
			from, to := l, p
			switch {
			case lerr == nil && perr == nil && os.SameFile(li, pi):
				continue
			case errors.Is(lerr, fs.ErrNotExist) && perr == nil:
				from, to, restored = p, l, true
			case lerr != nil:
				continue
			}
			os.Remove(to)
			if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
				return back, err
			}
			if err := os.Link(from, to); err != nil {
				return back, err
			}
		}
		if restored {
			now := time.Now()
			os.Chtimes(live[i], now, now)
			back = true
		}
	}
	return back, nil
}

func keep(all []*session) bool {
	back, err := keepChecked(all)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-resume: could not keep pinned sessions: %v\n", err)
	}
	return back
}

func keepChecked(all []*session) (bool, error) {
	pins, err := backupScan(pinned)
	if err != nil {
		return false, err
	}
	byID := map[string][]*session{}
	for _, s := range all {
		byID[s.id] = append(byID[s.id], s)
	}
	back := false
	var failed error
	for _, p := range pins {
		f := folder(p)
		var live *session
		for _, s := range byID[p.id] {
			if folder(s) == f {
				live = s
			}
		}
		if live == nil && len(byID[p.id]) == 1 {
			live = byID[p.id][0]
			old := parts(pinned, f, p.id)
			os.Remove(old[0])
			os.RemoveAll(old[1])
			f = folder(live)
		}
		b, err := link(p.id, f)
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("session %s: %w", p.id, err))
		}
		if live != nil {
			live.pinned = true
		}
		back = back || b
	}
	return back, failed
}

func pin(s *session) (string, bool) {
	if root(s) != config {
		return "Not pinned: only sessions in the session list can be pinned.", false
	}
	if _, err := link(s.id, folder(s)); err != nil {
		unpin(s)
		return "Not pinned: " + err.Error(), false
	}
	s.pinned = true
	return "Pinned \"" + name(s) + "\".", true
}

func unpin(s *session) string {
	for _, p := range parts(pinned, folder(s), s.id) {
		os.RemoveAll(p)
	}
	if root(s) == config && s.mod.Before(time.Now().AddDate(0, 0, -30)) {
		renew(s, files(s))
	}
	s.pinned = false
	return "Unpinned \"" + name(s) + "\"."
}

func resume(s *session, flags ...string) {
	cwd := ""
	eachLine(s.path, func(n int, _ []byte) bool { return n < 100 }, func(_ int, b []byte) bool {
		var r record
		json.Unmarshal(b, &r)
		cwd = r.Cwd
		return cwd == ""
	})
	if cwd == "" {
		die("no folder recorded for session %s", s.id)
	}
	if err := os.Chdir(cwd); errors.Is(err, fs.ErrNotExist) {
		here, _ := os.Getwd()
		fmt.Fprintf(os.Stderr, "Folder %s no longer exists; resuming in %s.\n", printable(cwd), printable(here))
	} else if err != nil {
		die("cannot open folder %s: %v", printable(cwd), err)
	}
	execClaude(append([]string{"--resume", s.id}, flags...)...)
}
