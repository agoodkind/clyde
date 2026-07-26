package parser

// EntryType names the record kind in a Claude transcript line's "type" field.
// Claude Code writes conversation turns and a family of sidecar control records
// into one JSONL file, and the type decides which fields a line carries.
type EntryType string

const (
	// EntryTypeUnspecified marks a line with no type field.
	EntryTypeUnspecified EntryType = ""
	// EntryTypeUser marks a user turn, which also carries tool results.
	EntryTypeUser EntryType = "user"
	// EntryTypeAssistant marks one assistant content block.
	EntryTypeAssistant EntryType = "assistant"
	// EntryTypeSystem marks a system control record such as a compaction boundary.
	EntryTypeSystem EntryType = "system"
	// EntryTypeAttachment marks a hook, reminder, or injected-context record.
	EntryTypeAttachment EntryType = "attachment"
	// EntryTypeQueueOperation marks a prompt-queue mutation.
	EntryTypeQueueOperation EntryType = "queue-operation"
	// EntryTypeLastPrompt records the latest prompt and the current leaf entry.
	EntryTypeLastPrompt EntryType = "last-prompt"
	// EntryTypeMode records the session interaction mode.
	EntryTypeMode EntryType = "mode"
	// EntryTypePermissionMode records a permission-mode change.
	EntryTypePermissionMode EntryType = "permission-mode"
	// EntryTypeAgentName records the display name chosen for the session.
	EntryTypeAgentName EntryType = "agent-name"
	// EntryTypePullRequestLink records a pull request the session touched.
	EntryTypePullRequestLink EntryType = "pr-link"
	// EntryTypeCustomTitle records a user-set conversation title.
	EntryTypeCustomTitle EntryType = "custom-title"
	// EntryTypeAITitle records a model-generated conversation title.
	EntryTypeAITitle EntryType = "ai-title"
	// EntryTypeFileHistorySnapshot records tracked-file backups for one message.
	EntryTypeFileHistorySnapshot EntryType = "file-history-snapshot"
	// EntryTypeFileHistoryDelta records one tracked-file backup.
	EntryTypeFileHistoryDelta EntryType = "file-history-delta"
	// EntryTypeWorktreeState records the worktree a session entered.
	EntryTypeWorktreeState EntryType = "worktree-state"
	// EntryTypeRelocated records a working-directory relocation.
	EntryTypeRelocated EntryType = "relocated"
	// EntryTypeBridgeSession records the bridged remote session and its sequence.
	EntryTypeBridgeSession EntryType = "bridge-session"
	// EntryTypeStarted marks a background agent starting.
	EntryTypeStarted EntryType = "started"
	// EntryTypeResult marks a background agent result payload.
	EntryTypeResult EntryType = "result"
)

// EntrySubtype names the variant of a system record. Only the compaction
// boundaries reach the parsed transcript; the rest are session telemetry.
type EntrySubtype string

const (
	// EntrySubtypeUnspecified marks a system record with no subtype.
	EntrySubtypeUnspecified EntrySubtype = ""
	// EntrySubtypeCompactBoundary marks a full compaction boundary.
	EntrySubtypeCompactBoundary EntrySubtype = "compact_boundary"
	// EntrySubtypeMicrocompactBoundary marks a micro-compaction boundary.
	EntrySubtypeMicrocompactBoundary EntrySubtype = "microcompact_boundary"
	// EntrySubtypeTurnDuration reports how long one assistant turn took.
	EntrySubtypeTurnDuration EntrySubtype = "turn_duration"
	// EntrySubtypeStopHookSummary reports the Stop hook fan-out result.
	EntrySubtypeStopHookSummary EntrySubtype = "stop_hook_summary"
	// EntrySubtypeScheduledTaskFire reports a scheduled task firing.
	EntrySubtypeScheduledTaskFire EntrySubtype = "scheduled_task_fire"
	// EntrySubtypeAwaySummary reports what happened while the user was away.
	EntrySubtypeAwaySummary EntrySubtype = "away_summary"
	// EntrySubtypeLocalCommand reports a locally executed slash command.
	EntrySubtypeLocalCommand EntrySubtype = "local_command"
	// EntrySubtypeAPIError reports an upstream API failure and its retry plan.
	EntrySubtypeAPIError EntrySubtype = "api_error"
	// EntrySubtypeBridgeStatus reports remote-bridge connectivity.
	EntrySubtypeBridgeStatus EntrySubtype = "bridge_status"
	// EntrySubtypeModelRefusalNoFallback reports a refusal with no fallback model.
	EntrySubtypeModelRefusalNoFallback EntrySubtype = "model_refusal_no_fallback"
	// EntrySubtypeModelRefusalFallback reports a refusal that fell back to another model.
	EntrySubtypeModelRefusalFallback EntrySubtype = "model_refusal_fallback"
	// EntrySubtypeInformational reports a plain informational notice.
	EntrySubtypeInformational EntrySubtype = "informational"
	// EntrySubtypeAgentsKilled reports background agents terminated by the session.
	EntrySubtypeAgentsKilled EntrySubtype = "agents_killed"
)

