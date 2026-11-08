// Package policy rates how risky a shell command is. The rating is only a
// label for the audit log and the activity stream; nothing is blocked.
package policy

import (
	"path"
	"strings"

	"github.com/hua-ri/coxswain/internal/action"
	"mvdan.cc/sh/v3/syntax"
)

// readOnly lists commands that only read state, whatever their
// arguments. Commands that are read-only only with some subcommands are in
// readOnlySub; commands whose arguments decide (rm, sed, find, awk, tee,
// ip, sysctl…) are handled in classifyArgs and argChecks.
var readOnly = set("cat", "head", "tail", "grep", "egrep", "fgrep", "zgrep", "zcat",
	"ls", "ll", "df", "du", "free", "uptime", "uname", "whoami", "id", "groups", "w", "who", "last",
	"ps", "pgrep", "pidof", "top", "htop", "netstat", "lsof", "vmstat", "iostat",
	"mpstat", "sar", "stat", "file", "wc", "cut", "tr", "echo",
	"printf", "env", "printenv", "which", "type", "nproc", "lscpu", "lsblk", "blkid", "lsmod", "lspci", "lsusb",
	"getent", "ping", "dig", "nslookup", "host", "traceroute", "tracepath", "md5sum", "sha1sum", "sha256sum",
	"readlink", "realpath", "basename", "dirname", "pwd", "test", "true", "false", "seq", "column", "jq",
	"diff", "cmp", "findmnt", "getenforce", "sestatus", "locale", "cal", "arch", "nl", "rev", "tac",
	"strings", "od", "hexdump", "cd", "pushd", "popd")

// readOnlySub: commands that are read-only only with these subcommands.
var readOnlySub = map[string]map[string]bool{
	"systemctl": set("status", "is-active", "is-enabled", "is-failed", "list-units", "list-unit-files",
		"list-timers", "show", "cat", "list-dependencies", "show-environment"),
	"docker": set("ps", "logs", "inspect", "images", "version", "info", "top", "port", "diff", "history"),
	"git":    set("status", "log", "diff", "show", "branch", "remote", "rev-parse", "ls-files"),
	"apt":    set("list", "show", "search", "policy"),
	"yum":    set("list", "info", "search"),
	"rpm":    set("-q", "-qa", "-qi", "-ql"),
	"dpkg":   set("-l", "-L", "-s"),
}

// destructiveCmd are always destructive regardless of arguments.
var destructiveCmd = set("mkfs", "wipefs", "shutdown", "reboot", "halt", "poweroff", "init", "telinit", "fdisk",
	"parted", "sfdisk", "mkswap", "eval", "exec", "source", ".")

// systemctlDestructive are systemctl verbs that stop or switch the machine.
var systemctlDestructive = set("reboot", "poweroff", "halt", "kexec", "isolate", "emergency", "rescue",
	"suspend", "hibernate", "default")

// wrappers run another command; we classify the wrapped command.
var wrappers = set("sudo", "doas", "env", "timeout", "nice", "time", "command", "builtin", "stdbuf", "ionice",
	"xargs", "setsid")

// protectedRoots may not be written, nor anything below them, without a
// destructive rating.
var protectedRoots = []string{"/etc", "/usr", "/boot", "/bin", "/sbin", "/lib", "/lib64", "/proc", "/sys",
	"/root", "/var/spool/cron", "/var/lib"}

// protectedDevices are block device prefixes.
var protectedDevices = []string{"/dev/sd", "/dev/nvme", "/dev/vd", "/dev/disk"}

// protectedDirs and protectedFiles are protected wherever they are.
var (
	protectedDirs  = set(".ssh", ".kube", ".aws", ".gnupg")
	protectedFiles = set("authorized_keys", "sudoers", ".bashrc", ".bash_profile", ".bash_login", ".zshrc", ".zprofile",
		".zshenv", ".profile")
)

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// ClassifyCommand rates a shell command line. Anything it cannot parse
// or whose command name is computed at run time is rated Destructive so
// that it is always confirmed; unknown commands are Write.
func ClassifyCommand(cmd string) action.Risk {
	return rater{}.classify(cmd, 0)
}

// rater carries context through one classification. cd is set when the
// command line changes directory, so relative write targets are unknown.
type rater struct{ cd bool }

