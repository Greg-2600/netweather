package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Greg-2600/netweather/internal/store"
)

// backupCommand writes a consistent copy of the database with VACUUM INTO
// (SCHEMA.md §14). The release image has no shell and no sqlite3 binary, so the
// documented procedure has to be a command on this binary rather than something
// an operator types into the container (THREAT_MODEL.md T43).
func backupCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dest := fs.String("o", "", "destination file for the consistent copy")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if len(fs.Args()) > 0 {
		return fmt.Errorf("backup: unexpected argument %q", fs.Args()[0])
	}
	if strings.TrimSpace(*dest) == "" {
		return errors.New("backup: -o is required")
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

	if err := st.Backup(ctx, *dest); err != nil {
		return err
	}
	info, err := os.Stat(*dest)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "backup written to %s (%d bytes)\n", *dest, info.Size())
	return err
}
