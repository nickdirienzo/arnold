package arnold

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func helperPaddock(t *testing.T) (Paddock, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX subprocess fixture")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "paddock")
	record := filepath.Join(dir, "call.json")
	script := "#!/bin/sh\nexec " + shellQuote(os.Args[0]) + " -test.run=TestPaddockHelper -- " + shellQuote(record) + " \"$@\"\n"
	if e := os.WriteFile(path, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	return Paddock{Binary: path}, record
}
func TestPaddockHelper(t *testing.T) {
	index := -1
	for i, a := range os.Args {
		if a == "--" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	record := os.Args[index+1]
	args := os.Args[index+2:]
	b, _ := json.Marshal(args)
	os.WriteFile(record, b, 0600)
	flag := func(name string) string {
		for i, a := range args {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	if os.Getenv("SLACK_BOT_TOKEN") != "" || os.Getenv("PADDOCK_TASK_ID") != "" {
		os.Exit(9)
	}
	switch args[0] {
	case "run":
		prompt, e := os.ReadFile(flag("--prompt-file"))
		if e != nil {
			os.Exit(2)
		}
		if string(prompt) != "Literal $(echo nope) and `pwd`" {
			os.Exit(3)
		}
		var contextData map[string]string
		b, e := os.ReadFile(flag("--context-file"))
		if e != nil || json.Unmarshal(b, &contextData) != nil || contextData["channel"] != "C1" {
			os.Exit(4)
		}
		if flag("--profile") != "" || flag("--launcher") == "" {
			os.Exit(6)
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{"id": "task1", "runtime": flag("--runtime"), "idle_ttl": flag("--idle-ttl"), "session": map[string]string{"agent": flag("--agent")}, "provenance": map[string]string{"identity": flag("--identity"), "launcher": flag("--launcher")}})
	case "message":
		json.NewEncoder(os.Stdout).Encode(map[string]string{"status": args[2]})
		if args[2] != "delivered" {
			os.Exit(1)
		}
	case "output":
		json.NewEncoder(os.Stdout).Encode(OutputPage{TaskID: args[1], Events: []OutputEvent{}, NextCursor: flag("--after")})
	case "identity":
		fmt.Fprintln(os.Stdout, `{"engineer":{"mode":"app","profiles":["investigation","implementation"]}}`)
	case "launcher":
		fmt.Fprintln(os.Stdout, `{"implementation":{"runtime":"openshell"},"investigation":{"runtime":"openshell","read_only":true}}`)
	case "oversize":
		for i := 0; i < 100000; i++ {
			fmt.Fprint(os.Stdout, strings.Repeat("x", 4096))
		}
	default:
		os.Exit(5)
	}
	os.Exit(0)
}
func TestPaddockCLIArgumentsEnvironmentAndReceipts(t *testing.T) {
	p, path := helperPaddock(t)
	t.Setenv("SLACK_BOT_TOKEN", "sentinel-secret")
	t.Setenv("PADDOCK_TASK_ID", "unrelated-task")
	for _, agent := range []string{"claude", "codex"} {
		task, e := p.Run(testContext, RunOptions{Prompt: "Literal $(echo nope) and `pwd`", Repo: "project", Actor: "U1", Identity: "engineer", Profile: "implementation", Agent: agent, ExternalID: "stable", Context: map[string]string{"channel": "C1"}})
		if e != nil {
			t.Fatal(e)
		}
		if task.Session.Agent != agent || task.Runtime != "openshell" {
			t.Fatal("runtime/agent changed")
		}
		raw, _ := os.ReadFile(path)
		var args []string
		json.Unmarshal(raw, &args)
		if !strings.Contains(strings.Join(args, " "), "--requester-source arnold-slack") || !strings.Contains(strings.Join(args, " "), "--detach") {
			t.Fatal("missing integration flags")
		}
	}
	for _, status := range []string{"pending", "claimed", "uncertain", "delivered"} {
		r, e := p.Message(testContext, "task1", status, "stable")
		if e != nil || r.Status != status {
			t.Fatal("receipt lost", r, e)
		}
	}
	if _, e := p.Output(testContext, "task1", "opaque-$(echo nope)"); e != nil {
		t.Fatal(e)
	}
	raw, _ := os.ReadFile(path)
	var args []string
	json.Unmarshal(raw, &args)
	if strings.Join(args, "|") != "output|task1|--after|opaque-$(echo nope)|--limit|20|--json" {
		t.Fatal("cursor changed")
	}
	c := Config{Users: map[string]Actor{"U1": {Identity: "engineer", Profile: "investigation"}}}
	if e := p.Check(testContext, c); e != nil {
		t.Fatal(e)
	}
	c.Users["U1"] = Actor{Identity: "missing", Profile: "investigation"}
	if p.Check(testContext, c) == nil {
		t.Fatal("missing identity accepted")
	}
}
func TestPaddockOversizedOutputIsBoundedAndCancelled(t *testing.T) {
	p, _ := helperPaddock(t)
	ctx, cancel := context.WithTimeout(testContext, 15*time.Second)
	defer cancel()
	start := time.Now()
	var out any
	if e := p.call(ctx, []string{"oversize"}, &out, 1024); e == nil {
		t.Fatal("unbounded output accepted")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("oversized child was not stopped promptly")
	}
}

func TestPaddockCheckRejectsUnsafeLaunchers(t *testing.T) {
	for name, launchers := range map[string]string{
		"missing":  `{}`,
		"host":     `{"investigation":{"runtime":"host","read_only":true}}`,
		"writable": `{"investigation":{"runtime":"openshell"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "paddock")
			script := "#!/bin/sh\ncase $1 in\nidentity) echo '{\"engineer\":{\"mode\":\"app\"}}';;\nlauncher) echo " + shellQuote(launchers) + ";;\nesac\n"
			if err := os.WriteFile(path, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			c := Config{Users: map[string]Actor{"U1": {Identity: "engineer", Profile: "investigation"}}}
			if err := (Paddock{Binary: path}).Check(testContext, c); err == nil {
				t.Fatal("unsafe launcher accepted")
			}
		})
	}
}
