package output

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// keyBindings are the keys the live view reacts to. Centralized so adding
// a `help` bubble in the future picks them up via Bubbles' WithHelp text
// automatically — and so the deliberate non-handling of `q`/`esc` (cob's
// TUI is non-interactive; a stray keypress shouldn't kill a 15-minute
// publish) stays an explicit, locatable decision.
var keyBindings = struct {
	cancel key.Binding
}{
	cancel: key.NewBinding(
		key.WithKeys("ctrl+c"),
		key.WithHelp("ctrl+c", "cancel (press twice to force-quit)"),
	),
}

// tea.Msg types — these are the events liveRenderer forwards into the
// program. Keeping them as concrete struct types (rather than interfaces
// over the public asset events) lets Update switch on type assertions
// cleanly.
type (
	assetsExpectedMsg struct {
		count      int
		totalBytes int64
	}
	assetStartMsg struct {
		name      string
		sourceURI string
		size      int64
	}
	assetProgressMsg struct {
		name  string
		delta int64
	}
	assetDoneMsg struct {
		name       string
		sourceURI  string
		size       int64
		sha256     string
		durationMs int64
		method     string
	}
	assetFailMsg struct {
		name      string
		sourceURI string
		err       error
	}
	assetSkipMsg struct {
		name string
	}
	quitMsg struct{}
	tickMsg time.Time
)

// rowState is a single asset's place in the lifecycle. Order matches
// natural sort priority: queued at bottom while running, done floats up.
type rowState int

const (
	stateQueued rowState = iota
	stateActive
	stateDone
	stateSkipped
	stateFailed
)

// assetRow is one displayable line in the live view.
type assetRow struct {
	name     string
	size     int64 // -1 if unknown
	bytes    int64 // bytes transferred so far
	state    rowState
	method   string // verify/pull method (e.g. match(source), spilled, …)
	err      error
	started  time.Time
	finished time.Time
}

// rate tracks recent throughput over a 3s sliding window, sampled into
// 100ms buckets. Used for the overall rate line at the bottom; per-asset
// rate is derived from each row's (bytes/elapsed) since its start.
type rate struct {
	buckets  [30]int64 // 30 × 100ms = 3s window
	cursor   int
	lastTick time.Time
}

func (r *rate) add(delta int64) {
	r.advance()
	r.buckets[r.cursor] += delta
}

func (r *rate) bytesPerSec() float64 {
	r.advance()
	var sum int64
	for _, b := range r.buckets {
		sum += b
	}
	return float64(sum) / 3.0
}

func (r *rate) advance() {
	now := time.Now()
	if r.lastTick.IsZero() {
		r.lastTick = now
		return
	}
	steps := int(now.Sub(r.lastTick) / (100 * time.Millisecond))
	if steps <= 0 {
		return
	}
	for i := 0; i < steps && i < len(r.buckets); i++ {
		r.cursor = (r.cursor + 1) % len(r.buckets)
		r.buckets[r.cursor] = 0
	}
	if steps >= len(r.buckets) {
		for i := range r.buckets {
			r.buckets[i] = 0
		}
	}
	r.lastTick = now
}

// liveModel is the bubbletea program state for cob's transfer view.
//
// interrupted and onInterrupt make Ctrl-C work. Bubbletea's raw mode
// (ISIG off) swallows the kernel's translation of Ctrl-C into SIGINT,
// so the OS-level signal.NotifyContext in main never fires while the
// TUI is up. Catching the KeyMsg here is the only mechanism that
// reaches the in-flight pull/publish/promote goroutines.
type liveModel struct {
	rows          []*assetRow
	byName        map[string]*assetRow
	expectedCount int
	expectedBytes int64 // 0 if unknown
	transferred   int64
	rate          rate
	startTime     time.Time
	width         int
	style         liveStyle

	// interrupted is set by Update when Ctrl-C arrives; the renderer
	// reads it after Run() exits to decide whether the quit was "user
	// asked out" vs "command finished normally".
	interrupted *atomic.Bool
	// onInterrupt fires inside Update on Ctrl-C, before tea.Quit
	// propagates — that way the operation's context is canceled
	// immediately and any in-flight AWS calls start aborting while
	// bubbletea is still tearing down the terminal.
	onInterrupt func()
}

func newLiveModel(interrupted *atomic.Bool, onInterrupt func()) liveModel {
	return liveModel{
		byName:      make(map[string]*assetRow),
		startTime:   time.Now(),
		width:       80,
		style:       newLiveStyle(),
		interrupted: interrupted,
		onInterrupt: onInterrupt,
	}
}

