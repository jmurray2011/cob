package output

import (
	"github.com/jmurray2011/cob/internal/cob"
)

// renderer is the asset-event sink chosen at Writer construction. Three
// implementations exist:
//
//   - silentRenderer — JSON/quiet: streaming events are discarded, the
//     CommandResult JSON or terminal silence is the only output.
//   - streamRenderer — non-TTY, --no-tui, COB_TUI=0: one OK/FAIL/SKIP
//     line per asset, no in-place updates. Suitable for CI logs and pipes.
//   - liveRenderer   — interactive TTY: bubbletea-backed multi-line
//     progress with per-asset bars, rate, ETA; restores the scrollback on
//     exit so the run remains in shell history.
//
// Splitting these out keeps each command's emission code mode-agnostic.
// New commands inherit correct output for all four modes for free.
type renderer interface {
	// AssetsExpected is called once before the asset stream begins,
	// announcing how many assets and (when known) their total size. The
	// live renderer uses it to size its progress meter; the stream and
	// silent renderers ignore it.
	AssetsExpected(count int, totalBytes int64)

	// AssetStart announces an asset transfer is beginning. size is the
	// known content size, 0 if unknown until the source resolves.
	AssetStart(name, sourceURI string, size int64)

	// AssetProgress reports a byte-count delta for the named asset's
	// in-flight transfer. May fire many times per asset; called from
	// concurrent goroutines.
	AssetProgress(name string, delta int64)

	// AssetOK marks the named asset as successfully transferred. The
	// result carries the canonical SHA-256, size, duration, and method.
	AssetOK(r *cob.AssetResult, sourceURI string)

	// AssetFail marks the named asset as failed with err.
	AssetFail(name, sourceURI string, err error)

	// AssetSkipped marks the named asset as skipped (e.g. on --resume).
	AssetSkipped(name string)

	// Close tears down any background machinery and flushes final output.
	// Idempotent. Required for liveRenderer (bubbletea program teardown);
	// a no-op on stream and silent renderers.
	Close()
}
