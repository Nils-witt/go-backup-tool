package ctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"nilswitt.dev/go-backup-tool/internal/backup/status"
)

// timeLayout is how status prints timestamps, in the local time zone.
const timeLayout = "2006-01-02 15:04:05"

// healthResponse mirrors webui's GET /api/health body.
type healthResponse struct {
	Status   string `json:"status"`
	Version  string `json:"version"`
	Database string `json:"database"`
}

// runHealth checks GET /api/health: exit 0 when the instance reports
// itself healthy, exitError otherwise (unreachable, or a 503 because its
// state db is down).
func runHealth(ctx context.Context, c *client, opts options, stdout io.Writer) (int, error) {
	code, body, err := c.get(ctx, "/api/health")
	if err != nil {
		return exitError, err
	}

	var health healthResponse
	if err := json.Unmarshal(body, &health); err != nil {
		return exitError, fmt.Errorf("GET /api/health: %d, decoding response: %w", code, err)
	}

	if opts.json {
		_, err = stdout.Write(body)
	} else {
		_, err = fmt.Fprintf(stdout, "%s (version %s, database %s)\n", sanitize(health.Status), sanitize(health.Version), sanitize(health.Database))
	}

	if err != nil {
		return exitError, fmt.Errorf("writing output: %w", err)
	}

	if code != http.StatusOK || health.Status != "ok" {
		return exitError, nil
	}

	return exitOK, nil
}

// statusReport is status's -json output.
type statusReport struct {
	Jobs      []status.JobSnapshot      `json:"jobs"`
	Receivers []status.ReceiverSnapshot `json:"receivers"`
}

// runStatus prints every job (with its targets) and receiver, from GET
// /api/status and /api/receivers. It exits exitAttention when any of them
// needs attention (see needsAttention).
func runStatus(ctx context.Context, c *client, opts options, stdout io.Writer) (int, error) {
	report := statusReport{}

	if err := c.getJSON(ctx, "/api/status", &report.Jobs); err != nil {
		return exitError, err
	}

	if err := c.getJSON(ctx, "/api/receivers", &report.Receivers); err != nil {
		return exitError, err
	}

	// A JSON null decodes to nil; -json prints [] for "none" either way.
	if report.Jobs == nil {
		report.Jobs = []status.JobSnapshot{}
	}

	if report.Receivers == nil {
		report.Receivers = []status.ReceiverSnapshot{}
	}

	var err error
	if opts.json {
		err = writeStatusJSON(stdout, report)
	} else {
		err = writeStatusText(stdout, report)
	}

	if err != nil {
		return exitError, fmt.Errorf("writing output: %w", err)
	}

	if needsAttention(report) {
		return exitAttention, nil
	}

	return exitOK, nil
}

// needsAttention reports whether a job failed (in full or in part), a
// target failed, or a receiver failed or went stale.
func needsAttention(report statusReport) bool {
	for _, job := range report.Jobs {
		if job.State == status.StateFailed || job.State == status.StateIncomplete {
			return true
		}

		for _, target := range job.Targets {
			if target.State == status.StateFailed {
				return true
			}
		}
	}

	for _, recv := range report.Receivers {
		if recv.State == status.StateFailed || recv.Stale {
			return true
		}
	}

	return false
}

func writeStatusJSON(w io.Writer, report statusReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(report)
}

// writeStatusText prints report as two aligned tables: jobs, each followed
// by its targets, then receivers. Either is left out when empty.
func writeStatusText(w io.Writer, report statusReport) error {
	if len(report.Jobs) == 0 && len(report.Receivers) == 0 {
		_, err := fmt.Fprintln(w, "no jobs or receivers")
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	if len(report.Jobs) > 0 {
		writeJobRows(tw, report.Jobs)
	}

	if len(report.Jobs) > 0 && len(report.Receivers) > 0 {
		_, _ = fmt.Fprintln(tw)
	}

	if len(report.Receivers) > 0 {
		writeReceiverRows(tw, report.Receivers)
	}

	return tw.Flush()
}

// writeJobRows writes the jobs table. Errors writing to tw surface from
// its Flush.
func writeJobRows(tw io.Writer, jobs []status.JobSnapshot) {
	writeRow(tw, "JOB", "STATE", "LAST RUN", "NEXT RUN", "DURATION", "SIZE", "ERROR")

	for _, job := range jobs {
		writeRow(tw, job.Name, string(job.State), formatTime(job.LastStart), formatTime(job.NextRun), orDash(job.Duration), orDash(job.Size), job.Error)

		for _, target := range job.Targets {
			writeRow(tw, "  "+target.Server+"/"+target.Bucket, string(target.State), "", "", "", "", target.Error)
		}
	}
}

// writeReceiverRows writes the receivers table. Errors writing to tw
// surface from its Flush.
func writeReceiverRows(tw io.Writer, receivers []status.ReceiverSnapshot) {
	writeRow(tw, "RECEIVER", "KIND", "STATE", "LAST SEEN", "STALE", "ERROR")

	for _, recv := range receivers {
		stale := "-"
		if recv.StaleAfter != "" {
			stale = "no"
			if recv.Stale {
				stale = "yes"
			}
		}

		writeRow(tw, recv.ID, recv.Kind, string(recv.State), formatTime(recv.LastSeen), stale, recv.Error)
	}
}

// writeRow writes cells as one tab-separated line, each sanitized so a
// value can't break the table or inject terminal escapes.
func writeRow(w io.Writer, cells ...string) {
	for i, cell := range cells {
		cells[i] = sanitize(cell)
	}

	_, _ = fmt.Fprintln(w, strings.Join(cells, "\t"))
}

// sanitize replaces control characters (newlines, tabs, terminal escapes)
// in s, which comes from the instance, with spaces.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}

		return r
	}, s)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}

	return t.Local().Format(timeLayout)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}

	return s
}