// Init schedules the periodic tick that drives smooth rate / ETA updates
// independent of byte-delta arrivals (so a stalled transfer still ticks
// down its ETA rather than freezing the display).
func (m liveModel) Init() tea.Cmd {
	return tickEvery(150 * time.Millisecond)
}

func tickEvery(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m liveModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.KeyMsg:
		// Ctrl-C lifecycle:
		//
		//  First press: mark interrupted, fire the registered cancel
		//  callback (which cancels the runXxx's ctx so in-flight AWS
		//  calls abort), but DO NOT quit bubbletea — the pull/publish
		//  goroutines are about to emit AssetFail/Skipped for what was
		//  in flight, and those need a live renderer to land on, or
		//  the final paint shows them frozen at their last progress
		//  bar. Bubbletea quits naturally when the runXxx returns and
		//  defer out.Close() lands a quitMsg.
		//
		//  Second press: the goroutines aren't responding (network
		//  hung, SDK retrying past the cancel, whatever). Force-quit
		//  the TUI so the operator gets their shell back; in-flight
		//  goroutines will eventually clean up on their own.
		//
		// Other keys are no-ops — cob's TUI is non-interactive (no
		// `q`/`esc` quit so a stray keypress can't kill a long pull).
		if key.Matches(msg, keyBindings.cancel) {
			if m.interrupted != nil && m.interrupted.Load() {
				return m, tea.Quit
			}
			if m.interrupted != nil {
				m.interrupted.Store(true)
			}
			if m.onInterrupt != nil {
				m.onInterrupt()
			}
			return m, nil
		}
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

	case tickMsg:
		// Idle wake-up to keep ETA/rate refreshed even when no bytes
		// flow (e.g. waiting on a queued upload).
		return m, tickEvery(150 * time.Millisecond)

	case assetsExpectedMsg:
		m.expectedCount = msg.count
		m.expectedBytes = msg.totalBytes
		return m, nil

	case assetStartMsg:
		// Idempotent — a duplicate Start (shouldn't happen but defend)
		// just refreshes the row's state to active.
		ar, ok := m.byName[msg.name]
		if !ok {
			ar = &assetRow{name: msg.name, size: msg.size, started: time.Now()}
			m.rows = append(m.rows, ar)
			m.byName[msg.name] = ar
		}
		ar.state = stateActive
		if ar.size <= 0 {
			ar.size = msg.size
		}
		return m, nil

	case assetProgressMsg:
		// Only update active rows. Once a row has transitioned to
		// done/failed/skipped, late-arriving progress deltas (which
		// can happen on cancellation — the abort path crosses the
		// goroutine's last io.Copy buffer flush) shouldn't push its
		// byte count past 100% or revive a failed row's bar.
		if ar, ok := m.byName[msg.name]; ok && ar.state == stateActive {
			ar.bytes += msg.delta
			m.transferred += msg.delta
			m.rate.add(msg.delta)
		}
		return m, nil

	case assetDoneMsg:
		ar, ok := m.byName[msg.name]
		if !ok {
			// Done arrived without a Start (e.g. asset reported only on
			// completion). Synthesize a row so it appears in the table.
			ar = &assetRow{name: msg.name, started: time.Now()}
			m.rows = append(m.rows, ar)
			m.byName[msg.name] = ar
		}
		ar.state = stateDone
		ar.method = msg.method
		ar.finished = time.Now()
		if msg.size > 0 {
			ar.size = msg.size
		}
		// Reconcile the byte count to the known final size so the row's
		// bar renders as 100% even when AssetProgress under-reported
		// (skipped uploads, identical-file pull short-circuits, etc).
		if ar.size > 0 {
			gap := ar.size - ar.bytes
			if gap > 0 {
				ar.bytes = ar.size
				m.transferred += gap
			}
		}
		return m, nil

	case assetFailMsg:
		ar, ok := m.byName[msg.name]
		if !ok {
			ar = &assetRow{name: msg.name, started: time.Now()}
			m.rows = append(m.rows, ar)
			m.byName[msg.name] = ar
		}
		ar.state = stateFailed
		ar.err = msg.err
		ar.finished = time.Now()
		return m, nil

	case assetSkipMsg:
		ar, ok := m.byName[msg.name]
		if !ok {
			ar = &assetRow{name: msg.name, started: time.Now()}
			m.rows = append(m.rows, ar)
			m.byName[msg.name] = ar
		}
		ar.state = stateSkipped
		ar.finished = time.Now()
		return m, nil

	case quitMsg:
		return m, tea.Quit
	}
	return m, nil
}

