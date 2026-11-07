package policy

import (
	"regexp"
	"strings"

	"github.com/hua-ri/coxswain/internal/action"
)

// argChecks rate commands that read with some arguments and change state
// with others. Anything that does not match the read form is Write.
var argChecks = map[string]func(args []string) action.Risk{
	"ip":          checkIP,
	"ifconfig":    func(a []string) action.Risk { return readIf(len(nonFlags(a, nil)) <= 1) },
	"route":       func(a []string) action.Risk { return writeIf(hasAny(a, "add", "del", "delete", "flush", "change")) },
	"hostname":    checkHostname,
	"date":        checkDate,
	"sysctl":      checkSysctl,
	"mount":       func(a []string) action.Risk { return readIf(len(a) == 0) },
	"timedatectl": checkCtl,
	"hostnamectl": checkCtl,
	"ss":          func(a []string) action.Risk { return writeIf(hasAny(a, "--kill", "--diag") || hasShort(a, "KD")) },
	"dmesg":       checkDmesg,
	"nvidia-smi":  checkNvidiaSmi,
	"sort": func(a []string) action.Risk {
		return writeIf(hasShort(a, "o") || hasPrefix(a, "--output", "--compress-program"))
	},
	"uniq": func(a []string) action.Risk { return readIf(len(nonFlags(a, set("-f", "-s", "-w"))) <= 1) },
	"xxd": func(a []string) action.Risk {
		return writeIf(hasAny(a, "-revert") || hasShort(a, "r") ||
			len(nonFlags(a, set("-c", "-g", "-l", "-s", "-o", "-n", "-cols", "-len", "-seek"))) > 1)
	},
	"tree":    func(a []string) action.Risk { return writeIf(hasPrefix(a, "-o")) },
	"base64":  func(a []string) action.Risk { return writeIf(hasPrefix(a, "-o", "--output")) },
	"ulimit":  func(a []string) action.Risk { return readIf(len(nonFlags(a, nil)) == 0) },
	"crontab": checkCrontab,
}

func readIf(ok bool) action.Risk {
	if ok {
		return action.Read
	}
	return action.Write
}

func writeIf(ok bool) action.Risk { return readIf(!ok) }

// nonFlags returns the args that are not flags, skipping the value after
// any flag in valued.
func nonFlags(args []string, valued map[string]bool) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case valued[a]:
			i++
		case strings.HasPrefix(a, "-") && a != "-":
		default:
			out = append(out, a)
		}
	}
	return out
}

func hasAny(args []string, words ...string) bool {
	for _, a := range args {
		for _, w := range words {
			if a == w {
				return true
			}
		}
	}
	return false
}

func hasPrefix(args []string, prefixes ...string) bool {
	for _, a := range args {
		for _, p := range prefixes {
			if strings.HasPrefix(a, p) {
				return true
			}
		}
	}
	return false
}

// hasShort reports whether a short flag cluster (-abc) contains any of chars.
func hasShort(args []string, chars string) bool {
	for _, a := range args {
		if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsAny(a[1:], chars) {
			return true
		}
	}
	return false
}

// ipWrites are ip(8) commands that change state. ip accepts any prefix of
// a command (ip a a = ip addr add), so abbreviations count too.
var ipWrites = []string{"add", "del", "delete", "set", "change", "replace", "flush", "append", "up", "down",
	"prepend", "restore", "exec"}

func checkIP(args []string) action.Risk {
	if hasAny(args, "-b", "-batch", "-force") {
		return action.Write
	}
	words := nonFlags(args, set("-f", "-family", "-n", "-netns", "-rc", "-rcvbuf"))
	for i, w := range words {
		for _, cmd := range ipWrites {
			if w == cmd || (i > 0 && strings.HasPrefix(cmd, w)) { // words[0] is the object
				return action.Write
			}
		}
	}
	return action.Read
}

func checkHostname(args []string) action.Risk {
	return readIf(len(nonFlags(args, nil)) == 0 && !hasPrefix(args, "-F", "--file"))
}

func checkDate(args []string) action.Risk {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "--set") || (strings.HasPrefix(a, "-s") && !strings.HasPrefix(a, "--")):
			return action.Write
		case a == "-d" || a == "--date" || a == "-r" || a == "--reference" || a == "-f" || a == "--file":
			i++ // value is a date or a file to read
		case strings.HasPrefix(a, "-"):
		case !strings.HasPrefix(a, "+"):
			return action.Write
		}
	}
	return action.Read
}

func checkSysctl(args []string) action.Risk {
	for _, a := range args {
		if strings.Contains(a, "=") && !strings.HasPrefix(a, "--pattern") || a == "--system" ||
			strings.HasPrefix(a, "--load") || strings.HasPrefix(a, "--write") {
			return action.Write
		}
	}
	return writeIf(hasShort(args, "wp"))
}

// checkCtl rates timedatectl and hostnamectl.
func checkCtl(args []string) action.Risk {
	words := nonFlags(args, nil)
	if len(words) == 0 {
		return action.Read
	}
	return readIf(hasAny(words[:1], "status", "show", "list-timezones", "timesync-status", "show-timesync"))
}

