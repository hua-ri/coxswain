package inventory

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// An import from ~/.ssh/config adds the hosts it has and refreshes the
// connection fields of the ones Coxswain already has (tags and notes are
// Coxswain's and stay). A host whose connection fields were edited in
// Coxswain since its last import, or that was added by hand, is a
// conflict: it is left alone unless the user asks to overwrite it.

// NewHost is a host an import adds.
type NewHost struct {
	Name    string `json:"name"`
	Address string `json:"address"` // user@host:port
}

// FieldChange is one connection field an import changes.
type FieldChange struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// HostChange is a host whose connection fields an import refreshes.
type HostChange struct {
	Name    string        `json:"name"`
	Changes []FieldChange `json:"changes"`
}

// Conflict is a field of a host edited in Coxswain that the config would
// change: From is Coxswain's value, To the config's.
type Conflict struct {
	Name  string `json:"name"`
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// ImportPlan is what an import does (or did). After an import, Conflicts
// are the hosts that were left as they are.
type ImportPlan struct {
	Added     []NewHost    `json:"added"`
	Updated   []HostChange `json:"updated"`
	Unchanged []string     `json:"unchanged"`
	Conflicts []Conflict   `json:"conflicts"`
}

// connFields are a host's connection fields, as compared and shown.
func connFields(h Host) [][2]string {
	port := h.Port
	if port == 0 {
		port = 22
	}
	return [][2]string{
		{"hostname", h.HostName},
		{"port", strconv.Itoa(port)},
		{"user", h.User},
		{"proxy_jump", h.ProxyJump},
		{"identity_files", strings.Join(h.IdentityFiles, ", ")},
	}
}

// connFingerprint identifies a host's connection fields.
func connFingerprint(h Host) string {
	var b strings.Builder
	for _, f := range connFields(h) {
		b.WriteString(f[0] + "=" + f[1] + "\x00")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

func diff(old, h Host) []FieldChange {
	var out []FieldChange
	a, b := connFields(old), connFields(h)
	for i := range a {
		if a[i][1] != b[i][1] {
			out = append(out, FieldChange{Field: a[i][0], From: a[i][1], To: b[i][1]})
		}
	}
	return out
}

// edited reports whether the user changed h's connection fields in
// Coxswain since it was last imported, or added it by hand. Hosts
// imported before fingerprints were kept count as not edited.
func edited(h Host) bool {
	if h.Source == "manual" {
		return true
	}
	return h.ImportedFP != "" && connFingerprint(h) != h.ImportedFP
}

func addressOf(h Host) string {
	port := h.Port
	if port == 0 {
		port = 22
	}
	s := net.JoinHostPort(h.HostName, strconv.Itoa(port))
	if h.User != "" {
		s = h.User + "@" + s
	}
	return s
}

func plan(m map[string]Host, imported []Host) ImportPlan {
	p := ImportPlan{Added: []NewHost{}, Updated: []HostChange{}, Unchanged: []string{}, Conflicts: []Conflict{}}
	for _, h := range imported {
		old, ok := m[h.Name]
		switch {
		case !ok:
			p.Added = append(p.Added, NewHost{Name: h.Name, Address: addressOf(h)})
		case len(diff(old, h)) == 0:
			p.Unchanged = append(p.Unchanged, h.Name)
		case edited(old):
			for _, c := range diff(old, h) {
				p.Conflicts = append(p.Conflicts, Conflict{Name: h.Name, Field: c.Field, From: c.From, To: c.To})
			}
		default:
			p.Updated = append(p.Updated, HostChange{Name: h.Name, Changes: diff(old, h)})
		}
	}
	sort.Slice(p.Added, func(i, j int) bool { return p.Added[i].Name < p.Added[j].Name })
	sort.Slice(p.Updated, func(i, j int) bool { return p.Updated[i].Name < p.Updated[j].Name })
	sort.Strings(p.Unchanged)
	sort.SliceStable(p.Conflicts, func(i, j int) bool { return p.Conflicts[i].Name < p.Conflicts[j].Name })
	return p
}

// PlanImport says what Import(imported, nil) would do, without changing anything.
func (s *Store) PlanImport(imported []Host) (ImportPlan, error) {
	var p ImportPlan
	err := s.locked(func(m map[string]Host) error {
		p = plan(m, imported)
		return nil
	})
	return p, err
}

// Import adds and refreshes imported hosts (see ImportPlan). Conflicting
// hosts are overwritten only if named in overwrite; their tags and notes
// stay either way. A new alias without tags inherits tags and note from
// the strictest existing host at the same address, so it cannot shed
// env=prod.
func (s *Store) Import(imported []Host, overwrite []string) (ImportPlan, error) {
	var p ImportPlan
	err := s.Update(func(m map[string]Host) error {
		p = plan(m, imported)
		kept := []Conflict{}
		for _, c := range p.Conflicts {
			if slices.Contains(overwrite, c.Name) {
				continue
			}
			kept = append(kept, c)
		}
		byName := map[string]Host{}
		for _, h := range imported {
			byName[h.Name] = h
		}
		var over []HostChange
		for _, name := range overwrite {
			if h, ok := byName[name]; ok && slices.ContainsFunc(p.Conflicts, func(c Conflict) bool { return c.Name == name }) {
				over = append(over, HostChange{Name: name, Changes: diff(m[name], h)})
			}
		}
		for _, h := range imported {
			old, ok := m[h.Name]
			if slices.ContainsFunc(kept, func(c Conflict) bool { return c.Name == h.Name }) {
				continue
			}
			h.ImportedFP = connFingerprint(h)
			if ok {
				h.Tags, h.Note = old.Tags, old.Note
			} else if len(h.Tags) == 0 {
				if same, ok := SameAddress(m, h); ok {
					h.Tags, h.Note = maps.Clone(same.Tags), same.Note
				}
			}
			m[h.Name] = h
		}
		p.Updated = append(p.Updated, over...)
		sort.Slice(p.Updated, func(i, j int) bool { return p.Updated[i].Name < p.Updated[j].Name })
		p.Conflicts = kept
		return nil
	})
	return p, err
}