// View renders the table. Rows are sorted: done/skipped/failed first
// (settling at the top of the live area), active in the middle, queued
// at the bottom — so the eye lands on what's in flight without scanning.
// When the user has hit Ctrl-C, a "canceling…" line appears above the
// rows so the wait while goroutines abort doesn't feel like a hang.
func (m liveModel) View() string {
	if len(m.rows) == 0 {
		return ""
	}
	ordered := orderRows(m.rows)
	var b strings.Builder
	if m.interrupted != nil && m.interrupted.Load() {
		b.WriteString(m.style.failed.Render("Canceling… (Ctrl-C again to force-quit)"))
		b.WriteByte('\n')
		b.WriteByte('\n')
	}
	nameWidth := pickNameWidth(ordered, m.width)
	for _, r := range ordered {
		b.WriteString(m.style.renderRow(r, nameWidth, m.width))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(m.style.renderSummary(m.transferred, m.expectedBytes, m.rate.bytesPerSec(), m.startTime, m.width))
	b.WriteByte('\n')
	return b.String()
}

// orderRows groups by lifecycle state so the visual order stays stable
// across paints — completed rows settle at the top, active in the
// middle, queued at the bottom. Within a group, original arrival order
// is preserved.
func orderRows(rows []*assetRow) []*assetRow {
	var done, active, queued []*assetRow
	for _, r := range rows {
		switch r.state {
		case stateDone, stateSkipped, stateFailed:
			done = append(done, r)
		case stateActive:
			active = append(active, r)
		default:
			queued = append(queued, r)
		}
	}
	out := make([]*assetRow, 0, len(rows))
	out = append(out, done...)
	out = append(out, active...)
	out = append(out, queued...)
	return out
}

// pickNameWidth picks a column width for asset names that fits the
// widest current row name, capped to leave room for the rest of the
// columns (bar, bytes, rate, ETA). Names longer than the cap are
// truncated at render time.
func pickNameWidth(rows []*assetRow, termWidth int) int {
	const minCol, maxCol = 12, 36
	want := minCol
	for _, r := range rows {
		if n := lipgloss.Width(r.name); n > want {
			want = n
		}
	}
	cap := termWidth - 50 // leave ~50 chars for bar + sizes + rate + ETA
	if cap < minCol {
		cap = minCol
	}
	if cap > maxCol {
		cap = maxCol
	}
	if want > cap {
		want = cap
	}
	return want
}

// liveStyle holds the lipgloss styles. Centralized so a future theme
// switch is one place; lipgloss auto-falls-back on terminals that can't
// render colors.
type liveStyle struct {
	done     lipgloss.Style
	active   lipgloss.Style
	queued   lipgloss.Style
	failed   lipgloss.Style
	skipped  lipgloss.Style
	meta     lipgloss.Style
	barFill  lipgloss.Style
	barEmpty lipgloss.Style
	summary  lipgloss.Style
}

func newLiveStyle() liveStyle {
	// AdaptiveColor picks the right shade for the user's terminal
	// background — lipgloss does the dark/light detection. The previous
	// ANSI 16-color codes (10, 12, 8, ...) rendered as "bright blue" on
	// Solarized-light terminals, which is essentially invisible. Hex
	// codes pinned per side fix that without us having to detect
	// anything ourselves.
	return liveStyle{
		done: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{
			Light: "#2e7d32", Dark: "#7ee787", // green
		}),
		active: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{
			Light: "#0969da", Dark: "#79c0ff", // blue
		}),
		queued: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{
			Light: "#6e7781", Dark: "#7d8590", // dim gray
		}),
		failed: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{
			Light: "#cf222e", Dark: "#ff7b72", // red
		}),
		skipped: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{
			Light: "#9a6700", Dark: "#d29922", // amber
		}),
		meta: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{
			Light: "#6e7781", Dark: "#7d8590", // dim gray
		}),
		barFill: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{
			Light: "#0969da", Dark: "#79c0ff", // blue
		}),
		barEmpty: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{
			Light: "#d0d7de", Dark: "#3d444d", // dim gray, background-leaning
		}),
		summary: lipgloss.NewStyle().Bold(true),
	}
}

