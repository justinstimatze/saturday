package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/api/drive/v3"

	"saturday/inject"
	"saturday/watcherclient"
)

// buildManifestContent renders the live-session inventory voice mode is
// meant to check before naming a session in a note. Split into two lists,
// not one: watcher considers a session "live" purely from recent JSONL
// activity, which says nothing about whether saturday-backend can actually
// reach it via a tmux pane right now — a session with no pane still gets a
// real inject (commitInject falls back to direct-write/headless), just not
// a live-interactive one. Hiding those entirely would drop real, working
// targets from the inventory; folding them into one undifferentiated list
// would misrepresent what "live" means. tmuxLive is keyed by cwd (the same
// key inject.FindTmuxPane matches on), built by the caller since the tmux
// lookup itself is a live system call, not something this pure function
// should perform — same reasoning as buildQuery's since parameter for now.
func buildManifestContent(sessions []watcherclient.SessionEntry, tmuxLive map[string]bool, now time.Time) string {
	var tmux, headlessOnly []watcherclient.SessionEntry
	for _, s := range sessions {
		if tmuxLive[s.State.Cwd] {
			tmux = append(tmux, s)
		} else {
			headlessOnly = append(headlessOnly, s)
		}
	}
	sort.Slice(tmux, func(i, j int) bool { return tmux[i].State.Project < tmux[j].State.Project })
	sort.Slice(headlessOnly, func(i, j int) bool { return headlessOnly[i].State.Project < headlessOnly[j].State.Project })

	var b strings.Builder
	fmt.Fprintf(&b, "Saturday session inventory — updated %s, %d live (%d reachable now, %d headless-only).\n",
		now.UTC().Format(time.RFC3339), len(sessions), len(tmux), len(headlessOnly))
	fmt.Fprintln(&b, "Say the exact project name below to route a note to that session.")
	b.WriteString("\n")

	b.WriteString("Reachable now — live, interactive tmux pane:\n")
	writeManifestSessions(&b, tmux)
	b.WriteString("\n")

	b.WriteString("Live but no tmux pane right now — still routable, runs as a one-shot headless reply instead of live:\n")
	writeManifestSessions(&b, headlessOnly)

	return b.String()
}

func writeManifestSessions(b *strings.Builder, sessions []watcherclient.SessionEntry) {
	if len(sessions) == 0 {
		b.WriteString("  (none)\n")
		return
	}
	for _, s := range sessions {
		summary := s.State.SessionArc
		if summary == "" {
			summary = s.State.LastAssistantText
		}
		if summary == "" {
			summary = "(no summary yet)"
		}
		fmt.Fprintf(b, "  %s — %s\n", s.State.Project, oneLine(summary, 140))
	}
}

// ensureManifest finds the manifest file by name in folderID, creating it
// empty on first run. Caches the discovered/created id on d so later calls
// in the same process skip the lookup.
func (d *driveClient) ensureManifest(ctx context.Context) (string, error) {
	if d.manifestID != "" {
		return d.manifestID, nil
	}
	q := fmt.Sprintf("'%s' in parents and trashed = false and name = '%s'", d.folderID, d.manifestName)
	res, err := d.svc.Files.List().Context(ctx).Q(q).Fields("files(id)").Do()
	if err != nil {
		return "", fmt.Errorf("find manifest: %w", err)
	}
	if len(res.Files) > 0 {
		d.manifestID = res.Files[0].Id
		return d.manifestID, nil
	}
	f, err := d.svc.Files.Create(&drive.File{
		Name:     d.manifestName,
		Parents:  []string{d.folderID},
		MimeType: "text/plain",
	}).Context(ctx).Media(strings.NewReader("")).Do()
	if err != nil {
		return "", fmt.Errorf("create manifest: %w", err)
	}
	d.manifestID = f.Id
	return d.manifestID, nil
}

// refreshManifest fetches the current live-session list from the watcher
// and writes it to Drive. Called once per poll tick, independent of
// whether that tick found any new notes, so voice mode always has an
// up-to-date inventory to check against — not just after activity.
func refreshManifest(ctx context.Context, d *driveClient, sockPath string) (int, error) {
	sessions, err := watcherclient.FetchSessions(sockPath)
	if err != nil {
		return 0, fmt.Errorf("fetch sessions: %w", err)
	}
	live := make([]watcherclient.SessionEntry, 0, len(sessions))
	tmuxLive := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		if s.State.SessionID == "" {
			continue
		}
		live = append(live, s)
		if s.State.Cwd != "" && inject.FindTmuxPane(s.State.Cwd) != "" {
			tmuxLive[s.State.Cwd] = true
		}
	}
	content := buildManifestContent(live, tmuxLive, time.Now())
	if err := d.writeManifest(ctx, content); err != nil {
		return 0, fmt.Errorf("write manifest: %w", err)
	}
	return len(live), nil
}

// writeManifest overwrites the manifest file's content in place — an
// Update, not a Create, so its file id (and thus its createdTime) never
// changes after the first run. That's what keeps ListNew's exclusion-by-
// name reliable without needing to special-case createdTime races.
func (d *driveClient) writeManifest(ctx context.Context, content string) error {
	id, err := d.ensureManifest(ctx)
	if err != nil {
		return err
	}
	_, err = d.svc.Files.Update(id, &drive.File{}).Context(ctx).Media(strings.NewReader(content)).Do()
	if err != nil {
		return fmt.Errorf("update manifest: %w", err)
	}
	return nil
}
