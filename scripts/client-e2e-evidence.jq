# Read only complete structured messages; stream_event repeats are not tool calls.
def calls:
  [to_entries[] | select(.value.type == "assistant") |
    .key as $position | .value.message.content[]? | select(.type == "tool_use") |
    . + {position: $position}];
def replies:
  [to_entries[] | select(.value.type == "user") |
    .key as $position | .value.message.content[]? | select(.type == "tool_result") |
    . + {position: $position}];
def content_text:
  if type == "string" then . else tojson end;
def same_target($a; $b):
  ($a.name == $b.name) and
  (($a.input.file_path // $a.input.path // null) as $target |
   if $target != null then
     $target == ($b.input.file_path // $b.input.path // null)
   else $a.input == $b.input end);

. as $events | calls as $calls | replies as $replies |
([$events[] | select(.type == "result")] | last) as $terminal |
[$replies[] | select(.is_error == true)] as $errors |
[$errors[] | . as $error |
  ([$calls[] | select(.id == $error.tool_use_id)] | first) as $failed |
  select(($failed == null) or
    ([ $calls[] | select(.position > $error.position and same_target(.; $failed)) |
       . as $retry | $replies[] |
       select(.tool_use_id == $retry.id and .is_error != true and .position > $retry.position)
     ] | length) == 0)] as $unrecovered |
([$events[] | select(.type == "system" and .subtype == "init") | .tools // []] | first // []) as $tools |
{
  initialized: any($events[]; .type == "system" and .subtype == "init"),
  tools: $tools,
  # Claude Code can recover a truncated response by inserting a user turn
  # without exposing an error event. Count only its known standalone message.
  automaticContinuations: ([$events[] | select(.type == "user") |
    .message.content? | select(type == "array" and length == 1) | .[0] |
    select(.type == "text" and (.text | type) == "string") |
    select(.text | startswith("Your response above was cut off mid-stream. Resume directly from where it stops"))] | length),
  terminalSuccess: ($terminal != null and $terminal.subtype == "success" and $terminal.is_error != true),
  protocolError: any($events[]; .type == "error" or .event.type? == "error" or
    (.type == "assistant" and .isApiErrorMessage == true)),
  protocolErrorCount: ([$events[] | select(.type == "error" or .event.type? == "error" or
    (.type == "assistant" and .isApiErrorMessage == true))] | length),
  subtype: ($terminal.subtype // "missing"),
  calls: ($calls | length), results: ($replies | length),
  errors: ($errors | length), recoveredErrors: (($errors | length) - ($unrecovered | length)),
  unrecoveredErrors: ($unrecovered | length),
  paired: (
    ($calls | length) == ($replies | length) and
    all($calls[]; (.id | type) == "string" and (.id | length) > 0) and
    ($calls | map(.id) | unique | length) == ($calls | length) and
    all($calls[]; . as $call |
      ([ $replies[] | select(.tool_use_id == $call.id and .position > $call.position)] | length) == 1)),
  fileRoundtrip: (
    [ $calls[] | select(.name == "Read") | . as $read |
      $replies[] | select(.tool_use_id == $read.id and .is_error != true) |
      . + {file: ($read.input.file_path // "")} ] as $reads |
    any($reads[]; . as $first |
      (.content | content_text | contains("FILE_WRITE_OK")) and
      any($calls[]; . as $edit |
        .name == "Edit" and .position > $first.position and
        .input.file_path == $first.file and
        any($replies[]; .tool_use_id == $edit.id and .is_error != true) and
        any($reads[]; .file == $first.file and .position > $edit.position and
          (.content | content_text | contains("FILE_EDIT_OK")))))),
  workspaceEdit: any($calls[]; . as $edit |
    (.name == "Edit" or .name == "Write") and
    (.input.file_path // "" | . == "workflow.sh" or endswith("/workflow.sh")) and
    any($replies[]; .tool_use_id == $edit.id and .is_error != true)),
  searched: any($calls[]; . as $search |
    .name == "WebSearch" and
    any($replies[]; .tool_use_id == $search.id and .is_error != true and
      (.content | content_text | test("https?://[^[:space:]<>]+"))))
}
