package main

import (
	"encoding/json"
	"fmt"
	"io"
)

// cliEnvelope is the same shape the HTTP API serves: `data` on success and
// `error` on failure. A script can therefore parse either surface with one
// decoder instead of learning a second schema (API.md §9).
type cliEnvelope struct {
	Data  any       `json:"data,omitempty"`
	Error *cliError `json:"error,omitempty"`
}

type cliError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeJSON emits one envelope and a trailing newline. stdout is data, so this
// is the only writer the JSON path touches.
func writeJSON(out io.Writer, data any) error {
	return writeEnvelope(out, cliEnvelope{Data: data})
}

// writeJSONError emits a failure envelope. The command still returns a non-nil
// error so the exit code and the human-readable stderr line are unchanged.
func writeJSONError(out io.Writer, code string, err error) error {
	message := "unknown error"
	if err != nil {
		message = err.Error()
	}
	return writeEnvelope(out, cliEnvelope{Error: &cliError{Code: code, Message: message}})
}

func writeEnvelope(out io.Writer, env cliEnvelope) error {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(env); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	return nil
}
