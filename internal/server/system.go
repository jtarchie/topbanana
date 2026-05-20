package server

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/bloomhollow/internal/editrec"
	"github.com/jtarchie/bloomhollow/internal/model"
)

// recentBuildsWindow is how many of the most recent transcripts across all
// apps the system dashboard reads for the "Recent builds" table and the
// "X of N successful" stat card. Reading every transcript would scale poorly
// — for an operator dashboard, the last 20 is plenty of signal.
const recentBuildsWindow = 20

// systemAppRow is one row in the per-app inventory table.
type systemAppRow struct {
	Slug       string
	Title      string
	Bytes      int64
	SizeLabel  string
	Edits      int
	LastEdited string
	URL        string // workspace link
}

// systemBuildRow is one row in the recent-builds table.
type systemBuildRow struct {
	Slug          string
	LogKey        string
	WhenLabel     string
	WhenISO       string
	Status        string // "successful" | "failed" | "in-progress"
	StatusLabel   string // user-facing copy for the badge
	DurationLabel string
	DebugURL      string
}

// systemStorageRow is one row in the storage breakdown.
type systemStorageRow struct {
	Label      string
	Bytes      int64
	BytesLabel string
	Count      int
}

// systemStorage is the structured breakdown the template renders.
type systemStorage struct {
	Rows            []systemStorageRow
	TotalBytes      int64
	TotalBytesLabel string
	TotalCount      int
}

type systemData struct {
	Chrome
	Flash string

	// At-a-glance
	AppCount      int
	CustomDomains int
	StorageBytes  int64
	StorageLabel  string
	StorageCount  int
	RecentTotal   int
	RecentSuccess int
	LastActivity  string

	// Sections
	Apps    []systemAppRow
	Builds  []systemBuildRow
	Storage systemStorage
	Config  systemConfig
}

// systemConfig is the read-only configuration block. Mirrors SystemInfo with
// formatted-for-display fields (e.g., snapshot retention "off" instead of "0").
//
// LLMAuthor / LLMEditor / LLMUtility / LLMVision are pre-resolved through
// the tier fallback chain so the dashboard reads naturally — every line
// shows the model that actually runs for that tier, not a confusing empty
// "(uses author)" marker.
type systemConfig struct {
	LLMAuthor          string
	LLMEditor          string
	LLMUtility         string
	LLMVision          string
	LLMBaseURL         string
	LLMReasoningEffort string
	S3Endpoint         string
	S3Bucket           string
	SnapshotKeep       string
	EditsKeep          string
	Domain             string
	CustomDomainCount  int
}

// slugListing pairs an editrec.Listing with its slug so the recent-builds
// flattening step can carry the per-app context through the sort.
type slugListing struct {
	Slug string
	editrec.Listing
}

// mostRecentListings flattens per-slug listings, sorts by timestamp
// descending, and slices to the top n. Pure — exercised directly in
// system_test.go to lock in the sort + slice contract.
func mostRecentListings(perApp map[string][]editrec.Listing, n int) []slugListing {
	flat := make([]slugListing, 0)
	for slug, list := range perApp {
		for _, l := range list {
			flat = append(flat, slugListing{Slug: slug, Listing: l})
		}
	}
	sort.SliceStable(flat, func(i, j int) bool {
		return flat[i].Timestamp.After(flat[j].Timestamp)
	})
	if n > 0 && len(flat) > n {
		flat = flat[:n]
	}
	return flat
}

// summarizeBuilds buckets transcripts by status. Pure — no I/O. FinishedAt
// is the in-flight signal because some run paths can return without
// stamping FinalStatus (crashes); we treat anything missing FinishedAt as
// still running rather than mark it failed.
func summarizeBuilds(transcripts []editrec.Transcript) (successful, failed, inFlight int) {
	for _, t := range transcripts {
		if t.FinishedAt.IsZero() {
			inFlight++
			continue
		}
		switch t.FinalStatus {
		case "completed":
			successful++
		case "failed":
			failed++
		default:
			// Empty FinalStatus with a FinishedAt set shouldn't happen, but
			// if it does count it as failed so the success-rate stat doesn't
			// silently understate problems.
			failed++
		}
	}
	return
}