// EntryLevel is the severity Claude attaches to a system record.
type EntryLevel string

const (
	// EntryLevelUnspecified marks a record with no level.
	EntryLevelUnspecified EntryLevel = ""
	// EntryLevelSuggestion marks advisory hook or workflow output.
	EntryLevelSuggestion EntryLevel = "suggestion"
	// EntryLevelInfo marks an informational notice.
	EntryLevelInfo EntryLevel = "info"
	// EntryLevelWarning marks a warning notice.
	EntryLevelWarning EntryLevel = "warning"
	// EntryLevelError marks a failure notice such as an API error.
	EntryLevelError EntryLevel = "error"
)

// Entrypoint names the client that produced the session.
type Entrypoint string

const (
	// EntrypointUnspecified marks a record with no entrypoint.
	EntrypointUnspecified Entrypoint = ""
	// EntrypointCLI marks the interactive claude CLI.
	EntrypointCLI Entrypoint = "cli"
	// EntrypointSDKTypeScript marks the TypeScript Agent SDK.
	EntrypointSDKTypeScript Entrypoint = "sdk-ts"
	// EntrypointSDKCLI marks the SDK's headless CLI runner.
	EntrypointSDKCLI Entrypoint = "sdk-cli"
)

// PermissionMode is the tool-permission posture in force for a turn.
type PermissionMode string

const (
	// PermissionModeUnspecified marks a record with no permission mode.
	PermissionModeUnspecified PermissionMode = ""
	// PermissionModeDefault prompts for every unapproved tool call.
	PermissionModeDefault PermissionMode = "default"
	// PermissionModePlan restricts the session to read-only planning.
	PermissionModePlan PermissionMode = "plan"
	// PermissionModeAuto lets the harness decide per call.
	PermissionModeAuto PermissionMode = "auto"
	// PermissionModeAcceptEdits pre-approves file edits.
	PermissionModeAcceptEdits PermissionMode = "acceptEdits"
	// PermissionModeDontAsk suppresses prompts for the current run.
	PermissionModeDontAsk PermissionMode = "dontAsk"
	// PermissionModeBypassPermissions approves every tool call.
	PermissionModeBypassPermissions PermissionMode = "bypassPermissions"
)

// PromptSource names where a user turn's prompt text came from.
type PromptSource string

const (
	// PromptSourceUnspecified marks a turn with no recorded prompt source.
	PromptSourceUnspecified PromptSource = ""
	// PromptSourceTyped marks a prompt the user typed interactively.
	PromptSourceTyped PromptSource = "typed"
	// PromptSourceQueued marks a prompt dequeued from the prompt queue.
	PromptSourceQueued PromptSource = "queued"
	// PromptSourceSystem marks a prompt the harness injected.
	PromptSourceSystem PromptSource = "system"
	// PromptSourceSDK marks a prompt supplied through the Agent SDK.
	PromptSourceSDK PromptSource = "sdk"
	// PromptSourceSuggestionAccepted marks an accepted suggested prompt.
	PromptSourceSuggestionAccepted PromptSource = "suggestion_accepted"
)

// QueueOperation names a mutation of the pending prompt queue.
type QueueOperation string

const (
	// QueueOperationUnspecified marks a queue record with no operation.
	QueueOperationUnspecified QueueOperation = ""
	// QueueOperationEnqueue adds a prompt to the queue.
	QueueOperationEnqueue QueueOperation = "enqueue"
	// QueueOperationDequeue takes the next prompt off the queue.
	QueueOperationDequeue QueueOperation = "dequeue"
	// QueueOperationRemove drops one queued prompt.
	QueueOperationRemove QueueOperation = "remove"
	// QueueOperationPopAll drains the queue.
	QueueOperationPopAll QueueOperation = "popAll"
)

// ToolDenialKind names why a tool call was refused.
type ToolDenialKind string

const (
	// ToolDenialKindUnspecified marks a turn with no denial.
	ToolDenialKindUnspecified ToolDenialKind = ""
	// ToolDenialKindPermissionRule marks a denial by a configured permission rule.
	ToolDenialKindPermissionRule ToolDenialKind = "permission-rule"
	// ToolDenialKindUserRejected marks a denial the user chose at the prompt.
	ToolDenialKindUserRejected ToolDenialKind = "user-rejected"
	// ToolDenialKindAutomodeBlocked marks a denial by automatic-mode policy.
	ToolDenialKindAutomodeBlocked ToolDenialKind = "automode-blocked"
	// ToolDenialKindAutomodeUnavailable marks a denial because automatic mode was unavailable.
	ToolDenialKindAutomodeUnavailable ToolDenialKind = "automode-unavailable"
)

