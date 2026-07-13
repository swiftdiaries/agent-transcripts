# Review-Stream Transcript Design

## Goal

Make local Codex and Claude Code sessions readable as reviewable turns rather
than as provider transport logs. Preserve the normalized source facts needed
for diagnostics without putting them in the primary reading flow.

## Scope

- Normalize the current Claude and Codex record shapes into a shared,
  review-oriented projection.
- Render prompts, assistant responses, and tool activity as turns.
- Keep provider-only or unsupported records available as collapsed diagnostics.
- Add regression fixtures for the observed current provider shapes.

No source file is rewritten and no provider record is discarded from the
stored source package.

## Provider Rules

### Claude Code

Provider detection scans records until it finds a Claude conversation record;
leading title, queue, hook, and attachment records do not reject the session.
Message content is processed one block at a time. Text, tool calls, and tool
results become visible events. Thinking and unknown blocks remain diagnostic
events, so they cannot hide other blocks in the same message.

### Codex

`event_msg.user_message` is the visible user prompt. The corresponding
`response_item` user payload is supplemental source detail: it can include
images and harness serialization that should not replace the prompt.
`response_item` assistant messages, `custom_tool_call`, and
`custom_tool_call_output` become visible activity. Developer messages,
environment contexts, world state, token counts, and encrypted reasoning are
diagnostics. The parser extracts text blocks from tool outputs and retains
other output blocks as diagnostics.

## Review Projection and Rendering

A review turn begins at a visible user prompt and contains all following
visible assistant and tool events up to the next visible prompt. The page uses
the turn list as its primary stream and prompt index. Tool inputs and results
are collapsed within the turn. Diagnostics are collapsed and do not appear in
the prompt index or as standalone primary cards.

The projection is derived from the normalized session after parsing. This
keeps parsing responsible for provider semantics and keeps templates
responsible for presentation.

## Tests

Parser tests prove Codex uses the event-message prompt, maps custom tool calls
and outputs, and does not promote developer/environment messages. Claude tests
prove leading metadata is accepted and a thinking block does not suppress a
sibling visible block. Web tests prove the primary transcript stream contains
turns and excludes diagnostic-only records from the prompt index.

## Error Handling

Malformed JSON and size limits retain their current failure behavior. Unknown
valid records stay normalized as raw diagnostics, preserving forward
compatibility without degrading the reading flow.
