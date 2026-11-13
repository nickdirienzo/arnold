package main

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

func TestVersionAndHelpNeedNoCredentials(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}, {"--help"}} {
		var out bytes.Buffer
		if e := run(args, &out); e != nil {
			t.Fatal(e)
		}
		if !strings.Contains(out.String(), "arnold") {
			t.Fatal("missing help/version")
		}
	}
}
func TestLaunchdRendersStandaloneBinaryAndEscapesPaths(t *testing.T) {
	var out bytes.Buffer
	e := renderLaunchd([]string{"--directory", "/tmp/A & B", "--binary", "/tmp/arnold"}, &out)
	if e != nil {
		t.Fatal(e)
	}
	decoder := xml.NewDecoder(bytes.NewReader(out.Bytes()))
	for {
		_, e = decoder.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
	}
	text := out.String()
	for _, required := range []string{"/tmp/arnold", "A &amp; B", "--env-file", "dev.arnold.slack", "KeepAlive"} {
		if !strings.Contains(text, required) {
			t.Fatal("missing", required)
		}
	}
	for _, forbidden := range []string{"node", "src/main.js", "SLACK_BOT_TOKEN"} {
		if strings.Contains(text, forbidden) {
			t.Fatal("nonbinary runtime or embedded credentials")
		}
	}
}
