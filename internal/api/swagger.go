package api

import (
	_ "embed"
	"net/http"
)

//go:embed docs/openapi.yaml
var openAPISpecBytes []byte

// openAPISpecHandler serves the raw OpenAPI YAML spec.
// The spec is embedded into the binary at compile time so it is always
// available in production without any file-system dependency.
func openAPISpecHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openAPISpecBytes)
}

// swaggerUIHandler serves a self-contained Swagger UI page.
//
// All UI assets are loaded from the official unpkg CDN (swagger-ui-dist).
// The spec URL is a relative path (/docs/openapi.yaml) so it resolves
// correctly on any deployment host — no hardcoded localhost references.
func swaggerUIHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(swaggerUIHTML))
}

// swaggerUIHTML is the full HTML page for Swagger UI.
// Key production decisions:
//   - CDN assets: no build step, no npm; always uses the latest 5.x patch.
//   - url: "/docs/openapi.yaml" — relative path, works on any hostname/port.
//   - deepLinking: true — browser back/forward works with operation anchors.
//   - tryItOutEnabled: true — "Try it out" is open by default.
//   - persistAuthorization: true — auth tokens survive page refresh.
const swaggerUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0" />
  <title>Mutual Fund Analytics API — Docs</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css" />
  <style>
    /* ── minimal overrides — keep the default Swagger UI look ── */
    body { margin: 0; }
    .swagger-ui .topbar { background-color: #1a1a2e; }
    .swagger-ui .topbar .download-url-wrapper { display: none; }
    .swagger-ui .topbar-wrapper .link { pointer-events: none; }
  </style>
</head>
<body>
  <div id="swagger-ui"></div>

  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-standalone-preset.js"></script>
  <script>
    window.onload = function () {
      SwaggerUIBundle({
        url: "/docs/openapi.yaml",
        dom_id: "#swagger-ui",
        presets: [
          SwaggerUIBundle.presets.apis,
          SwaggerUIStandalonePreset
        ],
        layout: "StandaloneLayout",
        deepLinking: true,
        tryItOutEnabled: true,
        persistAuthorization: true,
        displayRequestDuration: true,
        defaultModelsExpandDepth: 2,
        defaultModelExpandDepth: 2,
        docExpansion: "list",
        filter: true,
        showExtensions: true,
        showCommonExtensions: true,
      });
    };
  </script>
</body>
</html>`
