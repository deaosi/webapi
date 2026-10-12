package server

import "testing"

func TestChannelModelMatch(t *testing.T) {
	tests := []struct {
		allowed []string
		model   string
		want    bool
	}{
		{nil, "glm-5", true},
		{[]string{}, "glm-5", true},
		{[]string{""}, "glm-5", false},
		{[]string{"glm-5"}, "glm-5", true},
		{[]string{"glm-5"}, "glm-5.1", false},
		{[]string{"glm-*"}, "glm-5.3-flash", true},
		{[]string{"glm-*"}, "deepseek-v4", false},
		{[]string{"glm-5", "deepseek-*", ""}, "deepseek-v4", true},
		{[]string{"*"}, "anything", true},
	}
	for i, tt := range tests {
		if got := ChannelAllowedModel(tt.allowed, tt.model); got != tt.want {
			t.Fatalf("case %d: ChannelAllowedModel(%q,%q)=%v want %v", i, tt.allowed, tt.model, got, tt.want)
		}
	}
}

func TestResolveModelExplicit(t *testing.T) {
	realm, bare, explicit := resolveModelExplicit("cn:glm-5")
	if realm != "cn" || bare != "glm-5" || !explicit {
		t.Fatalf("got (%q,%q,%v) want (cn,glm-5,true)", realm, bare, explicit)
	}
	realm, bare, explicit = resolveModelExplicit("global:gpt")
	if realm != "global" || bare != "gpt" || !explicit {
		t.Fatalf("got (%q,%q,%v) want (global,gpt,true)", realm, bare, explicit)
	}
	realm, bare, explicit = resolveModelExplicit("glm-5")
	if realm != "cn" || bare != "glm-5" || explicit {
		t.Fatalf("got (%q,%q,%v) want (cn,glm-5,false)", realm, bare, explicit)
	}
	realm, bare, explicit = resolveModelExplicit("foo:bar")
	if realm != "cn" || bare != "foo:bar" || explicit {
		t.Fatalf("got (%q,%q,%v) want (cn,foo:bar,false)", realm, bare, explicit)
	}
}
