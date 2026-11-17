package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// targetDisabler adapts the runtime disable closure to api.TargetControl.
type targetDisabler func(context.Context, string) (bool, error)

func (f targetDisabler) DisableTarget(ctx context.Context, id string) (bool, error) {
	return f(ctx, id)
}

type targetRef struct {
	ID   string `json:"id"`
	Host string `json:"host"`
}

// disableCommand stops probing one target without a restart. It accepts the
// target id or its host, resolves the host through the API so an operator does
// not have to know the derived id, and then calls the runtime action. This is
// the abuse-response path from ACCEPTABLE_USE.md §7.
func disableCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("disable", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "emit the API envelope")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("disable: %w", err)
	}
	if fs.NArg() != 1 {
		return errors.New("disable: expected one target id or host")
	}
	want := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(fs.Arg(0)), "."))
	if want == "" {
		return errors.New("disable: target must not be empty")
	}

	client, err := newDebugClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	body, err := client.get(ctx, "/api/v1/targets")
	if err != nil {
		return fmt.Errorf("disable: %w", err)
	}
	var env struct {
		Data []targetRef `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("disable: target list: %w", err)
	}

	var found *targetRef
	for i := range env.Data {
		t := &env.Data[i]
		if t.ID == want || strings.EqualFold(t.Host, want) {
			found = t
			break
		}
	}
	if found == nil {
		return fmt.Errorf("disable: no target matches %q", want)
	}

	resp, err := client.do(ctx, http.MethodPost,
		"/api/v1/targets/"+url.PathEscape(found.ID)+"/disable", nil)
	if err != nil {
		return fmt.Errorf("disable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("disable: %s: %s", resp.Status, strings.TrimSpace(string(reply)))
	}

	if *asJSON {
		return writeJSON(out, map[string]string{
			"target": found.ID, "host": found.Host, "state": "disabled",
		})
	}
	_, _ = fmt.Fprintf(out, "disabled %s (%s); it stops on the next tick\n", found.Host, found.ID)
	return nil
}
