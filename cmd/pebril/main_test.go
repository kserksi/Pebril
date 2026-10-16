package main

import "testing"

func TestCommandArgsRequireInvocationGrant(t *testing.T) {
	for _, c := range []struct {
		args        []string
		command     string
		allow, fail bool
	}{
		{command: "serve"},
		{args: []string{"--allow-low-memory"}, command: "serve", allow: true},
		{args: []string{"serve", "--allow-low-memory"}, command: "serve", allow: true},
		{args: []string{"--allow-low-memory", "serve"}, command: "serve", allow: true},
		{args: []string{"check"}, command: "check"},
		{args: []string{"check", "--allow-low-memory"}, command: "check", allow: true},
		{args: []string{"version"}, command: "version"},
		{args: []string{"serve", "check"}, fail: true},
		{args: []string{"--allow-downloads"}, fail: true},
		{args: []string{"--allow-system-install"}, fail: true},
		{args: []string{"--allow-low-memory=true"}, fail: true},
		{args: []string{"unknown"}, fail: true},
	} {
		command, allow, err := commandArgs(c.args)
		if (err != nil) != c.fail || !c.fail && (command != c.command || allow != c.allow) {
			t.Fatalf("%q: command=%s allow=%t err=%v", c.args, command, allow, err)
		}
	}
	t.Setenv("PEBRIL_ALLOW_LOW_MEMORY", "true")
	if _, allow, err := commandArgs(nil); err != nil || allow {
		t.Fatal("Environment authorized restricted startup")
	}
}
