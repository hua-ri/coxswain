package inventory

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestPlanAndImport(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "hosts.json"))
	first := []Host{
		{Name: "king", HostName: "10.0.0.1", Port: 22, User: "root", Source: "ssh_config"},
		{Name: "same", HostName: "10.0.0.5", Port: 22, Source: "ssh_config"},
		{Name: "edited", HostName: "10.0.0.6", Port: 22, Source: "ssh_config"},
	}
	if _, err := s.Import(first, nil); err != nil {
		t.Fatal(err)
	}
	// The user edits one in Coxswain, and adds one by hand that the config also has.
	s.Update(func(m map[string]Host) error {
		h := m["edited"]
		h.HostName, h.Tags = "10.9.9.9", map[string]string{"env": "prod"}
		m["edited"] = h
		m["mine"] = Host{Name: "mine", HostName: "192.168.1.2", Source: "manual"}
		return nil
	})
	next := []Host{
		{Name: "king", HostName: "10.0.0.2", Port: 22, User: "root", Source: "ssh_config"},
		{Name: "same", HostName: "10.0.0.5", Port: 22, Source: "ssh_config"},
		{Name: "edited", HostName: "10.0.0.7", Port: 22, Source: "ssh_config"},
		{Name: "mine", HostName: "192.168.1.3", Port: 22, Source: "ssh_config"},
		{Name: "new", HostName: "10.0.0.8", Port: 2222, User: "ops", Source: "ssh_config"},
	}
	plan, err := s.PlanImport(next)
	if err != nil {
		t.Fatal(err)
	}
	want := ImportPlan{
		Added:     []NewHost{{Name: "new", Address: "ops@10.0.0.8:2222"}},
		Updated:   []HostChange{{Name: "king", Changes: []FieldChange{{Field: "hostname", From: "10.0.0.1", To: "10.0.0.2"}}}},
		Unchanged: []string{"same"},
		Conflicts: []Conflict{
			{Name: "edited", Field: "hostname", From: "10.9.9.9", To: "10.0.0.7"},
			{Name: "mine", Field: "hostname", From: "192.168.1.2", To: "192.168.1.3"},
		},
	}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("plan\n%+v\nwant\n%+v", plan, want)
	}
	// A dry run changes nothing.
	if h, _ := s.Get("king"); h.HostName != "10.0.0.1" {
		t.Fatal("planning wrote")
	}

	got, err := s.Import(next, []string{"edited"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Updated) != 2 || got.Updated[0].Name != "edited" || len(got.Conflicts) != 1 || got.Conflicts[0].Name != "mine" {
		t.Fatalf("import: %+v", got)
	}
	if h, _ := s.Get("edited"); h.HostName != "10.0.0.7" || h.Tags["env"] != "prod" {
		t.Fatalf("overwritten host lost its tags or kept its address: %+v", h)
	}
	if h, _ := s.Get("mine"); h.HostName != "192.168.1.2" || h.Source != "manual" {
		t.Fatalf("kept host changed: %+v", h)
	}
	if h, _ := s.Get("new"); h.Port != 2222 {
		t.Fatalf("%+v", h)
	}
	// Once overwritten, it is no longer a conflict.
	if plan, _ := s.PlanImport(next); len(plan.Conflicts) != 1 || len(plan.Updated) != 0 || len(plan.Unchanged) != 4 {
		t.Fatalf("after: %+v", plan)
	}
}

// TestPlanLegacyHosts: hosts imported before fingerprints existed are
// refreshed, not reported as edited.
func TestPlanLegacyHosts(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "hosts.json"))
	s.Update(func(m map[string]Host) error {
		m["old"] = Host{Name: "old", HostName: "10.0.0.1", Port: 22, Source: "ssh_config"}
		return nil
	})
	plan, _ := s.PlanImport([]Host{{Name: "old", HostName: "10.0.0.2", Port: 22, Source: "ssh_config"}})
	if len(plan.Updated) != 1 || len(plan.Conflicts) != 0 {
		t.Fatalf("%+v", plan)
	}
}
