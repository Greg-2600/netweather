package doctor_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/doctor"
)

func TestRunClassifiesChecks(t *testing.T) {
	t.Parallel()

	runner := &doctor.Runner{
		Now: func() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC) },
		Checks: []doctor.Check{
			{Name: "disk", Fatal: true, Run: func(context.Context) (string, error) {
				return "64 KiB written", nil
			}},
			{Name: "icmp", Run: func(context.Context) (string, error) {
				return "", errors.New("CAP_NET_RAW is required")
			}},
			{Name: "resolver", Fatal: true, Run: func(context.Context) (string, error) {
				return "", errors.New("no resolver")
			}},
		},
	}

	results := runner.Run(context.Background())
	if len(results) != 3 {
		t.Fatalf("results = %d", len(results))
	}
	if results[0].Status != doctor.OK || results[0].Detail != "64 KiB written" {
		t.Fatalf("first = %+v", results[0])
	}
	if results[1].Status != doctor.WARN {
		t.Fatalf("non-fatal failure = %+v, want warn", results[1])
	}
	if results[1].Detail != "CAP_NET_RAW is required" {
		t.Fatalf("warn detail = %q", results[1].Detail)
	}
	if results[2].Status != doctor.FAIL {
		t.Fatalf("fatal failure = %+v, want fail", results[2])
	}
	if !doctor.Failed(results) {
		t.Fatal("Failed did not report the fatal check")
	}
}

func TestFailedIgnoresWarnings(t *testing.T) {
	t.Parallel()

	results := []doctor.Result{
		{Name: "icmp", Status: doctor.WARN},
		{Name: "disk", Status: doctor.OK},
	}
	if doctor.Failed(results) {
		t.Fatal("a warning was treated as a failure")
	}
}

func TestWriteRendersEveryCheck(t *testing.T) {
	t.Parallel()

	results := []doctor.Result{
		{Name: "disk", Status: doctor.OK, Detail: "64 KiB written"},
		{Name: "trace", Status: doctor.WARN, Detail: ""},
		{Name: "resolver", Status: doctor.FAIL, Detail: "no resolver"},
	}

	var out bytes.Buffer
	if err := doctor.Write(&out, results); err != nil {
		t.Fatalf("Write: %v", err)
	}

	body := out.String()
	for _, want := range []string{"CHECK", "STATUS", "DETAIL", "disk", "ok", "64 KiB written", "trace", "warn", "-", "resolver", "fail", "no resolver"} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q:\n%s", want, body)
		}
	}
}
