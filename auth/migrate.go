package auth

import (
	"context"
	"fmt"
	"log/slog"
)

// MetaAdapter is the slice of build.Service this package needs to migrate
// pre-multi-tenancy apps. Defined here so internal/auth doesn't pick up
// a transitive dependency on internal/build; main.go binds a real
// *build.Service to it at startup.
type MetaAdapter interface {
	ReadOwnerID(ctx context.Context, slug string) (current string, exists bool)
	SetOwnerID(ctx context.Context, slug, ownerID string) error
}

// AppLister returns the list of slugs that should be considered for
// ownership migration. Wired to store.ListApps in main.go.
type AppLister interface {
	ListApps(ctx context.Context) ([]string, error)
}

// MigrateOwnership assigns the configured super admin as the OwnerID of
// every app whose SiteMeta has none. Idempotent: re-running over an
// already-migrated bucket logs `assigned=0`. Designed to run on every
// startup so a fresh deploy against an existing bucket converges without
// operator intervention.
func (a *Auth) MigrateOwnership(ctx context.Context, lister AppLister, meta MetaAdapter) error {
	apps, err := lister.ListApps(ctx)
	if err != nil {
		return fmt.Errorf("auth.migrate: list apps: %w", err)
	}
	owner := a.cfg.SuperAdminEmail
	assigned := 0
	skipped := 0
	for _, slug := range apps {
		current, exists := meta.ReadOwnerID(ctx, slug)
		if exists && current != "" {
			skipped++
			continue
		}
		err = meta.SetOwnerID(ctx, slug, owner)
		if err != nil {
			return fmt.Errorf("auth.migrate: set owner for %s: %w", slug, err)
		}
		assigned++
	}
	slog.Info("auth.migrate.app_ownership", "assigned", assigned, "skipped", skipped, "owner", owner)
	return nil
}
