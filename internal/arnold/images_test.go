package arnold

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const imagePNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aJ1sAAAAASUVORK5CYII="

type imageTransport func(*http.Request) (*http.Response, error)

func (f imageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func pngBytes() []byte                                                     { data, _ := base64.StdEncoding.DecodeString(imagePNG); return data }

func TestSlackImageDownloadsKeepCredentialsOnExpectedHost(t *testing.T) {
	calls := 0
	s := &Slack{BotToken: "bot-secret", HTTP: &http.Client{Transport: imageTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "files.slack.com" || r.Header.Get("Authorization") != "Bearer bot-secret" {
			t.Fatal("unexpected authenticated destination")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(pngBytes()))}, nil
	})}}
	file := SlackFile{ID: "F1", MIMEType: "image/png", URLPrivate: "https://files.slack.com/files-pri/T-F/shot.png"}
	image, err := s.DownloadImage(testContext, file)
	if err != nil || image.FileID != "F1" || !bytes.Equal(image.Data, pngBytes()) {
		t.Fatal(image, err)
	}
	for _, url := range []string{"https://files.slack.com.evil.test/shot", "http://files.slack.com/shot", "https://files.slack.com:443/shot", "https://user@files.slack.com/shot", "https://evil.test/shot"} {
		file.URLPrivate = url
		if _, err := s.DownloadImage(testContext, file); err == nil {
			t.Fatalf("accepted %s", url)
		}
	}
	file.URLPrivate = "https://files.slack.com/shot"
	file.External = true
	if _, err := s.DownloadImage(testContext, file); err == nil {
		t.Fatal("accepted remote file")
	}
	if calls != 1 {
		t.Fatal("credentials sent for rejected file")
	}
	file.External = false
	s.HTTP.Transport = imageTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://evil.test/steal"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if _, err := s.DownloadImage(testContext, file); err == nil || calls != 2 {
		t.Fatal("followed a redirect or concealed failure", err)
	}
}

