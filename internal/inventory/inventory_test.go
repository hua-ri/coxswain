package inventory

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const sshConfig = `
Host *
  ServerAliveInterval 30

Host bastion
  HostName 10.0.0.1
  User ops
  IdentityFile ~/.ssh/id_ed25519

Host web-1 web-2
  HostName 10.0.1.%h
  User deploy
  Port 2222
  ProxyJump bastion

Host db-*
  User dba

Host db-main
  HostName 10.0.2.5
`

func TestImportSSHConfig(t *testing.T) {
	hosts, err := ImportSSHConfig(strings.NewReader(sshConfig), "/home/k")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, h := range hosts {
		names = append(names, h.Name)
	}
	if !reflect.DeepEqual(names, []string{"bastion", "web-1", "web-2", "db-main"}) {
		t.Fatalf("names = %v", names)
	}
	b := hosts[0]
	if b.HostName != "10.0.0.1" || b.User != "ops" || b.Port != 22 ||
		!reflect.DeepEqual(b.IdentityFiles, []string{"/home/k/.ssh/id_ed25519"}) {
		t.Fatalf("bastion = %+v", b)
	}
	w := hosts[1]
	if w.HostName != "10.0.1.web-1" || w.Port != 2222 || w.User != "deploy" || w.ProxyJump != "bastion" {
		t.Fatalf("web-1 = %+v", w)
	}
	if d := hosts[3]; d.User != "dba" || d.HostName != "10.0.2.5" {
		t.Fatalf("db-main = %+v (wildcard settings must apply)", d)
	}
}

func TestStoreMergeKeepsTags(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "hosts.json"))
	if _, _, err := s.Merge([]Host{{Name: "web", HostName: "10.0.0.1"}}); err != nil {
		t.Fatal(err)
	}
	err := s.Update(func(m map[string]Host) error {
		h := m["web"]
		h.Tags = map[string]string{"env": "prod"}
		h.Note = "do not restart during business hours"
		m["web"] = h
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	added, updated, err := s.Merge([]Host{{Name: "web", HostName: "10.0.0.2"}, {Name: "db", HostName: "10.0.0.3"}})
	if err != nil || added != 1 || updated != 1 {
		t.Fatalf("merge: %d %d %v", added, updated, err)
	}
	h, err := s.Get("web")
	if err != nil || h.HostName != "10.0.0.2" || h.Tags["env"] != "prod" || h.Note == "" {
		t.Fatalf("web = %+v %v", h, err)
	}
	list, _ := s.List()
	if len(list) != 2 || list[0].Name != "db" {
		t.Fatalf("list = %+v", list)
	}
	if _, err := s.Get("nope"); err == nil {
		t.Fatal("expected not found")
	}
}
