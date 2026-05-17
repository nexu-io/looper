package sweeper

const (
	MetricProposalsPrefilterTotal         = "sweeper.proposals.prefilter.total"
	MetricProposalsCreatedTotal           = "sweeper.proposals.created.total"
	MetricProposalsValidationFailedTotal  = "sweeper.proposals.validation_failed.total"
	MetricProposalsStaleTotal             = "sweeper.proposals.stale.total"
	MetricProposalsByProposerKindTotal    = "sweeper.proposals.by_proposer_kind.total"
	MetricApplyAttemptsTotal              = "sweeper.apply.attempts.total"
	MetricApplyDryRunSkippedTotal         = "sweeper.apply.skipped_dry_run.total"
	MetricApplyCeilingSkippedTotal        = "sweeper.apply.skipped_ceiling_reached.total"
	MetricApplyStaleSkippedTotal          = "sweeper.apply.skipped_stale_proposal.total"
	MetricApplySchemaObsoleteSkippedTotal = "sweeper.apply.skipped_schema_obsolete.total"
	MetricApplyPartialResumedTotal        = "sweeper.apply.partial_resumed.total"
	MetricApplyCompletedWarnedTotal       = "sweeper.apply.completed_warned.total"
	MetricApplyCompletedClosedTotal       = "sweeper.apply.completed_closed.total"
	MetricApplyCompletedCancelledTotal    = "sweeper.apply.completed_cancelled.total"
	MetricApplyFailedTotal                = "sweeper.apply.failed.total"
)
