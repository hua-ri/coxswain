// Package policy decides whether an invocation may run, must be
// confirmed by a human, or is refused.
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
var readOnly = set("cat", "head", "tail", "less", "more", "grep", "egrep", "fgrep", "zgrep", "zcat",
	"ls", "ll", "df", "du", "free", "uptime", "uname", "whoami", "id", "groups", "w", "who", "last",
	"ps", "pgrep", "pidof", "top", "htop", "netstat", "lsof", "vmstat", "iostat",
	"mpstat", "sar", "stat", "file", "wc", "cut", "tr", "echo",
	"printf", "env", "printenv", "which", "type", "nproc", "lscpu", "lsblk", "blkid", "lsmod", "lspci", "lsusb",
	"getent", "ping", "dig", "nslookup", "host", "traceroute", "tracepath", "md5sum", "sha1sum", "sha256sum",
	"readlink", "realpath", "basename", "dirname", "pwd", "test", "true", "false", "seq", "column", "jq",
	"diff", "cmp", "findmnt", "getenforce", "sestatus", "locale", "cal", "arch", "nl", "rev", "tac",
	"strings", "od", "hexdump")

// readOnlySub: commands that are read-only only with these subcommands.
var readOnlySub = map[string]map[string]bool{
	"systemctl": set("status", "is-active", "is-enabled", "is-failed", "list-units", "list-unit-files",
		"list-timers", "show", "cat", "list-dependencies"),
	"docker": set("ps", "logs", "inspect", "images", "version", "info", "top", "port", "diff", "history"),
	"git":    set("status", "log", "diff", "show", "branch", "remote", "rev-parse", "ls-files"),
	"apt":    set("list", "show", "search", "policy"),
	"yum":    set("list", "info", "search"),
	"rpm":    set("-q", "-qa", "-qi", "-ql"),
	"dpkg":   set("-l", "-L", "-s"),
}

// destructiveCmd are always destructive regardless of arguments.
var destructiveCmd = set("mkfs", "wipefs", "shutdown", "reboot", "halt", "poweroff", "init", "fdisk",
	"parted", "sfdisk", "mkswap", "eval", "exec", "source", ".")

// wrappers run another command; we classify the wrapped command.
var wrappers = set("sudo", "env", "timeout", "nice", "nohup", "time", "command", "stdbuf", "ionice", "xargs")

// protectedPrefixes may not be written without a destructive rating.
var protectedPrefixes = []string{"/etc/", "/boot", "/usr/", "/bin/", "/sbin/", "/lib", "/proc/", "/sys/",
	"/dev/sd", "/dev/nvme", "/dev/vd", "~/.ssh", "~/.kube", "~/.aws", "~/.gnupg"}

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// ClassifyCommand rates a shell command line. Anything it cannot fully
// understand is rated Destructive so that it is always confirmed.
func ClassifyCommand(cmd string) action.Risk {
	f, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return action.Destructive
	}
	risk := action.Read
	raise := func(r action.Risk) {
		if r > risk {
			risk = r
		}
	}
	syntax.Walk(f, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.FuncDecl: // e.g. fork bombs
			raise(action.Destructive)
		case *syntax.Redirect:
			raise(classifyRedirect(n))
		case *syntax.CallExpr:
			raise(classifyCall(n.Args))
		case *syntax.DeclClause, *syntax.TestClause, *syntax.ArithmCmd, *syntax.LetClause:
			// harmless on their own; nested calls are walked separately
		}
		return true
	})
	return risk
}

