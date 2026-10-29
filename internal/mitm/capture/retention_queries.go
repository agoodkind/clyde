package capture

import _ "embed"

//go:embed sql/retention_initial_cutoff.sql
var retentionInitialCutoffSQL string

//go:embed sql/retention_nonblocking.sql
var retentionNonblockingSQL string

//go:embed sql/retention_restore_busy_timeout.sql
var retentionRestoreBusyTimeoutSQL string

//go:embed sql/retention_history.sql
var retentionHistorySQL string

//go:embed sql/retention_prepare.sql
var retentionPrepareSQL string

//go:embed sql/retention_progress.sql
var retentionProgressSQL string

//go:embed sql/retention_freelist.sql
var retentionFreelistSQL string

//go:embed sql/retention_checkpoint_passive.sql
var retentionCheckpointPassiveSQL string

//go:embed sql/retention_vacuum_mode.sql
var retentionVacuumModeSQL string

//go:embed sql/retention_reclaim_pages.sql
var retentionReclaimPagesSQL string

//go:embed sql/retention_checkpoint_truncate.sql
var retentionCheckpointTruncateSQL string

//go:embed sql/retention_create_selection.sql
var retentionCreateSelectionSQL string

//go:embed sql/retention_clear_selection.sql
var retentionClearSelectionSQL string

//go:embed sql/retention_select_requests.sql
var retentionSelectRequestsSQL string

//go:embed sql/retention_delete_orphan_events.sql
var retentionDeleteOrphanEventsSQL string

//go:embed sql/retention_delete_request_events.sql
var retentionDeleteRequestEventsSQL string

//go:embed sql/retention_delete_decoded_bodies.sql
var retentionDeleteDecodedBodiesSQL string

//go:embed sql/retention_delete_bodies.sql
var retentionDeleteBodiesSQL string

//go:embed sql/retention_delete_requests.sql
var retentionDeleteRequestsSQL string

//go:embed sql/retention_complete.sql
var retentionCompleteSQL string
