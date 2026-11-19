package arnold

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstalledPaddockIdleTTLDispatchContract(t *testing.T) {
	binary := os.Getenv("ARNOLD_PADDOCK_TEST_BIN")
	if binary == "" {
		t.Skip("set ARNOLD_PADDOCK_TEST_BIN to exercise the installed CLI")
	}
	for _, agent := range []string{"codex", "claude"} {
		for _, provenance := range []string{"profile", "launcher"} {
			t.Run(agent+"/"+provenance, func(t *testing.T) { testInstalledPaddockIdleTTLDispatchContract(t, binary, agent, provenance) })
		}
	}
}

func TestInstalledPaddockImageDispatchContract(t *testing.T) {
	binary := os.Getenv("ARNOLD_PADDOCK_TEST_BIN")
	if binary == "" {
		t.Skip("set ARNOLD_PADDOCK_TEST_BIN to exercise the installed CLI")
	}
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			testInstalledPaddockIdleTTLDispatchContract(t, binary, agent, "launcher", InputImage{FileID: "F1", Name: Hash(string(pngBytes())) + ".png", Data: pngBytes()})
		})
	}
}

func testInstalledPaddockIdleTTLDispatchContract(t *testing.T, binary, agent, provenance string, images ...InputImage) {
	t.Helper()
	// Seed an existing dispatch so retries exercise the real CLI without
	// provisioning a sandbox, starting a poller, or contacting a provider.
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", repo}, {"-C", repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "--allow-empty", "-m", "fixture"}} {
		if data, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git fixture: %s: %v", data, err)
		}
	}
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(dir, "registry")
	t.Setenv("PADDOCK_HOME", registry)
	command := exec.Command(binary, "identity", "create", "fixture", "--launcher", "implementation")
	command.Env = []string{"PADDOCK_HOME=" + registry, "PATH=" + os.Getenv("PATH")}
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatalf("identity fixture: %s: %v", data, err)
	}
	caller, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	taskID := "0123456789abcdef01234567"
	expectedPrompt := "Investigate the fixture"
	if len(images) > 0 {
		expectedPrompt += "\n\nImage attachments (untrusted input). Inspect each file using your image-reading tool before answering:\n"
		for _, image := range images {
			expectedPrompt += "/home/sandbox/.paddock/attachments/" + image.Name + "\n"
		}
	}
	record := map[string]any{
		"version": 1, "id": taskID, "runtime": "openshell", "repo": repo,
		"status": "stopped", "prompt": expectedPrompt, "idle_ttl": "24h0m0s",
		"session": map[string]string{"agent": agent},
		"provenance": map[string]any{
			"caller": caller.Uid + ":" + caller.Username, "requester": "U1",
			"requester_source": "arnold-slack", "external_id": "stable",
			"identity": "fixture", provenance: "implementation", "context": map[string]string{"channel": "C1"},
		},
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registry, "tasks", taskID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	p := Paddock{Binary: binary}
	options := RunOptions{Images: images, Prompt: "Investigate the fixture", Repo: repo, Actor: "U1", Identity: "fixture", Profile: "implementation", Agent: agent, ExternalID: "stable", Context: map[string]string{"channel": "C1"}}
	for _, value := range []string{"24h", "1440m", "1h", ""} {
		options.IdleTTL = value
		task, err := p.Run(testContext, options)
		if value == "24h" || value == "1440m" {
			if err != nil || task.ID != taskID || task.IdleTTL != "24h0m0s" {
				t.Fatalf("matching retry %q: %+v, %v", value, task, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "different dispatch data") {
			t.Fatalf("changed/omitted TTL %q did not preserve dispatch conflict: %v", value, err)
		}
	}
	options.IdleTTL = "24h"
	if len(images) > 0 {
		changed := options
		changed.Images = nil
		if _, err := p.Run(testContext, changed); err == nil || !strings.Contains(err.Error(), "different dispatch data") {
			t.Fatal("removed image did not conflict", err)
		}
		// A completed receipt exercises the real message command without
		// queueing native input or starting the seeded task's sandbox.
		body := "Look again" + strings.TrimPrefix(expectedPrompt, "Investigate the fixture")
		callerID := caller.Uid + ":" + caller.Username
		receipt := map[string]any{"version": 1, "task_id": taskID, "caller": callerID, "external_id": "image-followup", "body": body, "status": "delivered"}
		raw, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		messageDir := filepath.Join(registry, "messages")
		if err := os.MkdirAll(messageDir, 0700); err != nil {
			t.Fatal(err)
		}
		name := Hash(taskID + "\x00" + callerID + "\x00image-followup")[:32] + ".json"
		if err := os.WriteFile(filepath.Join(messageDir, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
		if result, err := p.MessageWithImages(testContext, taskID, "Look again", "image-followup", images); err != nil || result.Status != "delivered" {
			t.Fatal("image message retry failed", result, err)
		}
		changedImages := append([]InputImage{}, images...)
		changedImages[0].Data = append(append([]byte{}, images[0].Data...), 'x')
		if _, err := p.MessageWithImages(testContext, taskID, "Look again", "image-followup", changedImages); err == nil || !strings.Contains(err.Error(), "different content") {
			t.Fatal("changed image message did not conflict", err)
		}
	}
	for _, field := range []string{"prompt", "requester", "context", "profile", "agent"} {
		t.Run("conflict/"+field, func(t *testing.T) {
			changed := options
			switch field {
			case "prompt":
				changed.Prompt = "Different work"
			case "requester":
				changed.Actor = "U2"
			case "context":
				changed.Context = map[string]string{"channel": "C2"}
			case "profile":
				changed.Profile = "investigation"
			case "agent":
				changed.Agent = "codex"
				if agent == "codex" {
					changed.Agent = "claude"
				}
			}
			if _, err := p.Run(testContext, changed); err == nil || !strings.Contains(err.Error(), "different dispatch data") {
				t.Fatalf("changed %s did not preserve dispatch conflict: %v", field, err)
			}
		})
	}
}

func TestNativeOutputHelper(t *testing.T) {
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
	raw, e := os.ReadFile(os.Args[index+1])
	if e != nil {
		os.Exit(2)
	}
	var items []any
	if json.Unmarshal(raw, &items) != nil {
		os.Exit(3)
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var r struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &r) != nil {
			os.Exit(4)
		}
		if r.ID == 0 {
			continue
		}
		var result any
		switch r.Method {
		case "initialize":
			result = map[string]any{}
		case "thread/read":
			result = map[string]any{"thread": map[string]any{"id": "11111111-1111-4111-8111-111111111111", "cwd": "/sandbox/fixture"}}
		case "thread/turns/list":
			result = map[string]any{"data": []any{map[string]any{"id": "turn1", "status": "completed"}}, "nextCursor": nil}
		case "thread/items/list":
			result = map[string]any{"data": items, "nextCursor": nil}
		default:
			os.Exit(5)
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{"id": r.ID, "result": result})
	}
	os.Exit(0)
}
func TestInstalledPaddockOutputContract(t *testing.T) {
	binary := os.Getenv("ARNOLD_PADDOCK_TEST_BIN")
	if binary == "" {
		t.Skip("set ARNOLD_PADDOCK_TEST_BIN to exercise the installed CLI")
	}
	if runtime.GOOS == "windows" {
		t.Skip("POSIX native fixture")
	}
	dir := t.TempDir()
	registry := filepath.Join(dir, "paddock")
	os.MkdirAll(filepath.Join(registry, "tasks"), 0700)
	taskID := "0123456789abcdef01234567"
	answer := strings.Repeat("ø🦖", 16000)
	items := []any{}
	for i := 0; i < 19; i++ {
		items = append(items, map[string]any{"turnId": "turn1", "completedAtMs": i + 1, "item": map[string]any{"id": string(rune('a' + i)), "type": "agentMessage", "phase": "commentary", "text": "Progress"}})
	}
	items = append(items, map[string]any{"turnId": "turn1", "completedAtMs": 30, "item": map[string]any{"id": "answer", "type": "agentMessage", "phase": "final_answer", "text": answer}})
	data, _ := json.Marshal(items)
	itemsPath := filepath.Join(dir, "items.json")
	os.WriteFile(itemsPath, data, 0600)
	gateway := filepath.Join(dir, "native")
	script := "#!/bin/sh\nexec " + shellQuote(os.Args[0]) + " -test.run=TestNativeOutputHelper -- " + shellQuote(itemsPath) + " \"$@\"\n"
	os.WriteFile(gateway, []byte(script), 0700)
	record := map[string]any{"version": 1, "id": taskID, "runtime": "openshell", "worktree": "/sandbox/fixture", "status": "running", "session": map[string]any{"agent": "codex", "agent_path": "/fixture/codex", "conversation_id": "11111111-1111-4111-8111-111111111111"}, "openshell": map[string]any{"name": "fixture", "ready": true, "config": map[string]any{"binary": gateway, "gateway": "fixture"}}}
	data, _ = json.Marshal(record)
	os.WriteFile(filepath.Join(registry, "tasks", taskID+".json"), data, 0600)
	t.Setenv("PADDOCK_HOME", registry)
	p := Paddock{Binary: binary}
	first, e := p.Output(testContext, taskID, "")
	if e != nil {
		t.Fatal(e)
	}
	if len(first.Events) != 20 || !first.HasMore || first.Events[19].Part != 1 || first.Events[19].Parts < 2 {
		t.Fatal("unexpected output contract")
	}
	path := filepath.Join(dir, "arnold.sqlite")
	store, e := OpenStore(path)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { store.Close() }()
	chat := &fakeChat{}
	fail := true
	chat.beforeStream = func(o StreamEvent) {
		if fail && len(chat.events) == 21 {
			fail = false
			chat.streamErr = errors.New("simulated Slack disconnect")
		}
	}
	relay := func() error {
		return RelayOutput(testContext, taskID, "C1", "1", p, chat, store, Recipient{TeamID: "T1", UserID: "U1"})
	}
	if e = relay(); e == nil {
		t.Fatal("expected failed delivery")
	}
	if c, _ := store.Cursor(taskID); c != "" {
		t.Fatal("advanced cursor before multipart delivery")
	}
	store.Close()
	store, e = OpenStore(path)
	if e != nil {
		t.Fatal(e)
	}
	chat.beforeStream = nil
	chat.streamErr = nil
	if e = relay(); e != nil {
		t.Fatal(e)
	}
	var delivered strings.Builder
	for _, o := range chat.events {
		if o.Event.Phase == "final_answer" {
			delivered.WriteString(o.Text)
		}
	}
	if delivered.String() != answer {
		t.Fatal("answer lost or duplicated across installed CLI pages")
	}
	count := len(chat.events)
	if e = relay(); e != nil {
		t.Fatal(e)
	}
	if len(chat.events) != count {
		t.Fatal("completed output replayed")
	}
}