func checkDmesg(args []string) action.Risk {
	return writeIf(hasShort(args, "cCDEn") ||
		hasAny(args, "--clear", "--read-clear", "--console-off", "--console-on") ||
		hasPrefix(args, "--console-level"))
}

func checkNvidiaSmi(args []string) action.Risk {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-L" || a == "-q" || a == "--list-gpus" || a == "--query":
		case a == "-i" || a == "-d" || a == "-l":
			i++
		case strings.HasPrefix(a, "--id=") || strings.HasPrefix(a, "--query-") || strings.HasPrefix(a, "--format="):
		default:
			return action.Write
		}
	}
	return action.Read
}

// checkCrontab: only listing is Read; installing or removing a crontab is
// persistence and therefore Destructive.
func checkCrontab(args []string) action.Risk {
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-u" && i+1 < len(args) {
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	if len(rest) == 1 && rest[0] == "-l" {
		return action.Read
	}
	return action.Destructive
}

// classifyGit: read-only subcommands stay Read unless they write a file
// (--output) or set config (-c), and branch/remote only list.
func classifyGit(args []string) action.Risk {
	sub := subcommand(args, readOnlySub["git"])
	if sub == "" || hasPrefix(args, "--output") {
		return action.Write
	}
	i := 0
	for args[i] != sub {
		i++
	}
	if hasPrefix(args[:i], "-c", "--config-env", "--exec-path") {
		return action.Write
	}
	after := args[i+1:]
	switch sub {
	case "branch":
		return readIf(len(nonFlags(after, nil)) == 0)
	case "remote":
		for _, a := range after {
			if a != "-v" && a != "--verbose" {
				return action.Write
			}
		}
	}
	return action.Read
}

// sedPrint matches print/delete/quit commands with an optional address,
// several joined by ";".
var sedPrint = func() *regexp.Regexp {
	addr := `(\d+|\$|/[^/]*/)`
	cmd := `(` + addr + `(\s*,\s*` + addr + `)?)?\s*!?\s*[pdq=]`
	return regexp.MustCompile(`^\s*` + cmd + `(\s*;\s*` + cmd + `)*\s*;?\s*$`)
}()

// classifySed: Read only for scripts that print, delete or substitute
// without the w/e flags; -i edits files in place.
func (r rater) classifySed(args []string) action.Risk {
	for _, a := range args {
		if strings.HasPrefix(a, "--in-place") || (len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.Contains(a, "i")) {
			return r.writeTargets(args)
		}
	}
	var scripts, operands []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-n" || a == "-E" || a == "-r" || a == "--quiet" || a == "--silent" || a == "--regexp-extended":
		case a == "-e" || a == "--expression":
			if i+1 < len(args) {
				i++
				scripts = append(scripts, args[i])
			}
		case strings.HasPrefix(a, "--expression="):
			scripts = append(scripts, strings.TrimPrefix(a, "--expression="))
		case strings.HasPrefix(a, "-") && a != "-":
			return action.Write // -f, -s, -z, …
		default:
			operands = append(operands, a)
		}
	}
	if len(scripts) == 0 && len(operands) > 0 {
		scripts = operands[:1]
	}
	for _, s := range scripts {
		if !sedPrint.MatchString(s) && !sedSafeSubst(s) {
			return r.sedUnsafe(scripts)
		}
	}
	return action.Read
}

// sedUnsafe rates sed scripts that may write or execute: Write, or
// Destructive if they name a protected path (e.g. w /etc/cron.d/x).
func (r rater) sedUnsafe(scripts []string) action.Risk {
	for _, a := range scripts {
		if r.writeTargets(strings.Fields(a)) == action.Destructive {
			return action.Destructive
		}
	}
	return action.Write
}

// sedSafeSubst reports whether s is a single s/re/repl/flags command
// whose flags only print, ignore case or pick a match (no w or e).
func sedSafeSubst(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 4 || s[0] != 's' || strings.ContainsAny(s, "\n") {
		return false
	}
	delim := s[1]
	if delim == '\\' {
		return false
	}
	i := 2
	for part := 0; part < 2; part++ {
		for ; i < len(s) && s[i] != delim; i++ {
			if s[i] == '\\' {
				i++
			}
		}
		if i >= len(s) {
			return false
		}
		i++ // past the delimiter
	}
	return strings.Trim(s[i:], "gpiI0123456789") == ""
}

// classifyAwk: awk programs can run commands, write files and read other
// files; any of that is Write.
func classifyAwk(args []string) action.Risk {
	if hasPrefix(args, "-f", "--file", "-E", "--exec", "-i", "--include", "-l", "--load") {
		return action.Write
	}
	for _, a := range args {
		for _, bad := range []string{"|", "system", ">", "getline", "close(", "fflush"} {
			if strings.Contains(a, bad) {
				return action.Write
			}
		}
	}
	return action.Read
}
