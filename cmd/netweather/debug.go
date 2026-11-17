package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// debugCommand is the CLI half of LOGGING.md §5. It talks to the same HTTP
// endpoints a script would, so there is one code path and no privileged local
// back door.
func debugCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("debug: expected enable, status, or disable")
	}

	switch args[0] {
	case "enable":
		return debugEnable(args[1:], out)
	case "status":
		return debugStatus(args[1:], out)
	case "disable":
		return debugDisable(args[1:], out)
	default:
		return fmt.Errorf("debug: unknown subcommand %q (want enable, status, disable)", args[0])
	}
}

// debugClient builds the request base and credential from the environment. The
// token is never accepted on the command line: it would land in shell history.
type debugClient struct {
	base  string
	token string
	http  *http.Client
}

func newDebugClient() (*debugClient, error) {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("NETWEATHER_API_URL")), "/")
	if base == "" {
		base = "http://127.0.0.1:2600"
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("debug: NETWEATHER_API_URL %q is not an http(s) URL", base)
	}

	token := strings.TrimSpace(os.Getenv("NETWEATHER_UI_TOKEN"))
	if token == "" {
		if path := strings.TrimSpace(os.Getenv("NETWEATHER_UI_TOKEN_FILE")); path != "" {
			raw, err := os.ReadFile(path) // #nosec G304,G703 -- operator-supplied token file.
			if err != nil {
				return nil, fmt.Errorf("debug: read token file: %w", err)
			}
			token = strings.TrimSpace(string(raw))
		}
	}
	if token == "" {
		return nil, errors.New("debug: set NETWEATHER_UI_TOKEN or NETWEATHER_UI_TOKEN_FILE")
	}

	return &debugClient{
		base:  base,
		token: token,
		http:  &http.Client{Timeout: 10 * time.Second},
	}, nil
}

func (c *debugClient) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

type debugScope struct {
	ID         string    `json:"id"`
	Target     string    `json:"target"`
	Vantage    string    `json:"vantage"`
	Component  string    `json:"component"`
	ErrorClass string    `json:"error_class"`
	Level      string    `json:"level"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func debugEnable(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("debug enable", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	target := fs.String("target", "", "registrable host to scope")
	vantage := fs.String("vantage", "", "vantage to scope")
	component := fs.String("component", "", "component to scope")
	errorClass := fs.String("error-class", "", "error class to scope")
	level := fs.String("level", "debug", "debug or trace")
	forDur := fs.Duration("for", 0, "duration, max 1h (required)")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("debug enable: %w", err)
	}
	if *forDur <= 0 {
		return errors.New("debug enable: --for is required (for example --for 10m)")
	}

	payload, err := json.Marshal(map[string]string{
		"target":      *target,
		"vantage":     *vantage,
		"component":   *component,
		"error_class": *errorClass,
		"level":       *level,
		"ttl":         forDur.String(),
	})
	if err != nil {
		return err
	}

	client, err := newDebugClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := client.do(ctx, http.MethodPost, "/api/v1/debug/scopes", payload)
	if err != nil {
		return fmt.Errorf("debug enable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("debug enable: %s: %s", resp.Status, apiErrorText(body))
	}

	var created struct {
		Data debugScope `json:"data"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		return fmt.Errorf("debug enable: %w", err)
	}

	_, err = fmt.Fprintf(out, "scope %s enabled until %s\n",
		created.Data.ID, created.Data.ExpiresAt.UTC().Format(time.RFC3339))
	return err
}

func debugStatus(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("debug status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "emit the API envelope")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("debug status: %w", err)
	}
	if len(fs.Args()) > 0 {
		return fmt.Errorf("debug status: unexpected argument %q", fs.Args()[0])
	}

	client, err := newDebugClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := client.do(ctx, http.MethodGet, "/api/v1/debug/scopes", nil)
	if err != nil {
		return fmt.Errorf("debug status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("debug status: %s: %s", resp.Status, apiErrorText(body))
	}

	var env struct {
		Data []debugScope `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("debug status: %w", err)
	}

	if *asJSON {
		if env.Data == nil {
			env.Data = []debugScope{}
		}
		return writeJSON(out, env.Data)
	}

	if len(env.Data) == 0 {
		_, err := fmt.Fprintln(out, "no active debug scopes")
		return err
	}
	for _, sc := range env.Data {
		selectors := compactSelectors(sc)
		_, err := fmt.Fprintf(out, "%s  %-5s  %s  expires %s\n",
			sc.ID, sc.Level, selectors, sc.ExpiresAt.UTC().Format(time.RFC3339))
		if err != nil {
			return err
		}
	}
	return nil
}

func debugDisable(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("debug disable", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	all := fs.Bool("all", false, "disable every scope")
	id := fs.String("id", "", "scope id to disable")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("debug disable: %w", err)
	}
	if *all && *id != "" {
		return errors.New("debug disable: choose --all or --id, not both")
	}
	if !*all && *id == "" {
		return errors.New("debug disable: --all or --id is required")
	}

	client, err := newDebugClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if *all {
		resp, err := client.do(ctx, http.MethodGet, "/api/v1/debug/scopes", nil)
		if err != nil {
			return fmt.Errorf("debug disable: %w", err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("debug disable: %s: %s", resp.Status, apiErrorText(body))
		}

		var env struct {
			Data []debugScope `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return fmt.Errorf("debug disable: %w", err)
		}
		for _, sc := range env.Data {
			if err := removeScope(ctx, client, sc.ID); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(out, "disabled %d scope(s)\n", len(env.Data))
		return err
	}

	if err := removeScope(ctx, client, *id); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "scope %s disabled\n", *id)
	return err
}

func removeScope(ctx context.Context, client *debugClient, id string) error {
	resp, err := client.do(ctx, http.MethodDelete, "/api/v1/debug/scopes/"+url.PathEscape(id), nil)
	if err != nil {
		return fmt.Errorf("debug disable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("debug disable %s: %s: %s", id, resp.Status, apiErrorText(body))
	}
	return nil
}

func compactSelectors(sc debugScope) string {
	parts := make([]string, 0, 4)
	if sc.Target != "" {
		parts = append(parts, "target="+sc.Target)
	}
	if sc.Vantage != "" {
		parts = append(parts, "vantage="+sc.Vantage)
	}
	if sc.Component != "" {
		parts = append(parts, "component="+sc.Component)
	}
	if sc.ErrorClass != "" {
		parts = append(parts, "error_class="+sc.ErrorClass)
	}
	return strings.Join(parts, ",")
}

// apiErrorText pulls the message out of the API envelope without hiding an
// unexpected body.
func apiErrorText(body []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		return env.Error.Message
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "no response body"
	}
	if len(text) > 200 {
		text = text[:200]
	}
	return text
}
