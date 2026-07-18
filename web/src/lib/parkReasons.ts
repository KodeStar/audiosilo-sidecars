// Pure mapping from a typed park reason (the internal/state ParkCode enum) to a
// short, actionable hint shown under a parked book's error line. Kept React-free
// and unit-tested. An unknown or empty code returns null (no hint).
//
// A parked book can carry a scheduled auto-readmit instant (BookView.retry_at); that
// wire field is the SERVER's own decision that this park auto-retries, so parkHint takes
// an optional hasRetryAt and flips to the "retries automatically" hint purely on it -
// rather than maintaining a second frontend list of which codes auto-retry (which could
// drift from the scheduler's actual behaviour).

const PARK_HINTS: Record<string, string> = {
  agent_unavailable: 'Configure an agent backend (Settings > Agent), then Retry.',
  agent_rate_limited: 'The agent CLI is rate-limited. Retry later.',
  agent_validation_exhausted:
    'The agent output failed validation repeatedly. Retry re-runs the stage.',
  markers_not_confident:
    "Chapter markers could not be normalized confidently. Check the audio's chapters, then Retry.",
  qa_no_converge:
    'Transcript QA repairs stopped making progress. Inspect qa_report.md in the work dir; Retry grants one fresh adjudication round.',
  spelling_gate_failure:
    'A spelling correction failed its safety gates. Inspect corrections.json, then Retry.',
  media_tools_unavailable: 'ffmpeg/ffprobe are missing. Fix tool paths (Settings), then Retry.',
  asr_unavailable: 'No ASR backend is available. Install/configure one (Settings), then Retry.',
  manifest_changed:
    'The audio changed on disk since transcription. Retry re-runs from the new manifest.',
  fix_loop_exhausted:
    'The audit -> fix loop hit its cap. Review audit.json in the work dir, then Retry.',
  contrib_unavailable: 'Add a GitHub PAT in Settings or run gh auth login, then Retry.',
  core_needed: "This book's work is not on AudioSilo Meta yet - complete the work proposal.",
  core_pending:
    'Work proposal submitted - waiting for the metadata PR to merge; resumes automatically.',
  budget_exceeded:
    "This book reached its agent cost budget. Raise agent.book_budget_usd (Settings > Agent, restart to apply) if it's worth more, then Retry.",
  supervisor_escalated:
    'The supervisor contained a repeated or ambiguous failure. Inspect its decision before Retry.',
  supervisor_budget:
    'The supervisor stopped this stage at a configured limit. Increasing a budget requires an explicit operator change.',
};

// parkHint returns the actionable hint for a park code, or null for an unknown or empty
// code. When hasRetryAt is true the server has scheduled an automatic re-admit, so the
// hint says the book will retry itself once the window elapses instead of asking the
// user to Retry manually. hasRetryAt alone gates this (the server owns that decision);
// an unknown/empty code with no hint still returns null.
export function parkHint(code: string, hasRetryAt = false): string | null {
  const hint = PARK_HINTS[code] ?? null;
  if (hasRetryAt && hint !== null) {
    return 'The agent was temporarily unavailable. This book will retry automatically when the window elapses (or Retry now).';
  }
  return hint;
}
