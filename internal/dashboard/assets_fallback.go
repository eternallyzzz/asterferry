//go:build !dashboard_assets

package dashboard

import "embed"

// dashboardAssets is a source-level fallback used by ordinary Go tests and
// builds. It keeps the repository buildable without checking in generated
// Dashboard assets.
//
//go:embed fallback/*
var dashboardAssets embed.FS

const dashboardAssetRoot = "fallback"