// OriginKind names who or what produced a user turn.
type OriginKind string

const (
	// OriginKindUnspecified marks a turn with no origin record.
	OriginKindUnspecified OriginKind = ""
	// OriginKindHuman marks a turn the person typed.
	OriginKindHuman OriginKind = "human"
	// OriginKindTaskNotification marks a turn injected by a background task.
	OriginKindTaskNotification OriginKind = "task-notification"
	// OriginKindCoordinator marks a turn injected by a coordinating agent.
	OriginKindCoordinator OriginKind = "coordinator"
	// OriginKindAutoContinuation marks a turn the harness generated to continue work.
	OriginKindAutoContinuation OriginKind = "auto-continuation"
)

// AttachmentType names the payload an attachment record carries. Claude writes
// one attachment record per injected context item, and the type selects which
// of [Attachment]'s fields are populated.
type AttachmentType string

const (
	// AttachmentTypeUnspecified marks an attachment with no type.
	AttachmentTypeUnspecified AttachmentType = ""
	// AttachmentTypeHookSuccess marks a hook that ran and exited cleanly.
	AttachmentTypeHookSuccess AttachmentType = "hook_success"
	// AttachmentTypeHookCancelled marks a hook the harness cancelled.
	AttachmentTypeHookCancelled AttachmentType = "hook_cancelled"
	// AttachmentTypeHookNonBlockingError marks a hook that failed without blocking the turn.
	AttachmentTypeHookNonBlockingError AttachmentType = "hook_non_blocking_error"
	// AttachmentTypeHookAdditionalContext marks context a hook added to the turn.
	AttachmentTypeHookAdditionalContext AttachmentType = "hook_additional_context"
	// AttachmentTypeOutputStyle marks the active output style and its turn reminder.
	AttachmentTypeOutputStyle AttachmentType = "output_style"
	// AttachmentTypeTaskReminder marks an injected task-list reminder.
	AttachmentTypeTaskReminder AttachmentType = "task_reminder"
	// AttachmentTypeTaskStatus marks a background task status update.
	AttachmentTypeTaskStatus AttachmentType = "task_status"
	// AttachmentTypeQueuedCommand marks a prompt taken from the queue.
	AttachmentTypeQueuedCommand AttachmentType = "queued_command"
	// AttachmentTypeFile marks a file the harness read into context.
	AttachmentTypeFile AttachmentType = "file"
	// AttachmentTypeEditedTextFile marks an edited file re-injected into context.
	AttachmentTypeEditedTextFile AttachmentType = "edited_text_file"
	// AttachmentTypeReadTruncationNotice marks a notice that a file read was truncated.
	AttachmentTypeReadTruncationNotice AttachmentType = "read_truncation_notice"
	// AttachmentTypeCommandPermissions marks the tools a slash command may use.
	AttachmentTypeCommandPermissions AttachmentType = "command_permissions"
	// AttachmentTypeDeferredToolsDelta marks a change to the deferred tool set.
	AttachmentTypeDeferredToolsDelta AttachmentType = "deferred_tools_delta"
	// AttachmentTypeMCPInstructionsDelta marks a change to MCP server instructions.
	AttachmentTypeMCPInstructionsDelta AttachmentType = "mcp_instructions_delta"
	// AttachmentTypeAgentListingDelta marks a change to the available agent list.
	AttachmentTypeAgentListingDelta AttachmentType = "agent_listing_delta"
	// AttachmentTypeSkillListing marks the available skill list.
	AttachmentTypeSkillListing AttachmentType = "skill_listing"
	// AttachmentTypeInvokedSkills marks the skills a turn invoked.
	AttachmentTypeInvokedSkills AttachmentType = "invoked_skills"
	// AttachmentTypeGoalStatus marks a goal-tracking update.
	AttachmentTypeGoalStatus AttachmentType = "goal_status"
	// AttachmentTypeDateChange marks a calendar-date rollover notice.
	AttachmentTypeDateChange AttachmentType = "date_change"
	// AttachmentTypeCompactFileReference marks a file referenced across a compaction.
	AttachmentTypeCompactFileReference AttachmentType = "compact_file_reference"
	// AttachmentTypePlanFileReference marks the plan file backing plan mode.
	AttachmentTypePlanFileReference AttachmentType = "plan_file_reference"
	// AttachmentTypePlanMode marks entry into plan mode.
	AttachmentTypePlanMode AttachmentType = "plan_mode"
	// AttachmentTypePlanModeExit marks leaving plan mode.
	AttachmentTypePlanModeExit AttachmentType = "plan_mode_exit"
	// AttachmentTypePlanModeReentry marks re-entering plan mode.
	AttachmentTypePlanModeReentry AttachmentType = "plan_mode_reentry"
	// AttachmentTypeNestedMemory marks a nested memory file pulled into context.
	AttachmentTypeNestedMemory AttachmentType = "nested_memory"
)
