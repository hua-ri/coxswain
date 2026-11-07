package policy

import (
	"testing"

	"github.com/hua-ri/coxswain/internal/action"
)

func TestClassifyCommand(t *testing.T) {
	cases := []struct {
		cmd  string
		want action.Risk
	}{
		{"df -h", action.Read},
		{"ps aux | grep nginx | head -n 5", action.Read},
		{"journalctl -u nginx --since '1 hour ago' | tail -200", action.Read},
		{"cat /etc/os-release && uptime", action.Read},
		{"sudo -n systemctl status nginx", action.Read},
		{"docker ps -a", action.Read},
		{"ls /var/log > /dev/null 2>&1", action.Read},
		{"sed -n '1,20p' /etc/hosts", action.Read},
		{"find /var/log -name '*.gz' -mtime +7", action.Read},
		{"echo $(hostname)", action.Read},

		{"systemctl restart nginx", action.Write},
		{"sudo systemctl reload nginx", action.Write},
		{"docker restart api", action.Write},
		{"rm /tmp/a.log", action.Write},
		{"echo hi > /tmp/x", action.Write},
		{"kill 1234", action.Write},
		{"curl -X POST http://x", action.Write},
		{"sed -i 's/a/b/' /opt/app.conf", action.Write},
		{`find /tmp -name x -exec rm {} \;`, action.Write},
		{"unknown-tool --flag", action.Write},

		{"rm -rf /var/lib/mysql", action.Destructive},
		{"sudo rm -r /data", action.Destructive},
		{"reboot", action.Destructive},
		{"sudo -u root shutdown -h now", action.Destructive},
		{"mkfs.ext4 /dev/sdb1", action.Destructive},
		{"dd if=/dev/zero of=/dev/sda", action.Destructive},
		{"echo x > /etc/passwd", action.Destructive},
		{"echo key | tee -a ~/.ssh/authorized_keys", action.Destructive},
		{"cp evil /etc/sudoers.d/x", action.Destructive},
		{"find / -name core -delete", action.Destructive},
		{":(){ :|:& };:", action.Destructive},
		{"$CMD arg", action.Destructive},
		{"bash -c 'rm -rf /opt/app'", action.Destructive},
		{"bash -c 'uptime'", action.Read},
		{"ls | xargs rm -rf", action.Destructive},
		{"eval \"$X\"", action.Destructive},
		{"echo 'unterminated", action.Destructive},
		{"echo hi > $FILE", action.Destructive},
	}
	for _, c := range cases {
		if got := ClassifyCommand(c.cmd); got != c.want {
			t.Errorf("ClassifyCommand(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}
