package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

type tty struct {
	out  *bufio.Writer
	old  *term.State
	keys chan []byte
	wake chan struct{}
	pend []byte
	w, h int
}

func openTTY() *tty {
	old, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		die("cannot use the terminal: %v", err)
	}
	enableVT()
	t := &tty{out: bufio.NewWriterSize(os.Stdout, 64<<10), old: old, keys: make(chan []byte, 64), wake: make(chan struct{}, 1)}
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(t.keys)
				return
			}
			t.keys <- append([]byte(nil), buf[:n]...)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	go func() {
		<-sig
		t.close()
		os.Exit(143)
	}()
	t.out.WriteString("\x1b[?1049h")
	cleanup = t.close
	return t
}

func (t *tty) close() {
	t.out.WriteString("\x1b[?25h\x1b[?1049l")
	t.out.Flush()
	term.Restore(int(os.Stdin.Fd()), t.old)
	cleanup = func() {}
}

func (t *tty) size() (int, int) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w < 1 || h < 1 {
		return 80, 24
	}
	return w, h
}

func (t *tty) resized() bool {
	w, h := t.size()
	return w != t.w || h != t.h
}

func (t *tty) drain() {
	t.pend = nil
	for {
		select {
		case <-t.keys:
		default:
			return
		}
	}
}

func (t *tty) poke() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

var csi = map[string]string{
	"A": "up", "B": "down", "C": "right", "D": "left", "H": "home", "F": "end",
	"1;2A": "shift-up", "1;2B": "shift-down", "5~": "pgup", "6~": "pgdn", "3~": "del",
	"1~": "home", "4~": "end", "7~": "home", "8~": "end",
}

var ctrl = map[byte]string{
	0x00: "ctrl-space", 0x01: "ctrl-a", 0x02: "left", 0x03: "esc", 0x04: "ctrl-d", 0x05: "end", 0x06: "right",
	0x08: "bs", 0x7f: "bs", 0x0a: "down", 0x0b: "up", 0x0d: "enter", 0x0e: "down", 0x10: "up",
	0x12: "ctrl-r", 0x14: "ctrl-t", 0x15: "ctrl-u", 0x17: "ctrl-w",
}

// read returns the next key, "wake" when a preview has loaded, or "" after a quiet interval.
func (t *tty) read() string {
	for len(t.pend) == 0 {
		select {
		case b, ok := <-t.keys:
			if !ok {
				return "esc"
			}
			t.pend = append(t.pend, b...)
		case <-t.wake:
			return "wake"
		case <-time.After(250 * time.Millisecond):
			return ""
		}
	}
	p := t.pend
	if p[0] == 0x1b {
		if len(p) == 1 {
			select {
			case b := <-t.keys:
				p = append(p, b...)
			case <-time.After(30 * time.Millisecond):
			}
		}
		if len(p) == 1 {
			t.pend = nil
			return "esc"
		}
		if p[1] != '[' && p[1] != 'O' {
			t.pend = p[2:]
			return ""
		}
		i := 2
		for i < len(p) && (p[i] < 0x40 || p[i] > 0x7e) {
			i++
		}
		if i == len(p) {
			t.pend = nil
			return ""
		}
		t.pend = p[i+1:]
		return csi[string(p[2:i+1])]
	}
	if p[0] < 0x20 || p[0] == 0x7f {
		t.pend = p[1:]
		return ctrl[p[0]]
	}
	r, n := utf8.DecodeRune(p)
	t.pend = p[n:]
	if r == utf8.RuneError {
		return ""
	}
	return string(r)
}

type view struct {
	prompt  string
	items   []string
	bold    map[int]bool
	header  []string
	foot    string
	preview func(i int) []line
	keys    map[string]bool
	edit    bool
	fixed   bool
	warn    string

	query     []rune
	cur       int
	sel, top  int
	dim, hide bool
	pscroll   int
	shown     []int
	pos       [][]int
}

