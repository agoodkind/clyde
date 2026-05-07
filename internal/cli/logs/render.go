package logs

import (
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func writeInventoryTable(writer io.Writer, currentInventory inventory) error {
	rows := make([][]string, 0, len(currentInventory.Categories))
	for _, summary := range currentInventory.Categories {
		rows = append(rows, []string{
			string(summary.Category),
			strconv.Itoa(summary.Count),
			formatBytes(summary.TotalBytes),
			formatTime(summary.LatestModified),
			displayPath(summary.RepresentativePath),
			formatLargestFiles(summary.LargestFiles),
		})
	}
	headers := []string{"Category", "Count", "Total", "Latest modified", "Representative path", "Largest files"}
	widths := columnWidths(headers, rows)
	if _, err := fmt.Fprintf(writer, "State root: %s\n", currentInventory.StateRoot); err != nil {
		return writeTableError("write state root line", err)
	}
	if _, err := io.WriteString(writer, "\n"); err != nil {
		return writeTableError("write state root spacer", err)
	}
	if err := writeTableRow(writer, headers, widths); err != nil {
		return writeTableError("write header row", err)
	}
	if err := writeTableSeparator(writer, widths); err != nil {
		return writeTableError("write separator row", err)
	}
	for _, row := range rows {
		if err := writeTableRow(writer, row, widths); err != nil {
			return writeTableError("write category row", err)
		}
	}
	return nil
}

func columnWidths(headers []string, rows [][]string) []int {
	widths := make([]int, len(headers))
	for i, header := range headers {
		widths[i] = len(header)
	}
	for _, row := range rows {
		for i, value := range row {
			if len(value) > widths[i] {
				widths[i] = len(value)
			}
		}
	}
	return widths
}

func writeTableRow(writer io.Writer, values []string, widths []int) error {
	for i, value := range values {
		if i > 0 {
			if _, err := io.WriteString(writer, "  "); err != nil {
				return writeTableError("write column padding", err)
			}
		}
		if _, err := fmt.Fprintf(writer, "%-*s", widths[i], value); err != nil {
			return writeTableError("write column value", err)
		}
	}
	if _, err := io.WriteString(writer, "\n"); err != nil {
		return writeTableError("write row newline", err)
	}
	return nil
}

func writeTableSeparator(writer io.Writer, widths []int) error {
	for i, width := range widths {
		if i > 0 {
			if _, err := io.WriteString(writer, "  "); err != nil {
				return writeTableError("write separator padding", err)
			}
		}
		separator := strings.Repeat("-", width)
		if _, err := io.WriteString(writer, separator); err != nil {
			return writeTableError("write separator value", err)
		}
	}
	if _, err := io.WriteString(writer, "\n"); err != nil {
		return writeTableError("write separator newline", err)
	}
	return nil
}

func writeTableError(operation string, err error) error {
	slog.Warn("cli.logs.inventory.write_failed", "component", "cli", "operation", operation, "err", err)
	return fmt.Errorf("%s: %w", operation, err)
}

func formatBytes(size int64) string {
	const unit = int64(1024)
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	value := float64(size)
	for _, suffix := range units {
		value /= float64(unit)
		if value < float64(unit) {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/float64(unit))
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.UTC().Format("2006-01-02 15:04:05 MST")
}

func displayPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return "-"
	}
	return path
}

func formatLargestFiles(files []fileSummary) string {
	if len(files) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(files))
	for _, file := range files {
		path := filepath.ToSlash(file.RelativePath)
		parts = append(parts, fmt.Sprintf("%s (%s)", path, formatBytes(file.SizeBytes)))
	}
	return strings.Join(parts, "; ")
}
