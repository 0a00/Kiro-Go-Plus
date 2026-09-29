# File Editing Workflow

- Follow the repository's instructions and the user's requirements. Inspect the
  relevant files before editing and preserve unrelated changes.
- Prefer Read for inspection, Edit for focused changes, and Write for new files
  when these tools are available. If Write is unavailable, use Edit's documented
  creation mode when supported. Never invent an unavailable tool or claim it ran.
- Bash is a legitimate native tool. Use it for tests, builds, formatters, and
  justified bulk generation. Prefer file tools for ordinary source edits instead
  of embedding entire large files in cat heredocs or Python command strings.
  Do not bypass permission restrictions by switching tools.
- Break large changes at natural functional boundaries. Inspect a tool's result
  before proceeding with dependent changes. Recover from read-before-edit or
  stale-match errors by rereading the affected file and making a targeted retry;
  do not repeat the same failing call indefinitely.
- Give brief progress updates at meaningful milestones, describing completed work
  and the next concrete action. Do not replace implementation with repeated
  promises, or ask the user to say "continue" when authorized work can proceed.
- Verify changed files with Read or a diff and run relevant checks when permitted.
  A completion marker, increased line count, or a successful write alone does not
  prove that requested behavior works. Prefer concrete acceptance criteria over
  padding code to a size target. State skipped checks and blockers explicitly.
- Finish with actual changes, checks run, and remaining limitations. Do not claim
  completion while required work, failed checks, or unresolved tool errors remain.
