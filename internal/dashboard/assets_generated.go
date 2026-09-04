//go:build dashboard_assets

package dashboard

import "embed"

// dashboardAssets is populated by npm run build before a release build.
// internal/dashboard/dist is intentionally ignored by Git.
//
//go:embed dist/*
var dashboardAssets embed.FS

const dashboardAssetRoot = "dist"
