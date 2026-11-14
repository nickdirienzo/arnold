package arnold

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Paddock struct{ Binary string }
type commandError struct {
	stdout    []byte
	stderr    string
	operation string
	cause     error
}

func (e *commandError) Error() string {
	if e.stderr != "" {
		return fmt.Sprintf("Paddock %s failed: %s", e.operation, e.stderr)
	}
	return fmt.Sprintf("Paddock %s failed: %v", e.operation, e.cause)
}
func (e *commandError) Unwrap() error { return e.cause }

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	overflow func()
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		if b.overflow != nil {
			b.overflow()
		}
		return 0, fmt.Errorf("Paddock output exceeds %d bytes", b.limit)
	}
	return b.Buffer.Write(p)
}

type diagnosticBuffer struct{ bytes.Buffer }

func (b *diagnosticBuffer) Write(p []byte) (int, error) {
	n := min(len(p), max(0, 4096-b.Len()))
	_, _ = b.Buffer.Write(p[:n])
	return len(p), nil
}
func (p Paddock) call(ctx context.Context, args []string, out any, limit int) error {
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	binary := p.Binary
	if binary == "" {
		binary = "paddock"
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	allowed := map[string]bool{}
	for _, k := range []string{"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "LANG", "PADDOCK_HOME", "XDG_STATE_HOME", "REX_SERVER"} {
		allowed[k] = true
	}
	cmd.Env = []string{}
	for _, v := range os.Environ() {
		k, _, _ := strings.Cut(v, "=")
		if allowed[k] {
			cmd.Env = append(cmd.Env, v)
		}
	}
	stdout := &limitedBuffer{limit: limit, overflow: cancel}
	cmd.Stdout = stdout
	stderr := &diagnosticBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return &commandError{stdout: stdout.Bytes(), stderr: strings.TrimSpace(stderr.String()), operation: args[0], cause: err}
	}
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		return fmt.Errorf("invalid Paddock JSON response: %w", err)
	}
	return nil
}
func (p Paddock) Check(ctx context.Context, c Config) error {
	var identities map[string]struct {
		Mode     string   `json:"mode"`
		Disabled bool     `json:"disabled"`
		Profiles []string `json:"profiles"`
	}
	if e := p.call(ctx, []string{"identity", "list", "--json"}, &identities, 4<<20); e != nil {
		return e
	}
	var launchers map[string]struct {
		Runtime  string `json:"runtime"`
		ReadOnly bool   `json:"read_only"`
	}
	if e := p.call(ctx, []string{"launcher", "list", "--json"}, &launchers, 4<<20); e != nil {
		return e
	}
	actors := []Actor{}
	for _, a := range c.Users {
		actors = append(actors, a)
	}
	if c.Automation != nil {
		actors = append(actors, *c.Automation)
	}
	for _, a := range actors {
		i, ok := identities[a.Identity]
		profileOK := len(i.Profiles) == 0
		for _, v := range i.Profiles {
			if v == a.Profile {
				profileOK = true
			}
		}
		if !ok || i.Disabled || i.Mode != "app" || !profileOK {
			return fmt.Errorf("Paddock identity %s must be an active GitHub App allowing %s", a.Identity, a.Profile)
		}
		launcher, ok := launchers[a.Profile]
		if !ok || launcher.Runtime != "openshell" || (a.Profile == "investigation" && !launcher.ReadOnly) {
			return fmt.Errorf("Paddock launcher %s must use OpenShell and preserve the configured read-only policy", a.Profile)
		}
	}
	return nil
}
func VerifyTask(t Task, identity, profile, agent string) error {
	launcher := t.Provenance.Launcher
	if launcher == "" {
		launcher = t.Provenance.Profile
	}
	if t.ID == "" || t.Runtime != "openshell" || t.Provenance.Identity != identity || launcher != profile || (agent != "" && t.Session.Agent != agent) {
		return fmt.Errorf("Paddock returned an unexpected runtime, identity, profile or agent; refusing continuation")
	}
	return nil
}
func (p Paddock) Run(ctx context.Context, o RunOptions) (Task, error) {
	var t Task
	idleTTL, e := normalizeIdleTTL(o.IdleTTL)
	if e != nil {
		return t, e
	}
	dir, e := os.MkdirTemp("", "arnold-")
	if e != nil {
		return t, e
	}
	defer os.RemoveAll(dir)
	if e = os.WriteFile(filepath.Join(dir, "prompt"), []byte(o.Prompt), 0600); e != nil {
		return t, e
	}
	b, e := json.Marshal(o.Context)
	if e != nil {
		return t, e
	}
	if e = os.WriteFile(filepath.Join(dir, "context"), b, 0600); e != nil {
		return t, e
	}
	if o.Agent == "" {
		o.Agent = "codex"
	}
	args := []string{"run", "--prompt-file", filepath.Join(dir, "prompt"), "--repo", o.Repo, "--runtime", "openshell", "--agent", o.Agent, "--identity", o.Identity, "--launcher", o.Profile, "--requester", o.Actor, "--requester-source", "arnold-slack", "--external-id", o.ExternalID, "--context-file", filepath.Join(dir, "context"), "--json", "--detach"}
	if idleTTL != "" {
		args = append(args, "--idle-ttl", idleTTL)
	}
	e = p.call(ctx, args, &t, 4<<20)
	// A pre-launcher task has only provenance.profile. Inspect a confirmed
	// dispatch conflict before retrying without the new launcher assertion.
	var ce *commandError
	if errors.As(e, &ce) {
		match := legacyDispatchConflict.FindStringSubmatch(ce.stderr)
		if len(match) == 2 {
			prior, showErr := p.Show(ctx, match[1])
			if showErr == nil && prior.Provenance.Launcher == "" && prior.IdleTTL == idleTTL && VerifyTask(prior, o.Identity, o.Profile, o.Agent) == nil {
				for i, arg := range args {
					if arg == "--launcher" {
						args = append(args[:i:i], args[i+2:]...)
						break
					}
				}
				e = p.call(ctx, args, &t, 4<<20)
			}
		}
	}
	if e != nil {
		return t, e
	}
	if t.IdleTTL != idleTTL {
		return t, fmt.Errorf("Paddock returned an unexpected idle TTL; refusing dispatch binding")
	}
	return t, VerifyTask(t, o.Identity, o.Profile, o.Agent)
}

var legacyDispatchConflict = regexp.MustCompile(`^paddock: external ID already belongs to task ([a-f0-9]{24}) with different dispatch data$`)

func (p Paddock) Show(ctx context.Context, id string) (Task, error) {
	var t Task
	e := p.call(ctx, []string{"show", id, "--json"}, &t, 4<<20)
	return t, e
}
func (p Paddock) Output(ctx context.Context, id, after string) (OutputPage, error) {
	var page OutputPage
	args := []string{"output", id}
	if after != "" {
		args = append(args, "--after", after)
	}
	args = append(args, "--limit", "20", "--json")
	e := p.call(ctx, args, &page, 16<<20)
	return page, e
}
func (p Paddock) Message(ctx context.Context, id, prompt, external string) (MessageReceipt, error) {
	var r MessageReceipt
	e := p.call(ctx, []string{"message", id, prompt, "--external-id", external, "--json"}, &r, 4<<20)
	if e != nil {
		var ce *commandError
		if errors.As(e, &ce) && json.Unmarshal(ce.stdout, &r) == nil {
			switch r.Status {
			case "pending", "uncertain", "claimed":
				return r, nil
			}
		}
	}
	return r, e
}
