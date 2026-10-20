package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"go.mau.fi/zeroconfig"
)

func TestScrubLine(t *testing.T) {
	line := `{"level":"debug","method":"PUT","req_body":{"msgtype":"m.text","body":"secret {nested} \"quote\""},"url":"http://hs/_matrix/client/v3/rooms/!a/send/m.room.message/1","message":"Request completed"}` + "\n"
	got := string(scrubLine([]byte(line)))
	if strings.Contains(got, "secret") {
		t.Fatalf("body not scrubbed: %s", got)
	}
	want := `{"level":"debug","method":"PUT","req_body":"<message content omitted>","url":"http://hs/_matrix/client/v3/rooms/!a/send/m.room.message/1","message":"Request completed"}` + "\n"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	other := `{"req_body":{"username":"xmppbot"},"url":"http://hs/_matrix/client/v3/register","message":"Request completed"}`
	if string(scrubLine([]byte(other))) != other {
		t.Error("non-send requests must be left alone")
	}
}

func TestScrubLogging(t *testing.T) {
	var buf bytes.Buffer
	zeroconfig.Stdout = &buf
	debug := zerolog.DebugLevel
	cfg := &zeroconfig.Config{MinLevel: &debug, Writers: []zeroconfig.WriterConfig{{Type: zeroconfig.WriterTypeStdout, Format: zeroconfig.LogFormatPretty}}}
	scrubLogging(cfg)
	log, err := cfg.Compile()
	if err != nil {
		t.Fatal(err)
	}
	log.Debug().RawJSON("req_body", []byte(`{"body":"secret"}`)).Str("url", "/rooms/!x/send/m.room.message/1").Msg("Request completed")
	log.Trace().Msg("filtered by level")
	out := buf.String()
	if strings.Contains(out, "secret") || !strings.Contains(out, "Request completed") || !strings.Contains(out, "omitted") {
		t.Errorf("unexpected output: %q", out)
	}
	if strings.Contains(out, "filtered") {
		t.Error("level filtering broken")
	}
}
