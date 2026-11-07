package inventory

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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

func TestMergeNewAliasInheritsPolicy(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "hosts.json"))
	s.Merge([]Host{{Name: "prod", HostName: "10.0.0.5", Port: 22}, {Name: "b-prod", HostName: "10.0.0.5", Port: 22}})
	s.Update(func(m map[string]Host) error {
		h := m["prod"]
		h.Tags, h.Note = map[string]string{"env": "prod"}, "read only"
		m["prod"] = h
		return nil
	})
	added, updated, err := s.Merge([]Host{
		{Name: "alias", HostName: "10.0.0.5", Port: 22},
		{Name: "other-port", HostName: "10.0.0.5", Port: 2222},
		{Name: "tagged", HostName: "10.0.0.5", Port: 22, Tags: map[string]string{"x": "y"}},
	})
	if err != nil || added != 3 || updated != 0 {
		t.Fatalf("%d %d %v", added, updated, err)
	}
	if h, _ := s.Get("alias"); h.Tags["env"] != "prod" || h.Note != "read only" {
		t.Fatalf("alias: %+v", h)
	}
	if h, _ := s.Get("other-port"); h.Tags != nil {
		t.Fatalf("other port must not inherit: %+v", h)
	}
	if h, _ := s.Get("tagged"); h.Tags["env"] != "" {
		t.Fatalf("tagged import keeps its own tags: %+v", h)
	}
}

// TestConcurrentStores: separate Store values (as in separate processes)
// on one file must not lose each other's updates (review I5).
func TestConcurrentStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.json")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := NewStore(path).Update(func(m map[string]Host) error {
				name := fmt.Sprintf("h%02d", i)
				m[name] = Host{Name: name, HostName: name}
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	hosts, err := NewStore(path).List()
	if err != nil || len(hosts) != 20 {
		t.Fatalf("got %d hosts, %v", len(hosts), err)
	}
}
