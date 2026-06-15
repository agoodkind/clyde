package conversation

import (
	"fmt"
	"slices"
	"strings"
)

// CompactionExportScopeValues lists the user-facing scope values accepted by
// export surfaces.
func CompactionExportScopeValues() []string {
	return []string{
		string(CompactionExportScopeFull),
		string(CompactionExportScopeCurrentContext),
		string(CompactionExportScopeFromCheckpoint),
	}
}

// CompactionExportDetailValues lists the user-facing detail values accepted by
// export surfaces.
func CompactionExportDetailValues() []string {
	return []string{
		string(CompactionExportDetailNone),
		string(CompactionExportDetailSummary),
		string(CompactionExportDetailContext),
		string(CompactionExportDetailFull),
	}
}

// NormalizeCompactionExportOptions validates compaction export controls and
// fills conditional defaults that cannot be expressed as static flag defaults.
func NormalizeCompactionExportOptions(
	options CompactionExportOptions,
	historyStart int,
) (CompactionExportOptions, error) {
	if options.Scope == "" {
		options.Scope = CompactionExportScopeCurrentContext
	}
	if !validCompactionScope(options.Scope) {
		return CompactionExportOptions{}, fmt.Errorf(
			"unsupported compaction scope %q (allowed: %s)",
			options.Scope,
			strings.Join(CompactionExportScopeValues(), ", "),
		)
	}
	if options.Detail == "" {
		options.Detail = defaultCompactionDetail(options.Scope)
	}
	if !validCompactionDetail(options.Detail) {
		return CompactionExportOptions{}, fmt.Errorf(
			"unsupported compaction detail %q (allowed: %s)",
			options.Detail,
			strings.Join(CompactionExportDetailValues(), ", "),
		)
	}
	if options.Scope == CompactionExportScopeFull {
		if options.CheckpointNumber != 0 {
			return CompactionExportOptions{}, fmt.Errorf(
				"--compaction-checkpoint requires --compaction-scope %s",
				CompactionExportScopeFromCheckpoint,
			)
		}
		if options.Detail != CompactionExportDetailNone {
			return CompactionExportOptions{}, fmt.Errorf(
				"--compaction-detail %s requires a compaction scope other than %s",
				options.Detail,
				CompactionExportScopeFull,
			)
		}
		return options, nil
	}
	if historyStart > 0 {
		return CompactionExportOptions{}, fmt.Errorf(
			"--history-start cannot be combined with --compaction-scope %s",
			options.Scope,
		)
	}
	if options.Scope == CompactionExportScopeFromCheckpoint {
		if options.CheckpointNumber <= 0 {
			return CompactionExportOptions{}, fmt.Errorf(
				"--compaction-scope %s requires --compaction-checkpoint",
				CompactionExportScopeFromCheckpoint,
			)
		}
		return options, nil
	}
	if options.CheckpointNumber != 0 {
		return CompactionExportOptions{}, fmt.Errorf(
			"--compaction-checkpoint requires --compaction-scope %s",
			CompactionExportScopeFromCheckpoint,
		)
	}
	return options, nil
}

func defaultCompactionDetail(scope CompactionExportScope) CompactionExportDetail {
	switch scope {
	case CompactionExportScopeCurrentContext, CompactionExportScopeFromCheckpoint:
		return CompactionExportDetailFull
	case CompactionExportScopeFull, "":
		fallthrough
	default:
		return CompactionExportDetailNone
	}
}

func validCompactionScope(scope CompactionExportScope) bool {
	return slices.Contains(CompactionExportScopeValues(), string(scope))
}

func validCompactionDetail(detail CompactionExportDetail) bool {
	return slices.Contains(CompactionExportDetailValues(), string(detail))
}