func (v *view) filter() {
	terms := strings.Fields(string(v.query))
	if v.edit || v.fixed {
		terms = nil
	}
	v.shown, v.pos = v.shown[:0], v.pos[:0]
	for i, it := range v.items {
		if p, ok := match(it, terms); ok {
			v.shown = append(v.shown, i)
			v.pos = append(v.pos, p)
		}
	}
	v.sel = max(0, min(v.sel, len(v.shown)-1))
}

func (t *tty) run(v *view) (string, int) {
	v.filter()
	redraw := true
	for {
		if redraw || t.resized() {
			t.draw(v)
		}
		k := t.read()
		if v.fixed && !slices.Contains([]string{"", "wake", "esc", "enter", "up", "down", "pgup", "pgdn", "shift-up", "shift-down", "ctrl-space"}, k) {
			continue
		}
		redraw = k != ""
		n := len(v.shown)
		edited := false
		switch k {
		case "", "wake":
			continue
		case "esc":
			return k, -1
		case "ctrl-a":
			if v.keys[k] {
				return k, -1
			}
		case "enter", "ctrl-d", "ctrl-r", "ctrl-t":
			if n > 0 && (k == "enter" || v.keys[k]) {
				return k, v.shown[v.sel]
			}
		case "up":
			v.dim = v.sel > 0
			v.sel = max(0, v.sel-1)
			v.pscroll = 0
		case "down":
			v.dim = true
			v.sel = min(max(n-1, 0), v.sel+1)
			v.pscroll = 0
		case "pgup", "pgdn":
			_, h := t.size()
			step := max(1, h/3)
			if k == "pgup" {
				step = -step
			}
			v.sel = max(0, min(n-1, v.sel+step))
			v.pscroll = 0
		case "shift-up":
			v.pscroll = max(0, v.pscroll-1)
		case "shift-down":
			v.pscroll++
		case "ctrl-space":
			v.hide = !v.hide
		case "left":
			v.cur, v.dim = max(0, v.cur-1), false
		case "right":
			v.cur, v.dim = min(len(v.query), v.cur+1), false
		case "home":
			v.cur, v.dim = 0, false
		case "end":
			v.cur, v.dim = len(v.query), false
		case "bs":
			if v.cur > 0 {
				v.query = append(v.query[:v.cur-1], v.query[v.cur:]...)
				v.cur--
				edited = true
			}
		case "del":
			if v.cur < len(v.query) {
				v.query = append(v.query[:v.cur], v.query[v.cur+1:]...)
				edited = true
			}
		case "ctrl-u":
			v.query = append([]rune(nil), v.query[v.cur:]...)
			v.cur, edited = 0, true
		case "ctrl-w":
			i := v.cur
			for i > 0 && v.query[i-1] == ' ' {
				i--
			}
			for i > 0 && v.query[i-1] != ' ' {
				i--
			}
			v.query = append(v.query[:i], v.query[v.cur:]...)
			v.cur, edited = i, true
		default:
			if k == "/" && (len(v.query) == 0 || v.dim) && v.keys[k] {
				if n == 0 {
					return k, -1
				}
				return k, v.shown[v.sel]
			}
			if utf8.RuneCountInString(k) == 1 {
				r := []rune(k)[0]
				v.query = append(v.query[:v.cur], append([]rune{r}, v.query[v.cur:]...)...)
				v.cur++
				edited = true
			}
		}
		if edited {
			v.dim, v.sel, v.top, v.pscroll = false, 0, 0, 0
			v.filter()
		}
	}
}

func clip(s string, w int) string {
	if width(s) > w {
		return cut(s, max(0, w-2)) + "··"
	}
	return s
}