func TestSlackImagesValidateBytesAndResolveMissingMetadata(t *testing.T) {
	f := newSlackFixture(t)
	f.hook = func(method string, args map[string]any) (any, int) {
		if method != "files.info" || args["file"] != "F1" {
			t.Fatal(method, args)
		}
		return map[string]any{"ok": true, "file": SlackFile{ID: "F1", MIMEType: "image/png", URLPrivate: "https://files.slack.com/shot"}}, 200
	}
	transport := f.s.HTTP.Transport
	data := pngBytes()
	f.s.HTTP.Transport = imageTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "files.slack.com" {
			return transport.RoundTrip(r)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})
	if _, err := f.s.DownloadImage(testContext, SlackFile{ID: "F1"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{[]byte("<html>sign in</html>"), bytes.Repeat([]byte("x"), maxImageBytes+1)} {
		data = bad
		if _, err := f.s.DownloadImage(testContext, SlackFile{ID: "F1"}); err == nil {
			t.Fatal("accepted non-image or oversized response")
		}
	}
}

func TestSlackThreadCollectsImageOnlyMessagesBeforeRequest(t *testing.T) {
	f := newSlackFixture(t)
	f.hook = func(string, map[string]any) (any, int) {
		return map[string]any{"ok": true, "messages": []any{
			map[string]any{"ts": "1", "user": "Ben", "files": []SlackFile{{ID: "F1"}}},
			map[string]any{"ts": "2", "files": []SlackFile{{ID: "current"}}},
			map[string]any{"ts": "3", "files": []SlackFile{{ID: "future"}}},
		}}, 200
	}
	r, err := f.s.ContextWithFiles(testContext, "C1", "1", "2")
	if err != nil || len(r.Files) != 2 || r.Files[0].ID != "F1" || r.Files[1].ID != "current" || !strings.Contains(r.Text, "Ben") {
		t.Fatal(r, err)
	}
}

type imageChatFixture struct {
	*fakeChat
	files       []SlackFile
	downloads   []string
	downloadErr error
}

func (c *imageChatFixture) ContextWithFiles(context.Context, string, string, string) (ThreadContext, error) {
	return ThreadContext{Text: "Ben's earlier description", Files: c.files}, c.contextErr
}
func (c *imageChatFixture) DownloadImage(_ context.Context, file SlackFile) (InputImage, error) {
	c.downloads = append(c.downloads, file.ID)
	return InputImage{FileID: file.ID, Name: Hash(string(pngBytes())) + ".png", Data: pngBytes()}, c.downloadErr
}

type imagePaddockFixture struct {
	*fakePaddock
	images [][]InputImage
}

func (p *imagePaddockFixture) MessageWithImages(ctx context.Context, task, body, external string, images []InputImage) (MessageReceipt, error) {
	p.images = append(p.images, images)
	return p.fakePaddock.Message(ctx, task, body, external)
}

func TestImageRequestsFreezeAcrossRestartAndDeduplicatePerTask(t *testing.T) {
	f := newFixture(t)
	chat := &imageChatFixture{fakeChat: f.chat, files: []SlackFile{{ID: "historical", MIMEType: "image/png"}}}
	p := &imagePaddockFixture{fakePaddock: f.p}
	f.a.Slack, f.a.Paddock = chat, p
	e := incoming("2")
	e.Event.ThreadTS = "1"
	e.Event.Files = []SlackFile{{ID: "current", MIMEType: "image/png"}, {ID: "historical", MIMEType: "image/png"}}
	f.receive(e)
	f.drain()
	if len(f.p.runs) != 1 || len(f.p.runs[0].Images) != 2 {
		t.Fatal("initial images not supplied", f.p.runs)
	}
	if delivered, err := f.a.Store.ImageDelivered("task1", "historical"); err != nil || !delivered {
		t.Fatal("image delivery was not recorded", err)
	}
	chat.files = append(chat.files, SlackFile{ID: "later", MIMEType: "image/png"})
	f.p.messageStatus = "pending"
	e = incoming("3")
	e.Event.ThreadTS = "1"
	f.receive(e)
	f.drain()
	if len(p.images) != 1 || len(p.images[0]) != 1 || p.images[0][0].FileID != "later" {
		t.Fatal("missed new thread image or resent old images", p.images)
	}
	before := len(chat.downloads)
	f.restart()
	f.a.Slack, f.a.Paddock = chat, p
	chat.downloadErr = errors.New("source now unavailable")
	f.p.messageStatus = "delivered"
	f.drain()
	if len(chat.downloads) != before || len(p.images) != 2 || !bytes.Equal(p.images[1][0].Data, pngBytes()) {
		t.Fatal("retry did not use frozen bytes")
	}
	if delivered, err := f.a.Store.ImageDelivered("task1", "later"); err != nil || !delivered {
		t.Fatal("follow-up image receipt missing", err)
	}
	rows, _ := f.a.Store.Pending()
	if len(rows) != 0 {
		t.Fatal("delivered image bytes retained in pending payload")
	}
	// Each participant owns a distinct native task and needs the same evidence.
	e = incoming("4")
	e.Event.User, e.Event.ThreadTS = "U2", "1"
	chat.downloadErr = nil
	f.receive(e)
	f.drain()
	if len(f.p.runs) != 2 || len(f.p.runs[1].Images) != 2 {
		t.Fatal("another participant did not receive thread images")
	}
}

func TestImageFailurePreventsTextOnlyDispatch(t *testing.T) {
	f := newFixture(t)
	chat := &imageChatFixture{fakeChat: f.chat, files: []SlackFile{{ID: "F1", MIMEType: "image/png"}}, downloadErr: errors.New("missing_scope")}
	f.a.Slack = chat
	f.receive(incoming("1"))
	f.drain()
	if len(f.p.runs) != 0 {
		t.Fatal("dispatched without required evidence")
	}
	rows, _ := f.a.Store.Pending()
	if len(rows) != 1 || rows[0].Payload.Prepared != nil {
		t.Fatal("failed attachment was frozen as success")
	}
}

func TestImageManifestUsesPrivateTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	manifest, err := writeImageManifest(dir, []InputImage{{Name: "../../escape", Data: pngBytes()}})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(manifest)
	var paths []string
	if json.Unmarshal(data, &paths) != nil || len(paths) != 1 || filepath.Dir(paths[0]) != dir {
		t.Fatal("unsafe image path")
	}
	info, _ := os.Stat(paths[0])
	if info.Mode().Perm() != 0600 {
		t.Fatal("image permissions", info.Mode())
	}
}
