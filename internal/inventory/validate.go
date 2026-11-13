package inventory

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

var (
	namePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)
	addrPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._:%-]*$`)
	userPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
)

// ValidName reports whether name can name a host: letters, digits, '.',
// '_' and '-', up to 64, not starting with '-' or '.'.
func ValidName(name string) bool { return namePattern.MatchString(name) }

// Validate checks a host the user adds or edits. Its ProxyJump entries
// must be saved hosts (in saved) or [user@]host[:port].
func Validate(h Host, saved map[string]Host) error {
	switch {
	case h.Name == "":
		return fmt.Errorf("a name is required")
	case strings.HasPrefix(h.Name, "-"), strings.HasPrefix(h.Name, "."):
		return fmt.Errorf("host name %q starts with a dash or dot", h.Name)
	case strings.ContainsAny(h.Name, " \t/\\"):
		return fmt.Errorf("host name %q has spaces or slashes", h.Name)
	case !ValidName(h.Name):
		return fmt.Errorf("host name %q: use letters, digits, '.', '_' and '-' (at most 64)", h.Name)
	}
	if err := validAddress(h.HostName); err != nil {
		return err
	}
	if h.Port < 0 || h.Port > 65535 {
		return fmt.Errorf("port %d is out of range (1-65535)", h.Port)
	}
	if h.User != "" && !userPattern.MatchString(h.User) {
		return fmt.Errorf("user %q: use letters, digits, '.', '_' and '-'", h.User)
	}
	if h.ProxyJump == "" {
		return nil
	}
	for _, spec := range strings.Split(h.ProxyJump, ",") {
		spec = strings.TrimSpace(spec)
		if spec == h.Name {
			return fmt.Errorf("%s cannot be its own jump host", h.Name)
		}
		if _, ok := saved[spec]; ok {
			continue
		}
		if err := validJumpSpec(spec); err != nil {
			return err
		}
	}
	return nil
}

func validAddress(a string) error {
	switch {
	case a == "":
		return fmt.Errorf("an address (host name or IP) is required")
	case strings.HasPrefix(a, "-"):
		return fmt.Errorf("address must not start with '-': %q", a)
	case !addrPattern.MatchString(a):
		return fmt.Errorf("address must not contain spaces or special characters: %q", a)
	}
	return nil
}

// validJumpSpec checks [user@]host[:port] for a jump host that is not
// saved. A bare word without '.', ':' or '@' is more likely a mistyped
// host name than an address.
func validJumpSpec(spec string) error {
	bad := fmt.Errorf("jump host %q is not a saved host; use a saved host's name or user@host:port", spec)
	rest := spec
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		if !userPattern.MatchString(rest[:at]) {
			return bad
		}
		rest = rest[at+1:]
	}
	host := rest
	if h, p, err := net.SplitHostPort(rest); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("bad port in jump host %q", spec)
		}
		host = h
	} else if strings.Count(rest, ":") == 1 {
		return fmt.Errorf("bad port in jump host %q", spec)
	}
	if validAddress(host) != nil {
		return bad
	}
	if !strings.ContainsAny(spec, ".:@") && host != "localhost" {
		return bad
	}
	return nil
}
