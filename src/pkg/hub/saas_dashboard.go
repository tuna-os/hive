package hub

import _ "embed"

// dashboardHTML is kept as a source asset so frontend tooling can inspect it
// without parsing a Go raw string. Embedding preserves the single-binary
// deployment contract.
//
//go:embed static/saas_dashboard.html
var dashboardHTML string
