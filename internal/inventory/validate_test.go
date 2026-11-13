package inventory

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	saved := map[string]Host{"bastion": {Name: "bastion", HostName: "10.0.0.1"}}
	ok := []Host{
		{Name: "web-1", HostName: "10.0.1.1"},
		{Name: "db.main_2", HostName: "db.example.com", Port: 2222, User: "ops", ProxyJump: "bastion"},
		{Name: "x", HostName: "x", ProxyJump: "ops@jump.example.com:2200"},
		{Name: "y", HostName: "fe80::1", ProxyJump: "jump.example.com,bastion"},
	}
	for _, h := range ok {
		if err := Validate(h, saved); err != nil {
			t.Errorf("%+v: %v", h, err)
		}
	}
	bad := map[string]Host{
		"name":                        {Name: "", HostName: "x"},
		"spaces":                      {Name: "web 1", HostName: "x"},
		"slashes":                     {Name: "a/b", HostName: "x"},
		"address":                     {Name: "a", HostName: ""},
		"address must not":            {Name: "a", HostName: "x y"},
		"port":                        {Name: "a", HostName: "x", Port: 70000},
		"user":                        {Name: "a", HostName: "x", User: "a b"},
		"jump host \"nope\"":          {Name: "a", HostName: "x", ProxyJump: "nope"},
		"its own jump host":           {Name: "a", HostName: "x", ProxyJump: "a"},
		"port in jump host":           {Name: "a", HostName: "x", ProxyJump: "j:99999"},
		"starts with a dash":          {Name: "-a", HostName: "x"},
		"address must not start with": {Name: "a", HostName: "-oProxyCommand=x"},
	}
	for want, h := range bad {
		err := Validate(h, saved)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: err = %v, want %q", h, err, want)
		}
	}
}