// storageBreakdownEntry is the input to aggregateStorage: one labeled
// (bytes, count) pair per category.
type storageBreakdownEntry struct {
	Label string
	Bytes int64
	Count int
}

// aggregateStorage builds the storage breakdown table from a fixed-order
// list of category entries. Pure. Total is computed in one pass rather than
// expecting callers to pre-sum.
func aggregateStorage(entries []storageBreakdownEntry) systemStorage {
	var totalBytes int64
	totalCount := 0
	rows := make([]systemStorageRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, systemStorageRow{
			Label:      e.Label,
			Bytes:      e.Bytes,
			BytesLabel: humanizeBytes(e.Bytes),
			Count:      e.Count,
		})
		totalBytes += e.Bytes
		totalCount += e.Count
	}
	return systemStorage{
		Rows:            rows,
		TotalBytes:      totalBytes,
		TotalBytesLabel: humanizeBytes(totalBytes),
		TotalCount:      totalCount,
	}
}

// reservedPrefixes is the fixed list of non-slug bucket prefixes the system
// dashboard sums independently of per-app file walks.
var reservedPrefixes = []struct {
	Label  string
	Prefix string
}{
	{Label: "Snapshots", Prefix: "_snapshots/"},
	{Label: "Build transcripts", Prefix: "_edits/"},
	{Label: "TLS certs (ACME)", Prefix: "_acme/"},
	{Label: "Form data", Prefix: "_state/"},
}

func (s *Server) systemHandler(c *echo.Context) error {
	ctx := c.Request().Context()

	apps, err := s.store.ListApps(ctx)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list apps", err)
	}

	appRows, perAppBytes, perAppCount, transcriptsByApp, customDomains, lastActivity := s.collectAppRows(ctx, apps)

	// Recent builds — flatten + sort + slice + read the top window.
	recent := mostRecentListings(transcriptsByApp, recentBuildsWindow)
	recentTranscripts := make([]editrec.Transcript, 0, len(recent))
	buildRows := make([]systemBuildRow, 0, len(recent))
	for _, r := range recent {
		t, readErr := editrec.Read(ctx, s.store, r.Key)
		if readErr != nil {
			slog.Warn("system.transcript_read", "key", r.Key, "err", readErr)
			continue
		}
		recentTranscripts = append(recentTranscripts, t)
		buildRows = append(buildRows, makeBuildRow(r, t))
	}
	successful, _, _ := summarizeBuilds(recentTranscripts)

	// Storage breakdown.
	entries := []storageBreakdownEntry{{Label: "Apps (live files)", Bytes: perAppBytes, Count: perAppCount}}
	for _, p := range reservedPrefixes {
		b, n, sumErr := s.store.SumBytesUnderPrefix(ctx, p.Prefix)
		if sumErr != nil {
			slog.Warn("system.prefix_sum", "prefix", p.Prefix, "err", sumErr)
		}
		entries = append(entries, storageBreakdownEntry{Label: p.Label, Bytes: b, Count: n})
	}
	storage := aggregateStorage(entries)

	// At-a-glance numbers fold in everything above.
	last := "Never"
	if !lastActivity.IsZero() {
		last = humanizeAge(lastActivity)
	}

	return s.render(c, "system", systemData{
		Chrome:        Chrome{Active: "system"},
		Flash:         c.QueryParam("flash"),
		AppCount:      len(appRows),
		CustomDomains: customDomains,
		StorageBytes:  storage.TotalBytes,
		StorageLabel:  storage.TotalBytesLabel,
		StorageCount:  storage.TotalCount,
		RecentTotal:   len(recentTranscripts),
		RecentSuccess: successful,
		LastActivity:  last,
		Apps:          appRows,
		Builds:        buildRows,
		Storage:       storage,
		Config:        s.systemConfig(customDomains),
	})
}

