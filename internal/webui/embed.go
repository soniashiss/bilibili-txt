package webui

import "embed"

// assetsFS holds the embedded Web UI shell (HTML template, minimal CSS,
// JS placeholder). The glob patterns require at least one match per
// extension, so app.css/app.js exist even before Task 13 fills them in.
//
//go:embed assets/*.html assets/*.css assets/*.js
var assetsFS embed.FS
