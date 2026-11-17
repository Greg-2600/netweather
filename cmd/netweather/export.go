package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Greg-2600/netweather/internal/store"
)

var exportHeader = []string{
	"target_id", "host", "vantage", "ts", "state", "stage",
	"error_class", "http_status", "latency_ms", "confidence", "detail",
}

// exportCommand streams observations in a time range. stdout is data; diagnostics
// belong on stderr, and this command emits none on success.
func exportCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	fromArg := fs.String("from", "", "RFC 3339 start, inclusive (default 24h ago)")
	toArg := fs.String("to", "", "RFC 3339 end, exclusive (default now)")
	format := fs.String("format", "csv", "csv or jsonl")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("export: %w", err)
	}

	now := time.Now().UTC()
	from := now.Add(-24 * time.Hour)
	to := now
	if *fromArg != "" {
		parsed, err := time.Parse(time.RFC3339, *fromArg)
		if err != nil {
			return fmt.Errorf("export: --from: %w", err)
		}
		from = parsed
	}
	if *toArg != "" {
		parsed, err := time.Parse(time.RFC3339, *toArg)
		if err != nil {
			return fmt.Errorf("export: --to: %w", err)
		}
		to = parsed
	}
	if !from.Before(to) {
		return errors.New("export: --from must be before --to")
	}
	if *format != "csv" && *format != "jsonl" {
		return fmt.Errorf("export: unknown format %q (csv or jsonl)", *format)
	}

	dataDir := os.Getenv("NETWEATHER_DATA")
	if dataDir == "" {
		dataDir = "./data"
	}

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dataDir, "netweather.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	switch *format {
	case "csv":
		return exportCSV(ctx, st, from, to, out)
	case "jsonl":
		return exportJSONL(ctx, st, from, to, out)
	}
	return nil
}

func exportCSV(ctx context.Context, st *store.Store, from, to time.Time, out io.Writer) error {
	w := csv.NewWriter(out)
	if err := w.Write(exportHeader); err != nil {
		return fmt.Errorf("export: header: %w", err)
	}

	err := st.ExportProbes(ctx, from, to, func(r store.ExportRow) error {
		return w.Write([]string{
			r.TargetID, r.Host, r.VantageID, r.TS.Format(time.RFC3339),
			string(r.State), r.Stage, r.ErrorClass,
			strconv.Itoa(r.HTTPStatus), strconv.Itoa(r.LatencyMS),
			r.Confidence, r.Detail,
		})
	})
	w.Flush()
	if err != nil {
		return err
	}
	if err := w.Error(); err != nil {
		return fmt.Errorf("export: write: %w", err)
	}
	return nil
}

func exportJSONL(ctx context.Context, st *store.Store, from, to time.Time, out io.Writer) error {
	enc := json.NewEncoder(out)
	return st.ExportProbes(ctx, from, to, func(r store.ExportRow) error {
		return enc.Encode(map[string]any{
			"target_id":   r.TargetID,
			"host":        r.Host,
			"vantage":     r.VantageID,
			"ts":          r.TS.Format(time.RFC3339),
			"state":       string(r.State),
			"stage":       r.Stage,
			"error_class": r.ErrorClass,
			"http_status": r.HTTPStatus,
			"latency_ms":  r.LatencyMS,
			"confidence":  r.Confidence,
			"detail":      r.Detail,
		})
	})
}
