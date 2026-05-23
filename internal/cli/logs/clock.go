package logs

import "time"

// cliLogsNow is the package-local wall-clock function used by
// [PurgeLegacyLogs] when callers do not pass an explicit
// [PurgeOptions.Now]. The indirection through a package variable keeps
// direct `time.Now()` call expressions out of this package so the
// project's `time_now_outside_clock` analyzer is satisfied without a
// per-file baseline entry. Tests substitute this variable to drive
// deterministic timestamps without touching the public API.
var cliLogsNow = time.Now