func highlight(s string, pos []int, hl, normal string) string {
	var b strings.Builder
	j := 0
	for i, r := range []rune(s) {
		for j < len(pos) && pos[j] < i {
			j++
		}
		if j < len(pos) && pos[j] == i {
			b.WriteString(hl + string(r) + normal)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func wrap(lines []line, w int) []line {
	w = max(w, 3)
	var out []line
	for _, l := range lines {
		s := cut(l.text, w)
		out = append(out, line{l.style, s})
		for rest := l.text[len(s):]; rest != ""; rest = rest[len(s):] {
			if s = cut(rest, w-2); s == "" {
				_, n := utf8.DecodeRuneInString(rest)
				s = rest[:n]
			}
			out = append(out, line{"cont" + l.style, s})
		}
	}
	return out
}

func (t *tty) draw(v *view) {
	w, h := t.size()
	t.w, t.h = w, h
	var rows []string
	p := "\x1b[1;38;5;110m" + v.prompt
	if v.dim {
		p = "\x1b[1;2;38;5;110m" + v.prompt
	}
	rows = append(rows, p+"\x1b[0;1m"+clip(string(v.query), max(0, w-width(v.prompt)))+"\x1b[0m")
	info := fmt.Sprintf("%d/%d", len(v.shown), len(v.items))
	rows = append(rows, "  \x1b[38;5;144m"+info+"\x1b[38;5;59m "+strings.Repeat("─", max(0, w-3-len(info)))+"\x1b[0m")
	for _, hd := range v.header {
		rows = append(rows, "  \x1b[38;5;109m"+clip(hd, w-2)+"\x1b[0m")
	}

	ph := 0
	if v.preview != nil && !v.hide && h >= 10 {
		ph = h * 60 / 100
	}
	area := max(1, h-len(rows)-ph-1)
	if v.sel < v.top {
		v.top = v.sel
	}
	if v.sel >= v.top+area {
		v.top = v.sel - area + 1
	}
	for i := v.top; i < v.top+area; i++ {
		if i >= len(v.shown) {
			rows = append(rows, "")
			continue
		}
		s := clip(v.items[v.shown[i]], w-2)
		if i == v.sel {
			bg := "236"
			if v.warn == red {
				bg = "52"
			}
			rows = append(rows, "\x1b[1;38;5;161;48;5;"+bg+"m▌\x1b[0;48;5;"+bg+"m \x1b[1;38;5;254m"+
				highlight(s, v.pos[i], "\x1b[38;5;151m", "\x1b[38;5;254m")+strings.Repeat(" ", max(0, w-2-width(s)))+"\x1b[0m")
		} else {
			b := ""
			if v.bold[v.shown[i]] {
				b = "\x1b[1m"
			}
			rows = append(rows, "\x1b[38;5;236m▌\x1b[0m "+b+highlight(s, v.pos[i], "\x1b[38;5;108m", "\x1b[39m")+"\x1b[0m")
		}
	}

	if ph > 0 {
		inner := w - 4
		fr := "\x1b[38;5;59m"
		if v.warn != "" {
			fr = "\x1b[" + v.warn + "m"
		}
		rows = append(rows, fr+"╭"+strings.Repeat("─", max(0, w-2))+"╮\x1b[0m")
		i := -1
		if len(v.shown) > 0 {
			i = v.shown[v.sel]
		}
		body := wrap(v.preview(i), inner)
		v.pscroll = min(v.pscroll, max(0, len(body)-(ph-2)))
		for i := v.pscroll; i < v.pscroll+ph-2; i++ {
			s, style := "", ""
			if i < len(body) {
				s, style = body[i].text, body[i].style
			}
			pad := strings.Repeat(" ", max(0, inner-width(s)))
			if strings.HasPrefix(style, "cont") {
				style = style[4:]
				pad = pad[min(len(pad), 2):]
				s = "\x1b[2m↳ \x1b[0m" + "\x1b[" + style + "m" + s
			} else {
				s = "\x1b[" + style + "m" + s
			}
			rows = append(rows, fr+"│\x1b[0m "+s+"\x1b[0m"+pad+" "+fr+"│\x1b[0m")
		}
		rows = append(rows, fr+"╰"+strings.Repeat("─", max(0, w-2))+"╯\x1b[0m")
	}

	b := t.out
	b.WriteString("\x1b[?25l")
	for i := 0; i < h; i++ {
		s := ""
		if i < len(rows) {
			s = rows[i]
		}
		if i == h-1 {
			fc := "\x1b[38;5;145m"
			if v.warn != "" {
				fc = "\x1b[1;" + v.warn + "m"
			}
			s = fc + clip(v.foot, w) + "\x1b[0m"
			right, color := label(), "\x1b[38;5;243m"
			if note != "" && w >= width(note)+30 {
				right, color = note, "\x1b[33m"
			}
			if w >= width(right)+30 {
				f := clip(v.foot, w-width(right)-2)
				s = fc + f + "\x1b[0m" + strings.Repeat(" ", w-width(f)-width(right)) + color + right + "\x1b[0m"
			}
		}
		fmt.Fprintf(b, "\x1b[%d;1H\x1b[K%s", i+1, s)
	}
	if !v.dim {
		fmt.Fprintf(b, "\x1b[1;%dH\x1b[?25h", min(w, width(v.prompt)+width(string(v.query[:v.cur]))+1))
	}
	b.Flush()
}

func folders(all []*session) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range all {
		if c := tilde(s.cwd); !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

const red, orange = "31", "38;5;215"

type command struct{ name, key, about string }

var cmds = map[string][]command{
	"": {
		{"/archive", "archive", "move the session to the archive, where it never expires"},
		{"/rename", "ctrl-r", "rename the session (CTRL-R)"},
		{"/fork", "fork", "resume a copy of the session and leave the original as it is"},
		{"/export", "export", "save the session's prompts and replies as Markdown"},
		{"/delete", "ctrl-d", "move the session to the trash for 30 days (CTRL-D)"},
		{"/list-folders", "ctrl-a", "show the sessions of one folder (CTRL-A)"},
	},
	"archive": {
		{"/restore", "restore", "move the session back to the session list"},
		{"/rename", "ctrl-r", "rename the session (CTRL-R)"},
		{"/export", "export", "save the session's prompts and replies as Markdown"},
		{"/delete", "ctrl-d", "move the session to the trash for 30 days (CTRL-D)"},
		{"/list-folders", "ctrl-a", "show the archived sessions of one folder (CTRL-A)"},
	},
	"trash": {
		{"/restore", "restore", "move the session back to the session list"},
		{"/export", "export", "save the session's prompts and replies as Markdown"},
		{"/delete", "ctrl-d", "delete the session for good (CTRL-D)"},
		{"/empty-trash", "empty-trash", "delete all deleted sessions for good"},
		{"/list-folders", "ctrl-a", "show the deleted sessions of one folder (CTRL-A)"},
	},
}

var lists = []command{
	{"/backup-all", "backup-all", "back up all live sessions as a ZIP"},
	{"/restore-backup", "restore-backup", "restore sessions from a ZIP backup"},
	{"/list-sessions", "list:", "show the sessions"},
	{"/list-archives", "list:archive", "show the archived sessions"},
	{"/list-deleted", "list:trash", "show the deleted sessions"},
	{"/help", "help", "show all commands, shortcuts and command-line options"},
	{"/exit", "exit", "quit claude-resume"},
}

var shortcuts = [][2]string{
	{"↑↓ PGUP PGDN", "move through the list"},
	{"ENTER", "resume the session; in the archive and trash, restore and resume it"},
	{"CTRL-T", "pin or unpin the session"},
	{"CTRL-R", "rename the session"},
	{"CTRL-D", "delete the session"},
	{"CTRL-A", "show the sessions of one folder"},
	{"CTRL-SPACE", "show or hide the preview"},
	{"SHIFT-↑↓", "scroll the preview"},
	{"CTRL-U CTRL-W", "delete the search before the cursor, or the word before it"},
	{"ESC", "go back, or quit"},
}

func commands(mode string) []command {
	list := slices.Clone(cmds[mode])
	list = append(list, command{"/backup", "backup", "back up the selected session as a ZIP"})
	for _, c := range lists {
		if c.key != "list:"+mode {
			list = append(list, c)
		}
	}
	return list
}

func help(mode string) []line {
	l := []line{{"1", "Commands · type / on an empty search line, or after moving with the arrow keys"}}
	if mode == "" {
		l = append(l, line{"", fmt.Sprintf("  %-16s%s", "/pin, /unpin", "keep a session at the top and safe from Claude Code's cleanup (CTRL-T)")})
	}
	for _, c := range commands(mode) {
		l = append(l, line{"", fmt.Sprintf("  %-16s%s", c.name, c.about)})
	}
	l = append(l, line{}, line{"1", "Shortcuts"})
	for _, s := range shortcuts {
		if s[0] != "CTRL-T" || mode == "" {
			l = append(l, line{"", fmt.Sprintf("  %-16s%s", s[0], s[1])})
		}
	}
	l = append(l, line{}, line{"1", "Command line"})
	for _, u := range strings.Split(strings.TrimSpace(usageText), "\n") {
		l = append(l, line{"", "  " + strings.TrimSpace(strings.TrimPrefix(u, "usage:"))})
	}
	return l
}

var note string

func pick(all []*session) {
	note = notice()
	t := openTTY()
	loadAll(all)
	arch, bin := scan(filepath.Join(archived, "projects")), scan(filepath.Join(trash, "projects"))
	pools := map[string]*[]*session{"": &all, "archive": &arch, "trash": &bin}
	var mu sync.Mutex
	cache := map[string][]line{}
	loading := map[string]bool{}
	previewGeneration := 0
	var shown []*session
	v := &view{prompt: "search> ", keys: map[string]bool{"ctrl-a": true, "ctrl-d": true, "ctrl-r": true, "ctrl-t": true, "/": true}}
	v.preview = func(i int) []line {
		if i < 0 {
			return nil
		}
		s := shown[i]
		mu.Lock()
		defer mu.Unlock()
		if l, ok := cache[s.path]; ok {
			return l
		}
		if !loading[s.path] {
			loading[s.path] = true
			generation := previewGeneration
			go func() {
				l := preview(s)
				mu.Lock()
				if generation == previewGeneration {
					cache[s.path] = l
				}
				mu.Unlock()
				t.poke()
			}()
		}
		return nil
	}
	dir, msg, choose, mode := "", "", false, ""
	var follow *session
	var exported string
	show := func(m string) {
		mode, dir, msg, v.sel, v.top, v.query, v.cur = m, "", "", 0, 0, nil, 0
	}
	drop := func(pool *[]*session, s *session) {
		*pool = slices.DeleteFunc(*pool, func(o *session) bool { return o == s })
	}
	add := func(pool *[]*session, s *session) {
		*pool = append(*pool, s)
		order(*pool)
	}
	ask := func(warn string) func(string, []line) bool {
		return func(q string, lines []line) bool {
			saved := *v
			defer func() { *v = saved }()
			v.preview, v.foot, v.warn, v.dim, v.hide, v.pscroll = func(int) []line { return lines }, q, warn, true, false, 0
			redraw := true
			for {
				if redraw || t.resized() {
					t.draw(v)
				}
				k := t.read()
				redraw = false
				if k != "" && k != "wake" {
					return k == "y" || k == "Y"
				}
			}
		}
	}
	for {
		pool := pools[mode]
		loadAll(*pool)
		if choose {
			f := &view{prompt: "folder> ", items: folders(*pool), foot: "↑↓ select · ENTER show its sessions · ESC all sessions"}
			k, i := t.run(f)
			dir = ""
			if k == "enter" {
				dir = f.items[i]
			}
			msg, choose, v.sel, v.top = "", false, 0, 0
		}
		shown = shown[:0]
		for _, s := range *pool {
			if dir == "" || tilde(s.cwd) == dir {
				shown = append(shown, s)
			}
		}
		if len(shown) == 0 {
			dir, shown = "", append(shown, *pool...)
		}
		v.items, v.bold = rows(shown), map[int]bool{}
		for j, s := range shown {
			v.bold[j] = s.pinned
		}
		if follow != nil {
			v.filter()
			for x, j := range v.shown {
				if shown[j] == follow {
					v.sel = x
				}
			}
			follow = nil
		}
		v.header = nil
		switch mode {
		case "archive":
			v.header = append(v.header, "archived sessions")
		case "trash":
			v.header = append(v.header, "deleted sessions, kept for 30 days after the delete")
		}
		if dir != "" {
			v.header = append(v.header, "folder: "+dir)
		}
		if msg != "" {
			v.header = append(v.header, msg)
		}
		if len(v.header) == 0 {
			v.header = []string{""}
		}
		esc, enter, list := "quit", "resume", commands(mode)
		if mode != "" {
			esc, enter = "all sessions", "restore and resume"
		}
		if dir != "" {
			esc = "folders"
		}
		pinKey := ""
		if mode == "" {
			pinKey = " · CTRL-T pin"
		}
		v.foot = "Type to search · / commands · ↑↓ select · ENTER " + enter + pinKey + " · CTRL-R rename · CTRL-D delete · CTRL-SPACE preview · CTRL-A folders · ESC " + esc
		if exported != "" {
			v.foot = "Saved " + exported
		}
		k, i := t.run(v)
		exported = ""
		if k == "/" && i < 0 {
			list = slices.DeleteFunc(list, func(c command) bool {
				return !strings.HasPrefix(c.key, "list:") && c.key != "exit" && c.key != "help" && c.key != "backup-all" && c.key != "restore-backup"
			})
		}
		if k == "/" && i >= 0 && mode == "" {
			c := command{"/pin", "pin", "keep the session at the top, safe from Claude Code's cleanup (CTRL-T)"}
			if shown[i].pinned {
				c = command{"/unpin", "unpin", "stop keeping the session at the top and safe (CTRL-T)"}
			}
			list = append([]command{c}, list...)
		}
		if k == "/" && len(list) > 0 {
			k = t.command(v, list)
		}
		if k == "ctrl-t" {
			k = "pin"
			if shown[i].pinned {
				k = "unpin"
			}
			if mode != "" {
				k = ""
			}
		}
		if m, ok := strings.CutPrefix(k, "list:"); ok {
			show(m)
			continue
		}
		switch k {
		case "backup", "backup-all", "restore-backup":
			var selected *session
			if k == "backup" {
				selected = shown[i]
			}
			msg = t.backupDialog(k, selected)
			all, arch, bin = scan(projects), scan(filepath.Join(archived, "projects")), scan(filepath.Join(trash, "projects"))
			keep(all)
			order(all)
			mu.Lock()
			cache = map[string][]line{}
			loading = map[string]bool{}
			previewGeneration++
			mu.Unlock()
		case "esc":
			if dir != "" {
				choose = true
			} else if mode != "" {
				show("")
			} else {
				t.close()
				os.Exit(0)
			}
		case "exit":
			t.close()
			os.Exit(0)
		case "help":
			saved, text := *v, help(mode)
			v.preview, v.foot, v.dim, v.hide, v.pscroll = func(int) []line { return text }, "↑↓ PGUP PGDN scroll · any other key goes back", true, false, 0
		scroll:
			for redraw := true; ; {
				if redraw || t.resized() {
					t.draw(v)
				}
				_, h := t.size()
				k := t.read()
				redraw = true
				switch k {
				case "", "wake":
					redraw = false
				case "up", "shift-up":
					v.pscroll = max(0, v.pscroll-1)
				case "down", "shift-down":
					v.pscroll++
				case "pgup":
					v.pscroll = max(0, v.pscroll-h/3)
				case "pgdn":
					v.pscroll += h / 3
				default:
					break scroll
				}
			}
			*v = saved
		case "ctrl-a":
			choose = true
		case "enter":
			if mode != "" {
				var ok bool
				if msg, ok = restore(*pool, shown[i]); !ok {
					break
				}
			}
			t.close()
			resume(shown[i])
		case "fork":
			t.close()
			resume(shown[i], "--fork-session")
		case "pin", "unpin":
			ok := true
			if k == "pin" {
				msg, ok = pin(shown[i])
			} else {
				msg = unpin(shown[i])
			}
			if ok {
				order(all)
				follow = shown[i]
			}
		case "archive", "restore":
			s, ok := shown[i], false
			if k == "archive" {
				msg, ok = archive(all, s)
			} else {
				msg, ok = restore(*pool, s)
			}
			if ok {
				to := &all
				if k == "archive" {
					to = &arch
				}
				drop(pool, s)
				add(to, s)
			}
		case "ctrl-r":
			s := shown[i]
			e := &view{prompt: "rename> ", items: v.items[i : i+1], edit: true, hide: v.hide, query: []rune(s.title),
				preview: func(int) []line { return v.preview(i) }, foot: "Type the new title · ENTER save · ESC cancel"}
			e.cur = len(e.query)
			msg = ""
			if k, _ := t.run(e); k == "enter" {
				msg, _ = rename(s, string(e.query))
				mu.Lock()
				delete(cache, s.path)
				delete(loading, s.path)
				mu.Unlock()
			}
			v.hide = e.hide
		case "export":
			s := shown[i]
			e := &view{prompt: "export> ", items: v.items[i : i+1], edit: true, hide: v.hide, query: []rune(exportName(s)),
				preview: func(int) []line { return v.preview(i) }, foot: "Type the path · ENTER save · ESC cancel"}
			e.cur = len(e.query)
			msg = ""
			for {
				if k, _ := t.run(e); k != "enter" {
					break
				}
				path, err := exportPath(s, string(e.query))
				if err != nil {
					e.header = []string{"Not exported: " + printable(err.Error())}
					continue
				}
				err = exportSession(s, path, false)
				if errors.Is(err, fs.ErrExist) {
					t.drain()
					if !ask(orange)("Overwrite this file? · Y confirm · any other key edits the path", []line{{"", printable(path)}}) {
						continue
					}
					err = exportSession(s, path, true)
				}
				if err != nil {
					e.header = []string{"Not exported: " + printable(err.Error())}
					continue
				}
				exported = printable(path)
				break
			}
			v.hide = e.hide
		case "ctrl-d":
			t.drain()
			s, to, color, ok := shown[i], trash, orange, false
			if mode == "trash" {
				to, color = "", red
			}
			if msg, ok = remove(*pool, s, false, ask(color), to); ok {
				drop(pool, s)
				if to != "" {
					add(&bin, s)
				}
			}
		case "empty-trash":
			t.drain()
			var total int64
			for _, s := range bin {
				for _, p := range own(bin, s) {
					total += size(p)
				}
			}
			lines := []line{{"1;31", "WARNING: this permanently deletes every session below. It cannot be undone."}, {}}
			for _, r := range rows(bin) {
				lines = append(lines, line{"", r})
			}
			msg = "Cancelled, nothing was deleted."
			if ask(red)(fmt.Sprintf("Empty the trash? %sB · Y confirm · any other key cancels", human(total)), lines) {
				n, err := purge(bin, time.Now().Add(time.Hour))
				if msg = fmt.Sprintf("Emptied the trash: %d deleted for good.", n); err != nil {
					msg = "Only partly emptied the trash: " + err.Error()
				}
				bin = scan(filepath.Join(trash, "projects"))
			}
		}
	}
}

func (t *tty) command(v *view, cmds []command) string {
	saved := *v
	defer func() { *v = saved }()
	q, sel := "/", 0
	for redraw := true; ; {
		var match []command
		for _, c := range cmds {
			if strings.Contains(c.name, q[1:]) {
				match = append(match, c)
			}
		}
		sel = max(0, min(sel, len(match)-1))
		if redraw || t.resized() {
			var lines []line
			for j, c := range match {
				var pos []int
				for p := strings.Index(c.name, q[1:]); p < len(c.name) && len(pos) < len(q)-1; p++ {
					pos = append(pos, p)
				}
				l := line{"", "  " + highlight(c.name, pos, "\x1b[1m", "\x1b[22m") + strings.Repeat(" ", 16-len(c.name)) + c.about}
				if j == sel {
					l = line{"38;5;151", "› " + l.text[2:]}
				}
				lines = append(lines, l)
			}
			v.query, v.cur, v.dim, v.hide, v.pscroll = []rune(q), len(q), false, false, 0
			v.preview, v.foot = func(int) []line { return lines }, "Type a command · ↑↓ select · ENTER run · ESC back"
			t.draw(v)
		}
		k := t.read()
		redraw = k != "" && k != "wake"
		switch k {
		case "esc":
			return ""
		case "enter":
			if len(match) > 0 {
				return match[sel].key
			}
		case "up":
			sel--
		case "down":
			sel++
		case "bs":
			_, n := utf8.DecodeLastRuneInString(q)
			if q = q[:len(q)-n]; q == "" {
				return ""
			}
		default:
			if utf8.RuneCountInString(k) == 1 {
				q, sel = q+strings.ToLower(k), 0
			}
		}
	}
}

func (t *tty) confirm(_ string, lines []line) bool {
	return t.confirmDialog("Delete session", lines)
}

func (t *tty) confirmDialog(title string, lines []line) bool {
	t.drain()
	sel, last, redraw := 0, time.Now(), true
	for {
		if redraw || t.resized() {
			t.dialog(title, lines, sel)
		}
		k := t.read()
		redraw = k != ""
		switch k {
		case "", "wake":
			if time.Since(last) > 10*time.Minute {
				return false
			}
			continue
		case "left":
			sel = 0
		case "right":
			sel = 1
		case "enter":
			return sel == 1
		case "esc", "q":
			return false
		}
		last = time.Now()
	}
}

func (t *tty) dialog(title string, lines []line, sel int) {
	cols, rows := t.size()
	t.w, t.h = cols, rows
	w := min(cols-4, 96)
	if w < 30 {
		w = min(cols, 30)
	}
	inner := w - 4
	avail := rows - 2
	tail := 1
	if avail >= 4 {
		tail = 2
	}
	pad := 0
	if avail-tail >= len(lines)+2 {
		pad = 1
	}
	n := max(0, min(avail-tail-2*pad, len(lines)))
	for n > 0 && lines[n-1].text == "" {
		n--
	}
	h := n + tail + 2*pad + 2
	top, left := 1, 1
	if rows > h {
		top = (rows-h)/2 + 1
	}
	if cols > w {
		left = (cols-w)/2 + 1
	}
	b := t.out
	r := top
	row := func(style, s string) {
		if width(s) > inner {
			s = cut(s, inner-1) + "…"
		}
		fmt.Fprintf(b, "\x1b[%d;%dH\x1b[31m│\x1b[0m \x1b[%sm%s\x1b[0m\x1b[%d;%dH\x1b[31m│\x1b[0m", r, left, style, s, r, left+w-1)
		r++
	}
	label, hint := " "+title+" ", " ←→ select · ENTER choose · ESC cancel "
	if width(label) > w-4 {
		label = ""
	}
	if width(hint) > w-6 {
		hint = ""
	}
	b.WriteString("\x1b[?25l\x1b[2J")
	fmt.Fprintf(b, "\x1b[%d;%dH\x1b[31m╭──%s%s╮\x1b[0m", r, left, label, strings.Repeat("─", max(0, w-4-width(label))))
	r++
	if pad == 1 {
		row("0", "")
	}
	for _, l := range lines[:n] {
		row(l.style, l.text)
	}
	if tail == 2 {
		row("0", "")
	}
	b0, b1, gap := "[ CANCEL ]", "[ CONFIRM ]", "   "
	if inner < 24 {
		b0, b1, gap = "[CANCEL]", "[CONFIRM]", " "
	}
	lpad := max(0, (inner-len(b0)-len(gap)-len(b1))/2)
	fmt.Fprintf(b, "\x1b[%d;%dH\x1b[31m│\x1b[0m %*s", r, left, lpad, "")
	if sel == 0 {
		fmt.Fprintf(b, "\x1b[7m%s\x1b[0m%s\x1b[31m%s\x1b[0m", b0, gap, b1)
	} else {
		fmt.Fprintf(b, "%s%s\x1b[1;7;31m%s\x1b[0m", b0, gap, b1)
	}
	fmt.Fprintf(b, "\x1b[%d;%dH\x1b[31m│\x1b[0m", r, left+w-1)
	r++
	if pad == 1 {
		row("0", "")
	}
	fmt.Fprintf(b, "\x1b[%d;%dH\x1b[31m╰──%s%s╯\x1b[0m", r, left, hint, strings.Repeat("─", max(0, w-4-width(hint))))
	b.Flush()
}
