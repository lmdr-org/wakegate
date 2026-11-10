package proxy

import (
	"strings"
	"testing"
	"time"
)

const minimal = `
target:
  host: 192.168.1.20
  mac: 04:d9:f5:cb:75:23
services:
  - name: ollama
    listen: 11434
    upstream: 11434
`

func TestConfigDefaults(t *testing.T) {
	c, err := ParseConfig([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Services[0]
	if c.Target.Name != "192.168.1.20" || c.Target.Broadcast[0] != "255.255.255.255" ||
		c.Target.BootTimeout != 3*time.Minute || c.Target.Probe != "192.168.1.20:11434" || c.MaxBodyMB != 16 ||
		c.Admin.Listen != "" || s.Listen != ":11434" || s.Addr() != "192.168.1.20:11434" || !*s.WaitingPage ||
		s.StartTimeout != time.Minute {
		t.Fatalf("defaults: %+v %+v", c, s)
	}
	if !s.Wakes("GET", "/anything") {
		t.Fatal("without wake_on, every request wakes")
	}
}

func TestConfigAgentAndTCP(t *testing.T) {
	c, err := ParseConfig([]byte(`
target:
  host: tower.lan
  wake_command: [ssh, router, etherwake, "04:d9:f5:cb:75:23"]
  agent: http://tower.lan:9777/
admin:
  listen: "127.0.0.1:9778"
services:
  - name: ssh
    tcp: true
    listen: ":2222"
    upstream: 22
  - name: s3
    listen: ":7070"
    upstream: "10.0.0.5:7070"
    never_wake: [GET /health]
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Target.Agent != "http://tower.lan:9777" || c.Target.AgentTokenFile != "/etc/wakegate/agent.token" ||
		c.Target.Probe != "" || *c.Services[0].WaitingPage || c.Services[1].Addr() != "10.0.0.5:7070" ||
		c.Services[1].Wakes("GET", "/health") {
		t.Fatalf("%+v", c)
	}
}

func TestConfigErrors(t *testing.T) {
	cases := map[string]string{
		"host":       strings.Replace(minimal, "host: 192.168.1.20", "", 1),
		"mac":        strings.Replace(minimal, "04:d9:f5:cb:75:23", "nope", 1),
		"name":       strings.Replace(minimal, "name: ollama", "name: Ollama!", 1),
		"listen":     strings.Replace(minimal, "listen: 11434", "listen: 99999", 1),
		"upstream":   strings.Replace(minimal, "upstream: 11434", "upstream: ''", 1),
		"unknown":    minimal + "    color: blue\n",
		"rule":       minimal + "    wake_on: [POST api/chat]\n",
		"tcp rules":  minimal + "    tcp: true\n    wake_on: [/x]\n",
		"health":     minimal + "    health: health\n",
		"duplicate":  minimal + "  - name: ollama\n    listen: 1\n    upstream: 1\n",
		"same port":  minimal + "  - name: other\n    listen: 11434\n    upstream: 1\n",
		"agent url":  minimal + "target2: x\n",
		"broadcast":  strings.Replace(minimal, "mac: 04:d9:f5:cb:75:23", "mac: 04:d9:f5:cb:75:23\n  broadcast: [lan]", 1),
		"timeout":    strings.Replace(minimal, "mac: 04:d9:f5:cb:75:23", "mac: 04:d9:f5:cb:75:23\n  boot_timeout: 1s", 1),
		"no service": "target: {host: h, mac: '04:d9:f5:cb:75:23'}\n",
	}
	for name, raw := range cases {
		if _, err := ParseConfig([]byte(raw)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestExampleConfig(t *testing.T) {
	c, err := LoadConfig("../../deploy/proxy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Target.Agent == "" || c.Admin.Listen != ":9778" || len(c.Services) == 0 {
		t.Fatalf("%+v", c)
	}
	s := c.Services[0]
	if s.Wakes("GET", "/api/tags") || !s.Wakes("POST", "/api/chat") || !s.Wakes("POST", "/v1/chat/completions") {
		t.Fatal("the Ollama example wakes the wrong requests")
	}
}
