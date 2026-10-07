package ssh

import (
	"encoding/json"
	"testing"
)

func TestEffectivePolicyDefaults(t *testing.T) {
	p, err := EffectivePolicy(json.RawMessage(`{"hosts_allow":["staging-*"]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.CertTTLMinutes != 15 || p.ExecTimeoutSeconds != 60 || p.MaxConcurrentSessions != 2 {
		t.Errorf("defaults wrong: %+v", p)
	}
	// platform deny defaults present
	if len(p.CommandDeny) < len(defaultCommandDeny) {
		t.Error("platform deny defaults missing")
	}
}

func TestEffectivePolicyTightestWins(t *testing.T) {
	ce := json.RawMessage(`{"hosts_allow":["staging-*","prod-*"],"cert_ttl_minutes":30,"exec_timeout_seconds":120,"max_concurrent_sessions":4}`)
	con := json.RawMessage(`{"hosts_allow":["staging-*"],"cert_ttl_minutes":20,"exec_timeout_seconds":60,"max_concurrent_sessions":1}`)
	p, err := EffectivePolicy(ce, con)
	if err != nil {
		t.Fatal(err)
	}
	if p.CertTTLMinutes != 20 || p.ExecTimeoutSeconds != 60 || p.MaxConcurrentSessions != 1 {
		t.Errorf("tightest not selected: %+v", p)
	}
	if len(p.HostsAllow) != 1 || p.HostsAllow[0] != "staging-*" {
		t.Errorf("grant hosts_allow should win: %v", p.HostsAllow)
	}
}

func TestEffectivePolicyNoEscalation(t *testing.T) {
	ce := json.RawMessage(`{"hosts_allow":["staging-*"]}`)
	con := json.RawMessage(`{"hosts_allow":["prod-db*"]}`)
	if _, err := EffectivePolicy(ce, con); err == nil {
		t.Fatal("host escalation must be rejected")
	}
	// ceiling allows nothing → constraint cannot add
	ce2 := json.RawMessage(`{}`)
	con2 := json.RawMessage(`{"hosts_allow":["x"]}`)
	if _, err := EffectivePolicy(ce2, con2); err == nil {
		t.Fatal("allow where ceiling allows none must be rejected")
	}
}

func TestCheckCommandTable(t *testing.T) {
	p, err := EffectivePolicy(json.RawMessage(`{"hosts_allow":["*"]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		cmd     string
		want    CommandVerdict
		pattern string
	}{
		{"rm -rf /var/log", CommandGate, "rm -rf *"},
		{"sudo rm -rf x", CommandGate, "rm -rf *"},
		{"dd if=/dev/zero of=/dev/sda", CommandGate, "dd *of=*"},
		{"systemctl stop nginx", CommandGate, "systemctl stop *"},
		{"mkfs.ext4 /dev/sdb1", CommandGate, "mkfs*"},
		{"shutdown -h now", CommandGate, "shutdown*"},
		{"echo x > /etc/passwd", CommandGate, "*> /etc/*"},
		{"journalctl -u nginx", CommandPass, ""},
		{"ls -la", CommandPass, ""},
		{"cat /var/log/app.log", CommandPass, ""},
	}
	for _, tc := range cases {
		got, pat := p.CheckCommand(tc.cmd)
		if got != tc.want || (tc.want == CommandGate && pat != tc.pattern) {
			t.Errorf("%q: got %v/%q want %v/%q", tc.cmd, got, pat, tc.want, tc.pattern)
		}
	}
}

func TestCheckCommandWithAllowList(t *testing.T) {
	p, err := EffectivePolicy(json.RawMessage(`{"hosts_allow":["*"],"command_allow":["journalctl *","ls *"]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := p.CheckCommand("journalctl -u x"); v != CommandPass {
		t.Errorf("allowed command rejected: %v", v)
	}
	if v, _ := p.CheckCommand("ls -la"); v != CommandPass {
		t.Errorf("allowed command rejected: %v", v)
	}
	if v, _ := p.CheckCommand("cat /etc/hosts"); v != CommandDeny {
		t.Errorf("command outside allow must be CommandDeny, got %v", v)
	}
	// deny still wins over allow
	if v, pat := p.CheckCommand("ls /; rm -rf /"); v != CommandGate || pat != "rm -rf *" {
		t.Errorf("deny must win over allow: %v %q", v, pat)
	}
}

func TestHostAllowed(t *testing.T) {
	p, err := EffectivePolicy(json.RawMessage(`{"hosts_allow":["staging-*","prod-web-*"],"hosts_deny":["prod-db*"]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !p.HostAllowed("staging-1") || !p.HostAllowed("prod-web-3") {
		t.Error("allowed hosts rejected")
	}
	if p.HostAllowed("prod-db1") {
		t.Error("denied host accepted")
	}
	if p.HostAllowed("random.host") {
		t.Error("default-deny violated")
	}
}
