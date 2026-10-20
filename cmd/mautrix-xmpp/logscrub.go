package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/rs/zerolog"
	"go.mau.fi/zeroconfig"
)

// mautrix-go logs the JSON body of every Matrix API request at debug level,
// and bbctl's generated config logs at debug level. For message sends that
// body is the bridged message. scrubLogging wraps every configured log writer
// so those bodies are replaced, unless message content logging is enabled.

var redactedBody = []byte(`"<message content omitted>"`)

// scrubLogging rewrites the logging config in place so each writer goes
// through a scrubbing wrapper.
func scrubLogging(cfg *zeroconfig.Config) {
	for i, orig := range cfg.Writers {
		typ := zeroconfig.WriterType(fmt.Sprintf("mautrix-xmpp-scrubbed-%d", i))
		inner := orig
		zeroconfig.RegisterWriter(typ, func(*zeroconfig.WriterConfig) (io.Writer, error) {
			w, err := inner.Compile()
			if err != nil {
				return nil, err
			}
			return scrubWriter{inner: w}, nil
		})
		// The wrapper receives raw JSON; formatting and level filtering
		// happen in the inner writer.
		cfg.Writers[i] = zeroconfig.WriterConfig{Type: typ, Format: zeroconfig.LogFormatJSON}
	}
}

type scrubWriter struct {
	inner io.Writer
}

func (w scrubWriter) Write(p []byte) (int, error) {
	_, err := w.inner.Write(scrubLine(p))
	return len(p), err
}

func (w scrubWriter) WriteLevel(level zerolog.Level, p []byte) (int, error) {
	line := scrubLine(p)
	var err error
	if lw, ok := w.inner.(zerolog.LevelWriter); ok {
		_, err = lw.WriteLevel(level, line)
	} else {
		_, err = w.inner.Write(line)
	}
	return len(p), err
}

// scrubLine replaces the req_body field of request log lines for event sends
// (PUT /rooms/{roomID}/send/{type}/{txnID}). Other lines are returned as is.
func scrubLine(p []byte) []byte {
	const key = `"req_body":`
	idx := bytes.Index(p, []byte(key))
	if idx < 0 || !bytes.Contains(p, []byte(`/send/`)) {
		return p
	}
	start := idx + len(key)
	dec := json.NewDecoder(bytes.NewReader(p[start:]))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return p
	}
	end := start + int(dec.InputOffset())
	out := make([]byte, 0, len(p))
	out = append(out, p[:start]...)
	out = append(out, redactedBody...)
	return append(out, p[end:]...)
}