// renderRow returns one row of the live table: state glyph, name (left-
// padded to nameWidth), then state-specific trailing content (progress
// bar + bytes for active, duration for done, error for failed).
func (s liveStyle) renderRow(r *assetRow, nameWidth, termWidth int) string {
	name := truncateName(r.name, nameWidth)
	switch r.state {
	case stateDone:
		dur := FormatDuration((r.finished.Sub(r.started)).Milliseconds())
		size := FormatSize(maxInt64(r.size, r.bytes))
		method := r.method
		if method != "" {
			method = " " + method
		}
		return fmt.Sprintf("  %s %s  %s  %s%s",
			s.done.Render("✓"),
			padRight(name, nameWidth),
			s.meta.Render(rightPad(size, 10)),
			s.meta.Render(dur),
			s.meta.Render(method),
		)
	case stateSkipped:
		return fmt.Sprintf("  %s %s  %s",
			s.skipped.Render("⊘"),
			padRight(name, nameWidth),
			s.meta.Render("skipped"),
		)
	case stateFailed:
		msg := ""
		if r.err != nil {
			msg = r.err.Error()
		}
		return fmt.Sprintf("  %s %s  %s",
			s.failed.Render("✗"),
			padRight(name, nameWidth),
			s.failed.Render(msg),
		)
	case stateActive:
		// Bar + bytes/total + per-row rate + ETA, sized to leave room
		// on narrow terminals.
		barWidth := termWidth - nameWidth - 38
		if barWidth < 8 {
			barWidth = 8
		}
		bar := s.renderBar(r.bytes, r.size, barWidth)
		var sizeText string
		if r.size > 0 {
			sizeText = fmt.Sprintf("%s/%s", FormatSize(r.bytes), FormatSize(r.size))
		} else {
			sizeText = FormatSize(r.bytes)
		}
		elapsed := time.Since(r.started).Seconds()
		var rateText, etaText string
		if elapsed > 0.1 && r.bytes > 0 {
			rateBps := float64(r.bytes) / elapsed
			rateText = FormatSize(int64(rateBps)) + "/s"
			if r.size > r.bytes {
				etaText = formatETA(time.Duration(float64(r.size-r.bytes)/rateBps) * time.Second)
			}
		}
		return fmt.Sprintf("  %s %s  %s  %s  %s  %s",
			s.active.Render("⏵"),
			padRight(name, nameWidth),
			bar,
			s.meta.Render(rightPad(sizeText, 17)),
			s.meta.Render(rightPad(rateText, 10)),
			s.meta.Render(etaText),
		)
	default: // queued
		size := ""
		if r.size > 0 {
			size = FormatSize(r.size)
		}
		return fmt.Sprintf("  %s %s  %s",
			s.queued.Render("·"),
			padRight(name, nameWidth),
			s.meta.Render(size),
		)
	}
}

// renderBar draws a unicode block-fill progress bar of the given width.
// Unknown total (size <= 0) renders as a marquee-style dim bar so the
// row still conveys "in flight" without faking a percentage.
func (s liveStyle) renderBar(bytes, size int64, width int) string {
	if width < 4 {
		width = 4
	}
	if size <= 0 {
		return s.barEmpty.Render(strings.Repeat("░", width))
	}
	frac := float64(bytes) / float64(size)
	if frac > 1 {
		frac = 1
	}
	if frac < 0 {
		frac = 0
	}
	filled := int(frac * float64(width))
	return s.barFill.Render(strings.Repeat("█", filled)) +
		s.barEmpty.Render(strings.Repeat("░", width-filled))
}

// renderSummary draws the trailing total line: bytes transferred (with
// the known total when there is one), the overall rate, and an ETA when
// the total is known.
func (s liveStyle) renderSummary(transferred, total int64, bps float64, start time.Time, termWidth int) string {
	var sizeText string
	if total > 0 {
		pct := int64(0)
		if total > 0 {
			pct = transferred * 100 / total
		}
		sizeText = fmt.Sprintf("%s / %s (%d%%)", FormatSize(transferred), FormatSize(total), pct)
	} else {
		sizeText = FormatSize(transferred)
	}
	rate := fmt.Sprintf("%s/s", FormatSize(int64(bps)))
	var eta string
	if total > 0 && bps > 0 && transferred < total {
		eta = "ETA " + formatETA(time.Duration(float64(total-transferred)/bps)*time.Second)
	}
	return s.summary.Render(fmt.Sprintf("Total: %s  %s  %s", sizeText, rate, eta))
}

func formatETA(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func truncateName(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	if width <= 1 {
		return "…"
	}
	return s[:width-1] + "…"
}

func padRight(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

func rightPad(s string, width int) string { return padRight(s, width) }

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