// collectAppRows walks ListApps and, for each, gathers size + edit count +
// most recent transcript timestamp. Returns the rows sorted by descending
// size plus aggregate counters used elsewhere on the page.
func (s *Server) collectAppRows(ctx context.Context, apps []string) (
	rows []systemAppRow,
	totalBytes int64,
	totalCount int,
	transcriptsByApp map[string][]editrec.Listing,
	customDomains int,
	lastActivity time.Time,
) {
	rows = make([]systemAppRow, 0, len(apps))
	transcriptsByApp = make(map[string][]editrec.Listing, len(apps))
	for _, slug := range apps {
		files, err := s.store.ListWithMeta(ctx, slug)
		if err != nil {
			slog.Warn("system.list_app", "slug", slug, "err", err)
			continue
		}
		var slugBytes int64
		for _, f := range files {
			slugBytes += f.Size
		}
		totalBytes += slugBytes
		totalCount += len(files)

		meta := s.build.ReadMeta(ctx, slug)
		customDomains += len(meta.Domains)

		listings, listErr := editrec.List(ctx, s.store, slug)
		if listErr != nil {
			slog.Warn("system.list_edits", "slug", slug, "err", listErr)
		} else {
			transcriptsByApp[slug] = listings
			if len(listings) > 0 && listings[0].Timestamp.After(lastActivity) {
				lastActivity = listings[0].Timestamp
			}
		}

		row := systemAppRow{
			Slug:      slug,
			Title:     meta.Title,
			Bytes:     slugBytes,
			SizeLabel: humanizeBytes(slugBytes),
			Edits:     len(listings),
			URL:       "/workspace/" + slug,
		}
		if len(listings) > 0 {
			row.LastEdited = humanizeAge(listings[0].Timestamp)
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Bytes > rows[j].Bytes })
	return rows, totalBytes, totalCount, transcriptsByApp, customDomains, lastActivity
}

// makeBuildRow formats one (listing, transcript) pair for the recent-builds
// table. Status maps editrec's FinalStatus into the three buckets the badge
// uses; duration falls back to "—" when FinishedAt is unset.
func makeBuildRow(l slugListing, t editrec.Transcript) systemBuildRow {
	status, label := "in-progress", "in progress"
	if !t.FinishedAt.IsZero() {
		switch t.FinalStatus {
		case "completed":
			status, label = "successful", "ok"
		case "failed":
			status, label = "failed", "failed"
		default:
			status, label = "failed", "unknown"
		}
	}
	duration := "—"
	if !t.FinishedAt.IsZero() && !t.StartedAt.IsZero() {
		duration = t.FinishedAt.Sub(t.StartedAt).Round(time.Second).String()
	}
	return systemBuildRow{
		Slug:          l.Slug,
		LogKey:        l.LogKey,
		WhenLabel:     humanizeAge(l.Timestamp),
		WhenISO:       l.Timestamp.Format(time.RFC3339),
		Status:        status,
		StatusLabel:   label,
		DurationLabel: duration,
		DebugURL:      "/debug/" + l.Slug + "/edit?key=" + l.Key,
	}
}

// systemConfig formats SystemInfo for display. The retention fields show
// "off" when 0 because 0 disables retention in build.Service / snapshot.Service.
func (s *Server) systemConfig(customDomains int) systemConfig {
	tiers := s.systemInfo.LLMTiers
	cfg := systemConfig{
		LLMAuthor:          tiers.Resolve(model.TierAuthor),
		LLMEditor:          tiers.Resolve(model.TierEditor),
		LLMUtility:         tiers.Resolve(model.TierUtility),
		LLMVision:          tiers.Resolve(model.TierVision),
		LLMBaseURL:         s.systemInfo.LLMBaseURL,
		LLMReasoningEffort: s.systemInfo.LLMReasoningEffort,
		S3Endpoint:         s.systemInfo.S3Endpoint,
		S3Bucket:           s.systemInfo.S3Bucket,
		Domain:             s.domain,
		CustomDomainCount:  customDomains,
	}
	cfg.SnapshotKeep = retentionLabel(s.systemInfo.SnapshotKeep)
	cfg.EditsKeep = retentionLabel(s.systemInfo.EditsKeep)
	if cfg.LLMBaseURL == "" {
		cfg.LLMBaseURL = "(provider default)"
	}
	if cfg.LLMReasoningEffort == "" {
		cfg.LLMReasoningEffort = "off"
	}
	if cfg.S3Endpoint == "" {
		cfg.S3Endpoint = "(AWS S3)"
	}
	return cfg
}

func retentionLabel(keep int) string {
	if keep <= 0 {
		return "unlimited"
	}
	return "keep " + strconv.Itoa(keep)
}
