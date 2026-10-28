package capture_test

import _ "embed"

//go:embed testdata/retention/count_requests.sql
var testCountRequestsSQL string

//go:embed testdata/retention/initial_complete.sql
var testInitialCompleteSQL string

//go:embed testdata/retention/count_new_requests.sql
var testCountNewRequestsSQL string

//go:embed testdata/retention/count_decoded_bodies.sql
var testCountDecodedBodiesSQL string

//go:embed testdata/retention/count_decoded_tool_events.sql
var testCountDecodedToolEventsSQL string

//go:embed testdata/retention/freelist_count.sql
var testFreelistCountSQL string

//go:embed testdata/retention/count_old_requests.sql
var testCountOldRequestsSQL string

//go:embed testdata/retention/delete_requests.sql
var testDeleteRequestsSQL string

//go:embed testdata/retention/count_bodies.sql
var testCountBodiesSQL string

//go:embed testdata/retention/count_burst_complete.sql
var testCountBurstCompleteSQL string

//go:embed testdata/retention/count_drift_shapes.sql
var testCountDriftShapesSQL string

//go:embed testdata/retention/count_drift_checks.sql
var testCountDriftChecksSQL string

//go:embed testdata/retention/count_table.sql
var testCountTableSQL string

//go:embed testdata/retention/count_upstream_shapes.sql
var testCountUpstreamShapesSQL string

//go:embed testdata/retention/shape_seen_count.sql
var testShapeSeenCountSQL string

//go:embed testdata/retention/shape_dedup_counts.sql
var testShapeDedupCountsSQL string

//go:embed testdata/retention/count_claude_shapes.sql
var testCountClaudeShapesSQL string

//go:embed testdata/retention/count_claude_baselines.sql
var testCountClaudeBaselinesSQL string

//go:embed testdata/retention/baseline_diff_counts.sql
var testBaselineDiffCountsSQL string

//go:embed testdata/retention/drift_check_counts.sql
var testDriftCheckCountsSQL string