func (r rater) classify(cmd string, depth int) action.Risk {
	if depth > 4 {
		return action.Destructive
	}
	f, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return action.Destructive
	}
	r.cd = r.cd || changesDir(f)
	risk := action.Read
	raise := func(x action.Risk) {
		if x > risk {
			risk = x
		}
	}
	syntax.Walk(f, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.FuncDecl: // e.g. fork bombs
			raise(action.Destructive)
		case *syntax.Redirect:
			raise(r.classifyRedirect(n))
		case *syntax.CallExpr:
			raise(r.classifyCall(n.Args, depth))
			if riskyAssigns(n.Assigns) {
				raise(action.Write)
			}
		case *syntax.DeclClause: // export PATH=…
			if riskyAssigns(n.Args) {
				raise(action.Write)
			}
		case *syntax.TestClause, *syntax.ArithmCmd, *syntax.LetClause:
			// harmless on their own; nested calls are walked separately
		}
		return true
	})
	return risk
}

// riskyAssigns reports whether any assignment sets a variable that
// changes what a command loads or runs (LD_PRELOAD, PATH, GIT_*, PAGER…).
func riskyAssigns(as []*syntax.Assign) bool {
	for _, a := range as {
		if a.Name != nil && riskyEnv(a.Name.Value) {
			return true
		}
	}
	return false
}

func riskyEnv(name string) bool {
	switch name {
	case "PATH", "BASH_ENV", "ENV", "IFS", "PAGER", "EDITOR", "VISUAL":
		return true
	}
	return strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") || strings.HasPrefix(name, "GIT_") ||
		strings.HasSuffix(name, "_PAGER")
}

// changesDir reports whether the line runs cd or pushd anywhere.
func changesDir(f *syntax.File) bool {
	found := false
	syntax.Walk(f, func(n syntax.Node) bool {
		if c, ok := n.(*syntax.CallExpr); ok {
			for _, w := range c.Args {
				s, ok := literal(w)
				if !ok || (s != "builtin" && s != "command") {
					found = found || (ok && (s == "cd" || s == "pushd"))
					break
				}
			}
		}
		return !found
	})
	return found
}

// classifyString rates a command line found inside another command's
// arguments (bash -c, su -c, ssh host cmd…).
func (r rater) classifyString(cmd string, depth int) action.Risk {
	if strings.Contains(cmd, dynamic) {
		return action.Destructive
	}
	return r.classify(cmd, depth+1)
}

func (r rater) classifyRedirect(rd *syntax.Redirect) action.Risk {
	switch rd.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrAll, syntax.AppAll, syntax.RdrClob, syntax.RdrInOut, syntax.DplOut:
		target, ok := literal(rd.Word)
		if !ok {
			return action.Destructive
		}
		if rd.Op == syntax.DplOut && (target == "-" || isDigits(target)) {
			return action.Read // >&2, >&- duplicate or close a descriptor
		}
		if target == "/dev/null" || target == "/dev/stdout" || target == "/dev/stderr" {
			return action.Read
		}
		if r.protected(target) {
			return action.Destructive
		}
		return action.Write
	}
	return action.Read
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

func (r rater) classifyCall(words []*syntax.Word, depth int) action.Risk {
	if len(words) == 0 { // pure assignment like FOO=bar
		return action.Read
	}
	args := make([]string, 0, len(words))
	for i, w := range words {
		s, ok := literal(w)
		if !ok {
			if i == 0 {
				return action.Destructive // command name computed at runtime
			}
			s = dynamic // dynamic argument: keep position, cannot be inspected
		}
		args = append(args, s)
	}
	return r.classifyArgs(args, depth)
}

// dynamic stands for an argument whose value is only known at run time.
const dynamic = "\x00dynamic"

func (r rater) classifyArgs(args []string, depth int) action.Risk {
	w := stripWrapper(args)
	risk := r.classifyWrapped(w, depth)
	if w.riskyEnv {
		risk = max(risk, action.Write) // env LD_PRELOAD=… cmd
	}
	return risk
}

func (r rater) classifyWrapped(w wrapped, depth int) action.Risk {
	if depth > 4 {
		return action.Destructive
	}
	r.cd = r.cd || w.chdir
	args := w.args
	switch {
	case w.split != "": // env -S 'cmd args'
		return atLeastWrite(r.classifyString(strings.Join(append([]string{w.split}, args...), " "), depth))
	case len(args) == 0 && w.rootShell: // sudo -s, sudo -i
		return action.Destructive
	case len(args) == 0:
		return action.Read
	}
	name := cmdName(args[0])
	rest := args[1:]
	if strings.Contains(name, dynamic) {
		return action.Destructive // wrapped command name computed at run time
	}
	if readOnly[name] {
		return action.Read // pure readers stay Read even with dynamic args
	}
	risk := r.classifyName(name, rest, depth)
	if risk < action.Write && hasDynamic(rest) {
		if name == "dd" {
			return action.Destructive
		}
		return action.Write
	}
	return risk
}

