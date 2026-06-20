package conversation

import "fmt"

// NormalizeCompactionExportOptions validates compaction export controls and
// fills the default segment selector.
func NormalizeCompactionExportOptions(
	options CompactionExportOptions,
	historyStart int,
	lastN int,
) (CompactionExportOptions, error) {
	if historyStart < 0 {
		return CompactionExportOptions{}, fmt.Errorf("--history-start must be greater than or equal to 0")
	}
	if lastN < 0 {
		return CompactionExportOptions{}, fmt.Errorf("--last-n must be greater than or equal to 0")
	}
	if options.FullHistory {
		if options.IncludeSelector != "" && options.IncludeSelector != "all" {
			return CompactionExportOptions{}, fmt.Errorf("--full-history cannot be combined with --include-compactions %s", options.IncludeSelector)
		}
		options.IncludeSelector = "all"
	}
	if options.IncludeSelector == "" {
		options.IncludeSelector = defaultCompactionSelector
	}
	if historyStart > 0 && options.IncludeSelector != "all" {
		return CompactionExportOptions{}, fmt.Errorf("--history-start can only be combined with --include-compactions all")
	}
	return options, nil
}
