package output

// Mode is the rendering configuration cob picks once at Writer construction
// from the command-line/env. The Writer's public API doesn't change with
// mode — commands emit the same semantic events (Header, AssetStart,
// AssetProgress, etc.); the renderer chosen for this Mode decides how to
// draw them.
//
//	JSON  → all asset/streaming events are dropped; only CommandResult and
//	        Warn write to stdout/stderr respectively
//	Quiet → suppresses headers, summaries, and asset-stream events; errors
//	        and warnings still surface
//	NoTUI → forces the line-stream renderer even on an interactive TTY
//	        (set by --no-tui or COB_TUI=0)
//
// The decision tree the Writer uses to pick a renderer:
//
//	JSON or Quiet            → silentRenderer
//	TTY && !JSON && !Quiet && !NoTUI → liveRenderer (bubbletea)
//	otherwise                → streamRenderer (CI logs, pipes, --no-tui)
type Mode struct {
	JSON  bool
	Quiet bool
	NoTUI bool
}