// cmdName is the command's base name without a leading backslash (\rm).
func cmdName(s string) string { return path.Base(strings.TrimPrefix(s, `\`)) }

func atLeastWrite(r action.Risk) action.Risk { return max(r, action.Write) }

func hasDynamic(args []string) bool {
	for _, a := range args {
		if strings.Contains(a, dynamic) {
			return true
		}
	}
	return false
}

// classifyName rates a command that is not a pure reader.
func (r rater) classifyName(name string, rest []string, depth int) action.Risk {
	if check, ok := argChecks[name]; ok {
		return max(check(rest), r.outputRisk(name, rest))
	}
	switch {
	case destructiveCmd[name], strings.HasPrefix(name, "mkfs."):
		return action.Destructive
	case name == "sh" || name == "bash" || name == "zsh" || name == "dash":
		return r.classifyShell(rest, depth)
	case name == "su" || name == "runuser":
		if cmd, ok := flagValue(rest, "-c", "--command"); ok {
			return atLeastWrite(r.classifyString(cmd, depth))
		}
		return action.Destructive // a root shell reading stdin
	case name == "ssh":
		words := afterFlags(rest, "bcDEeFIiJLlmOopQRSWwB", nil)
		if len(words) < 2 {
			return action.Write
		}
		return atLeastWrite(r.classifyString(strings.Join(words[1:], " "), depth))
	case name == "docker" || name == "podman":
		if words := afterFlags(rest, "", nil); len(words) > 0 && words[0] == "exec" {
			inner := afterFlags(words[1:], "uwe", set("--user", "--workdir", "--env", "--env-file", "--detach-keys"))
			if len(inner) < 2 {
				return action.Write
			}
			return atLeastWrite(r.classifyArgs(inner[1:], depth+1))
		}
	case name == "kubectl" && hasAny(rest, "exec"):
		for i, a := range rest {
			if a == "--" {
				return atLeastWrite(r.classifyArgs(rest[i+1:], depth+1))
			}
		}
		return action.Destructive // old syntax: the command cannot be told from the pod
	case name == "find":
		return r.classifyFind(rest, depth)
	case name == "watch":
		words := afterFlags(rest, "nq", set("--interval"))
		if len(words) == 0 {
			return action.Write
		}
		return atLeastWrite(r.classifyString(strings.Join(words, " "), depth))
	case name == "systemd-run":
		words := afterFlags(rest, "upMHE", set("--unit", "--property", "--machine", "--host", "--description",
			"--slice", "--uid", "--gid", "--nice", "--setenv", "--working-directory", "--service-type",
			"--on-active", "--on-boot", "--on-startup", "--on-unit-active", "--on-unit-inactive", "--on-calendar",
			"--timer-property", "--path-property", "--socket-property"))
		return atLeastWrite(r.classifyArgs(words, depth+1))
	case name == "nohup":
		return atLeastWrite(r.classifyArgs(rest, depth+1))
	case name == "flock":
		if cmd, ok := flagValue(rest, "-c", "--command"); ok {
			return atLeastWrite(r.classifyString(cmd, depth))
		}
		words := afterFlags(rest, "wE", set("--timeout", "--conflict-exit-code"))
		if len(words) < 2 {
			return action.Write
		}
		return atLeastWrite(r.classifyArgs(words[1:], depth+1))
	case name == "systemctl" && hasAny(rest, keys(systemctlDestructive)...):
		return action.Destructive
	case name == "rm":
		for _, a := range rest {
			if a == "--recursive" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a, "rR")) {
				return action.Destructive
			}
		}
		return r.writeTargets(rest)
	case name == "dd":
		for _, a := range rest {
			if strings.HasPrefix(a, "of=") && a != "of=/dev/null" {
				return action.Destructive
			}
		}
		return action.Read
	case name == "sed":
		return r.classifySed(rest)
	case name == "tee":
		return r.writeTargets(rest)
	case name == "awk" || name == "gawk" || name == "mawk" || name == "nawk":
		return classifyAwk(rest)
	case name == "journalctl":
		for _, a := range rest {
			if strings.HasPrefix(a, "--vacuum") || a == "--rotate" || a == "--flush" {
				return action.Write
			}
		}
		return action.Read
	case name == "cp" || name == "mv" || name == "install" || name == "rsync" || name == "ln" || name == "chmod" || name == "chown":
		return r.writeTargets(rest)
	case name == "git":
		return r.classifyGit(rest)
	case name == "systemctl" && len(nonFlags(rest, nil)) == 0:
		return action.Read // systemctl --failed lists units
	}
	if subs, ok := readOnlySub[name]; ok {
		if sub := subcommand(rest, subs); sub != "" {
			return action.Read
		}
		return action.Write
	}
	return action.Write
}

// classifyShell rates sh/bash/zsh/dash. With -c, every operand may be
// the command string, so all are rated and the maximum taken; -c given
// more than once is Destructive. Without -c the shell reads a script or
// stdin, which cannot be seen: Destructive.
func (r rater) classifyShell(args []string, depth int) action.Risk {
	cs := 0
	var operands []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-o" || a == "+o" || a == "-O" || a == "+O":
			i++ // option name
		case len(a) > 1 && (a[0] == '-' || a[0] == '+') && a != "--":
			if a[0] == '-' && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") {
				cs++
			}
		default:
			operands = append(operands, a)
		}
	}
	if cs == 0 || len(operands) == 0 {
		return action.Destructive
	}
	if cs > 1 {
		return action.Destructive
	}
	risk := action.Read
	for _, op := range operands {
		risk = max(risk, r.classifyString(op, depth))
	}
	return risk
}

// classifyFind rates find: -delete is Destructive, commands run by
// -exec and friends are rated themselves (at least Write).
func (r rater) classifyFind(args []string, depth int) action.Risk {
	risk := action.Read
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-delete":
			return action.Destructive
		case "-fprint", "-fprintf", "-fls":
			risk = max(risk, action.Write)
		case "-exec", "-execdir", "-ok", "-okdir":
			j := i + 1
			for j < len(args) && args[j] != ";" && args[j] != "+" {
				j++
			}
			risk = max(risk, atLeastWrite(r.classifyArgs(args[i+1:j], depth+1)))
			i = j
		}
	}
	return risk
}

// flagValue returns the value of the first of the given flags (-c x,
// --command x or --command=x).
func flagValue(args []string, short, long string) (string, bool) {
	for i, a := range args {
		switch {
		case (a == short || a == long) && i+1 < len(args):
			return args[i+1], true
		case strings.HasPrefix(a, long+"="):
			return strings.TrimPrefix(a, long+"="), true
		}
	}
	return "", false
}

// afterFlags drops leading flags and returns the args from the first
// operand on. Short flags in valued (a cluster ending in one of them, as
// in -p 22) and long flags in long take the next arg as value.
func afterFlags(args []string, valued string, long map[string]bool) []string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return args[i+1:]
		case long[a]:
			i++
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for j := 1; j < len(a); j++ {
				if strings.IndexByte(valued, a[j]) >= 0 {
					if j == len(a)-1 {
						i++ // value is the next arg
					}
					break // otherwise the value is attached
				}
			}
		default:
			return args[i:]
		}
	}
	return nil
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// subcommand returns the first non-flag word if it is in subs (flags in
// subs such as rpm -q also count), else "".
func subcommand(args []string, subs map[string]bool) string {
	for _, a := range args {
		if subs[a] {
			return a
		}
		if !strings.HasPrefix(a, "-") {
			break // first non-flag word is the subcommand
		}
	}
	return ""
}

// writeTargets rates a command that writes to the paths among its args.
func (r rater) writeTargets(args []string) action.Risk {
	for _, a := range args {
		if r.protected(a) {
			return action.Destructive
		}
	}
	return action.Write
}

// protected reports whether writing p needs a destructive rating. After
// a cd, any relative path may point anywhere.
func (r rater) protected(p string) bool {
	if strings.HasPrefix(p, "-") {
		i := strings.IndexByte(p, '=')
		if i < 0 {
			return false // a flag, not a path
		}
		p = p[i+1:] // --target-directory=/etc
	}
	if r.cd && p != "" && !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "~") && !strings.HasPrefix(p, "$HOME") {
		return true
	}
	return IsProtected(p)
}

// IsProtected reports whether p is a system path, a credential store or a
// block device. Relative paths that climb out (../..) are protected too.
func IsProtected(p string) bool {
	if strings.HasPrefix(p, "$HOME/") {
		p = "~/" + p[len("$HOME/"):]
	}
	if p == "" {
		return false
	}
	c := path.Clean(p)
	if strings.HasPrefix(c, "/") {
		if c == "/" {
			return true
		}
		for _, root := range protectedRoots {
			if c == root || strings.HasPrefix(c, root+"/") {
				return true
			}
		}
		for _, dev := range protectedDevices {
			if strings.HasPrefix(c, dev) {
				return true
			}
		}
	}
	if c == ".." || strings.HasPrefix(c, "../") {
		return true
	}
	parts := strings.Split(c, "/")
	for i, part := range parts {
		if protectedDirs[part] {
			return true
		}
		if part == ".config" && i+1 < len(parts) && parts[i+1] == "systemd" {
			return true // user units run at login
		}
	}
	return protectedFiles[path.Base(c)]
}

// wrapped is a command with its wrappers removed.
type wrapped struct {
	args      []string
	split     string // env -S string, to be parsed as a command line
	rootShell bool   // sudo -s / -i
	chdir     bool   // sudo -D, env -C
	riskyEnv  bool   // env LD_PRELOAD=… and the like
}

// stripWrapper removes leading wrapper commands (sudo -u x, env A=b,
// timeout 5, xargs -0 …) and returns the wrapped command.
func stripWrapper(args []string) wrapped {
	var w wrapped
	for len(args) > 0 && wrappers[cmdName(args[0])] {
		name := cmdName(args[0])
		args = args[1:]
		for len(args) > 0 {
			a := args[0]
			switch {
			case name == "env" && strings.Contains(a, "=") && !strings.HasPrefix(a, "-"):
				if k, _, _ := strings.Cut(a, "="); riskyEnv(k) {
					w.riskyEnv = true
				}
				args = args[1:]
			case name == "env" && (a == "-S" || a == "--split-string"):
				if len(args) > 1 {
					w.split, args = args[1], args[2:]
				} else {
					args = args[1:]
				}
				w.args = args
				return w
			case name == "env" && (strings.HasPrefix(a, "-S") || strings.HasPrefix(a, "--split-string=")):
				w.split = strings.TrimPrefix(strings.TrimPrefix(a, "--split-string="), "-S")
				w.args = args[1:]
				return w
			case strings.HasPrefix(a, "-"):
				args = args[1:]
				if name == "sudo" && (a == "--shell" || a == "--login" ||
					(!strings.HasPrefix(a, "--") && strings.ContainsAny(a, "si"))) {
					w.rootShell = true
				}
				if (name == "sudo" && (a == "-D" || strings.HasPrefix(a, "--chdir"))) ||
					(name == "env" && (a == "-C" || strings.HasPrefix(a, "--chdir"))) {
					w.chdir = true
				}
				if takesValue(name, a) && len(args) > 0 {
					args = args[1:]
				}
			case name == "timeout" && len(a) > 0 && a[0] >= '0' && a[0] <= '9':
				args = args[1:]
			default:
				goto next
			}
		}
	next:
	}
	w.args = args
	return w
}

func takesValue(wrapper, flag string) bool {
	switch wrapper {
	case "sudo":
		return flag == "-u" || flag == "-g" || flag == "-C" || flag == "-h" || flag == "-p" || flag == "-U" ||
			flag == "-D" || flag == "-R" || flag == "-T" || flag == "-r" || flag == "-t"
	case "doas":
		return flag == "-u" || flag == "-C"
	case "env":
		return flag == "-u" || flag == "-C"
	case "nice", "ionice":
		return flag == "-n" || flag == "-c"
	case "timeout":
		return flag == "-s" || flag == "-k" || flag == "--signal" || flag == "--kill-after"
	case "xargs":
		return flag == "-n" || flag == "-I" || flag == "-P" || flag == "-L" || flag == "-d" || flag == "-s" ||
			flag == "-E" || flag == "-a" || flag == "--arg-file"
	case "stdbuf":
		return flag == "-i" || flag == "-o" || flag == "-e"
	}
	return false
}

// literal returns the word's value if it contains no expansions, with
// shell backslash escapes removed (\rm is rm, \; is ;).
func literal(w *syntax.Word) (string, bool) {
	var sb strings.Builder
	for _, p := range w.Parts {
		switch p := p.(type) {
		case *syntax.Lit:
			sb.WriteString(unescape(p.Value, ""))
		case *syntax.SglQuoted:
			sb.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				lit, ok := q.(*syntax.Lit)
				if !ok {
					return "", false
				}
				sb.WriteString(unescape(lit.Value, "$`\"\\\n"))
			}
		case *syntax.ParamExp:
			// allow $HOME only, so ~/.ssh-style protection still applies
			if p.Param != nil && p.Param.Value == "HOME" && p.Exp == nil && p.Repl == nil && p.Index == nil && p.Slice == nil {
				sb.WriteString("$HOME")
				continue
			}
			return "", false
		default:
			return "", false
		}
	}
	return sb.String(), true
}

// unescape removes backslashes before the characters in only (any
// character if only is empty, as outside quotes).
func unescape(s, only string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (only == "" || strings.IndexByte(only, s[i+1]) >= 0) {
			i++
			if s[i] == '\n' {
				continue // line continuation
			}
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}
