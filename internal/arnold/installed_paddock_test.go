package arnold

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

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
