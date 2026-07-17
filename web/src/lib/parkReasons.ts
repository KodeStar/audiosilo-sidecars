// Pure mapping from a typed park reason (the internal/state ParkCode enum) to a
// short, actionable hint shown under a parked book's error line. Kept React-free
// and unit-tested. An unknown or empty code returns null (no hint).

const PARK_HINTS: Record<string, string> = {
  agent_unavailable: 'Configure an agent backend (Settings > Agent), then Retry.',
  agent_rate_limited: 'The agent CLI is rate-limited. Retry later.',
  agent_validation_exhausted:
    'The agent output failed validation repeatedly. Retry re-runs the stage.',
  markers_not_confident:
    "Chapter markers could not be normalized confidently. Check the audio's chapters, then Retry.",
  qa_no_converge:
    'Transcript QA did not converge after 3 rounds. Inspect qa_report.md in the work dir.',
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
};

// parkHint returns the actionable hint for a park code, or null for an unknown
// or empty code.
export function parkHint(code: string): string | null {
  return PARK_HINTS[code] ?? null;
}
