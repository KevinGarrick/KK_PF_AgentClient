package main

import (
	"encoding/json"
	"testing"
)

func TestBuildNftConfigIncludesRoundRobinCommentsAndMasquerade(t *testing.T) {
	text := buildNftConfig(AgentConfig{
		ConfigSchemaVersion: 1,
		ConfigVersion:       2,
		GroupID:             1,
		NftTable:            "inet tforward",
		Rules: []AgentRule{{
			ID:         12,
			ListenPort: 30001,
			Protocol:   "both",
			Enabled:    true,
			Upstreams: []Upstream{
				{ID: 1, IP: "10.0.0.1", Port: 440, Enabled: true},
				{ID: 2, IP: "10.0.0.2", Port: 441, Enabled: true},
			},
		}},
	})

	mustContain(t, text, "numgen inc mod 2 map { 0 : 10.0.0.1 . 440, 1 : 10.0.0.2 . 441 }")
	mustContain(t, text, "comment \"tforward-rule-12-tcp\"")
	mustContain(t, text, "comment \"tforward-rule-12-udp\"")
	mustContain(t, text, "ip daddr 10.0.0.1 counter masquerade")
}

func TestPrecheckRejectsInvalidConfigWithoutTouchingPorts(t *testing.T) {
	err := precheckConfig(AgentConfig{
		ConfigSchemaVersion: 1,
		Rules: []AgentRule{{
			ID:         1,
			ListenPort: 30001,
			Protocol:   "tcp",
			Enabled:    false,
			Upstreams:  []Upstream{{ID: 1, IP: "2001:db8::1", Port: 440, Enabled: true}},
		}},
	})
	if err == nil {
		t.Fatal("expected invalid IPv6 upstream to fail")
	}

	err = precheckConfig(AgentConfig{
		ConfigSchemaVersion: 1,
		Rules: []AgentRule{
			{ID: 1, ListenPort: 30001, Protocol: "tcp", Enabled: false},
			{ID: 2, ListenPort: 30001, Protocol: "udp", Enabled: false},
		},
	})
	if err == nil {
		t.Fatal("expected duplicate listen port to fail")
	}
}

func TestCollectNftCountersParsesRuleComments(t *testing.T) {
	raw := []byte(`{
	  "nftables": [
	    {"rule": {"expr": [
	      {"counter": {"packets": 3, "bytes": 1200}},
	      {"comment": "tforward-rule-12-tcp"}
	    ]}},
	    {"rule": {"expr": [
	      {"counter": {"packets": 2, "bytes": 800}},
	      {"comment": "tforward-rule-12-udp"}
	    ]}},
	    {"rule": {"expr": [
	      {"counter": {"packets": 1, "bytes": 50}},
	      {"comment": "unrelated"}
	    ]}}
	  ]
	}`)
	var payload interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	out := map[int]uint64{}
	collectNftCounters(payload, out)
	if out[12] != 2000 {
		t.Fatalf("expected 2000 bytes for rule 12, got %d", out[12])
	}
}

func TestExpectedRuleCommentsMatchEnabledProtocols(t *testing.T) {
	comments := expectedRuleComments(AgentConfig{
		ConfigSchemaVersion: 1,
		Rules: []AgentRule{
			{
				ID:         1,
				ListenPort: 30001,
				Protocol:   "both",
				Enabled:    true,
				Upstreams:  []Upstream{{ID: 1, IP: "10.0.0.1", Port: 440, Enabled: true}},
			},
			{
				ID:         2,
				ListenPort: 30002,
				Protocol:   "tcp",
				Enabled:    false,
				Upstreams:  []Upstream{{ID: 2, IP: "10.0.0.2", Port: 440, Enabled: true}},
			},
			{
				ID:         3,
				ListenPort: 30003,
				Protocol:   "udp",
				Enabled:    true,
				Upstreams:  []Upstream{{ID: 3, IP: "10.0.0.3", Port: 440, Enabled: false}},
			},
		},
	})
	if len(comments) != 2 || comments[0] != "tforward-rule-1-tcp" || comments[1] != "tforward-rule-1-udp" {
		t.Fatalf("unexpected comments: %#v", comments)
	}
}

func mustContain(t *testing.T, text string, want string) {
	t.Helper()
	if !contains(text, want) {
		t.Fatalf("expected %q to contain %q", text, want)
	}
}

func contains(text string, want string) bool {
	for i := 0; i+len(want) <= len(text); i++ {
		if text[i:i+len(want)] == want {
			return true
		}
	}
	return false
}