func classifyRedirect(r *syntax.Redirect) action.Risk {
	switch r.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrAll, syntax.AppAll, syntax.RdrClob, syntax.RdrInOut, syntax.DplOut:
		target, ok := literal(r.Word)
		if !ok {
			return action.Destructive
		}
		if r.Op == syntax.DplOut && (target == "-" || isDigits(target)) {
			return action.Read // >&2, >&- duplicate or close a descriptor
		}
		if target == "/dev/null" || target == "/dev/stdout" || target == "/dev/stderr" {
			return action.Read
		}
		if isProtected(target) {
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

func classifyCall(words []*syntax.Word) action.Risk {
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
	return classifyArgs(args, 0)
}

// dynamic stands for an argument whose value is only known at run time.
const dynamic = "\x00dynamic"

func classifyArgs(args []string, depth int) action.Risk {
	if depth > 4 {
		return action.Destructive
	}
	args = stripWrapper(args)
	if len(args) == 0 {
		return action.Read
	}
	name := path.Base(args[0])
	rest := args[1:]
	if readOnly[name] {
		return action.Read // pure readers stay Read even with dynamic args
	}
	r := classifyName(name, rest, depth)
	if r < action.Write && hasDynamic(rest) {
		if name == "dd" {
			return action.Destructive
		}
		return action.Write
	}
	return r
}

func hasDynamic(args []string) bool {
	for _, a := range args {
		if strings.Contains(a, dynamic) {
			return true
		}
	}
	return false
}

// classifyName rates a command that is not a pure reader.
func classifyName(name string, rest []string, depth int) action.Risk {
	if check, ok := argChecks[name]; ok {
		return check(rest)
	}
	switch {
	case destructiveCmd[name], strings.HasPrefix(name, "mkfs."):
		return action.Destructive
	case name == "sh" || name == "bash" || name == "zsh" || name == "dash":
		for i, a := range rest {
			if a == "-c" && i+1 < len(rest) {
				if strings.Contains(rest[i+1], dynamic) {
					return action.Destructive
				}
				return ClassifyCommand(rest[i+1])
			}
		}
		return action.Destructive // a shell reading a script or stdin
	case name == "rm":
		for _, a := range rest {
			if a == "--recursive" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a, "rR")) {
				return action.Destructive
			}
		}
		return action.Write
	case name == "dd":
		for _, a := range rest {
			if strings.HasPrefix(a, "of=") && a != "of=/dev/null" {
				return action.Destructive
			}
		}
		return action.Read
	case name == "find":
		for _, a := range rest {
			switch a {
			case "-delete":
				return action.Destructive
			case "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprintf", "-fls":
				return action.Write
			}
		}
		return action.Read
	case name == "sed":
		return classifySed(rest)
	case name == "tee":
		return writeTargets(rest)
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
		return writeTargets(rest)
	case name == "git":
		return classifyGit(rest)
	}
	if subs, ok := readOnlySub[name]; ok {
		if sub := subcommand(rest, subs); sub != "" {
			return action.Read
		}
		return action.Write
	}
	return action.Write
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
func writeTargets(args []string) action.Risk {
	for _, a := range args {
		if isProtected(a) {
			return action.Destructive
		}
	}
	return action.Write
}

func isProtected(p string) bool {
	if strings.HasPrefix(p, "$HOME/") {
		p = "~/" + p[len("$HOME/"):]
	}
	for _, pre := range protectedPrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return p == "/"
}

// stripWrapper removes leading wrapper commands (sudo -u x, env A=b,
// timeout 5, xargs -0 …) and returns the wrapped command.
func stripWrapper(args []string) []string {
	for len(args) > 0 && wrappers[path.Base(args[0])] {
		w := path.Base(args[0])
		args = args[1:]
		for len(args) > 0 {
			a := args[0]
			switch {
			case w == "env" && strings.Contains(a, "=") && !strings.HasPrefix(a, "-"):
				args = args[1:]
			case strings.HasPrefix(a, "-"):
				args = args[1:]
				if takesValue(w, a) && len(args) > 0 {
					args = args[1:]
				}
			case w == "timeout" && len(a) > 0 && a[0] >= '0' && a[0] <= '9':
				args = args[1:]
			default:
				goto next
			}
		}
	next:
	}
	return args
}

func takesValue(wrapper, flag string) bool {
	switch wrapper {
	case "sudo":
		return flag == "-u" || flag == "-g" || flag == "-C" || flag == "-h" || flag == "-p" || flag == "-U"
	case "env":
		return flag == "-u" || flag == "-C"
	case "nice", "ionice":
		return flag == "-n" || flag == "-c"
	case "timeout":
		return flag == "-s" || flag == "-k" || flag == "--signal" || flag == "--kill-after"
	case "xargs":
		return flag == "-n" || flag == "-I" || flag == "-P" || flag == "-L" || flag == "-d" || flag == "-s" || flag == "-E"
	case "stdbuf":
		return flag == "-i" || flag == "-o" || flag == "-e"
	}
	return false
}

// literal returns the word's value if it contains no expansions.
func literal(w *syntax.Word) (string, bool) {
	var sb strings.Builder
	for _, p := range w.Parts {
		switch p := p.(type) {
		case *syntax.Lit:
			sb.WriteString(p.Value)
		case *syntax.SglQuoted:
			sb.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				lit, ok := q.(*syntax.Lit)
				if !ok {
					return "", false
				}
				sb.WriteString(lit.Value)
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
