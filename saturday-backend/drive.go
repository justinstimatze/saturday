package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// note is one Drive file in the watched folder, reduced to what the
// pipeline needs: identity for dedup, timestamp for cursor advancement, and
// its text content.
type note struct {
	ID          string
	CreatedTime time.Time
	Text        string
}

// driveSource is the polling boundary. The real implementation talks to the
// Drive API; tests use a fake — this is what keeps the route/expand/inject
// pipeline testable without live Google credentials.
type driveSource interface {
	// ListNew returns every note created after since, oldest first.
	ListNew(ctx context.Context, since time.Time) ([]note, error)
}

// googleDocMimeType is what Drive reports for a native Google Doc — those
// have no fixed byte content and must be exported, unlike an uploaded
// plain-text file which downloads directly.
const googleDocMimeType = "application/vnd.google-apps.document"

// driveClient is the real driveSource, backed by the Drive API v3 client.
type driveClient struct {
	svc      *drive.Service
	folderID string
}

// newDriveClient builds a driveClient from a cached OAuth token. tok must
// already have a refresh token (see auth.go's login flow) — this does not
// perform interactive consent.
func newDriveClient(ctx context.Context, cfg *oauth2.Config, tok *oauth2.Token, folderID string) (*driveClient, error) {
	svc, err := drive.NewService(ctx, option.WithTokenSource(cfg.TokenSource(ctx, tok)))
	if err != nil {
		return nil, fmt.Errorf("drive service: %w", err)
	}
	return &driveClient{svc: svc, folderID: folderID}, nil
}

// buildQuery constructs the Drive API query for listing notes in folderID
// created after since. Factored out as a pure function so this is
// testable without a real Drive client — see drive_test.go for the bug
// this specifically guards against.
func buildQuery(folderID string, since time.Time) string {
	query := fmt.Sprintf("'%s' in parents and trashed = false", folderID)
	// A zero-value since (fresh cursor, first run ever) would format as year
	// 0001, which the Drive API's query parser rejects outright — omit the
	// createdTime clause entirely rather than list everything since 1970.
	if !since.IsZero() {
		query += fmt.Sprintf(" and createdTime > '%s'", since.UTC().Format(time.RFC3339))
	}
	return query
}

func (d *driveClient) ListNew(ctx context.Context, since time.Time) ([]note, error) {
	call := d.svc.Files.List().
		Context(ctx).
		Q(buildQuery(d.folderID, since)).
		OrderBy("createdTime").
		Fields("files(id, name, mimeType, createdTime)")

	var notes []note
	err := call.Pages(ctx, func(page *drive.FileList) error {
		for _, f := range page.Files {
			created, err := time.Parse(time.RFC3339, f.CreatedTime)
			if err != nil {
				continue // malformed timestamp — skip rather than fail the whole page
			}
			text, err := d.readContent(ctx, f)
			if err != nil {
				return fmt.Errorf("read %s (%s): %w", f.Name, f.Id, err)
			}
			notes = append(notes, note{ID: f.Id, CreatedTime: created, Text: text})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return notes, nil
}

// isGoogleDoc reports whether mimeType is a native Google Doc — those have
// no fixed byte content and must be exported rather than downloaded.
// Factored out as a pure function so the branch is testable without a real
// Drive client.
func isGoogleDoc(mimeType string) bool {
	return mimeType == googleDocMimeType
}

// readContent branches on mimeType: a native Google Doc has no fixed byte
// content and must be exported as plain text; anything else (a plain
// uploaded .txt/.md file) downloads directly via alt=media.
func (d *driveClient) readContent(ctx context.Context, f *drive.File) (string, error) {
	if isGoogleDoc(f.MimeType) {
		r, err := d.svc.Files.Export(f.Id, "text/plain").Context(ctx).Download()
		if err != nil {
			return "", fmt.Errorf("export: %w", err)
		}
		defer r.Body.Close()
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return "", fmt.Errorf("read export body: %w", err)
		}
		return string(b), nil
	}
	r, err := d.svc.Files.Get(f.Id).Context(ctx).Download()
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	return string(b), nil
}
