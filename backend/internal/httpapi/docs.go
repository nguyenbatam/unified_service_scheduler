package httpapi

import (
	"embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed docs/openapi.yaml docs/swagger.html docs/swagger-init.js
var apiDocs embed.FS

// docsCSP locks the Swagger UI page down: it shares the SPA's origin, where access tokens live
// in localStorage, so only our own files and the SRI-pinned swagger-ui-dist assets on unpkg may
// run. Inline styles stay allowed (Swagger UI renders some through style attributes); scripts
// never are (the init code is the embedded swagger-init.js).
// swaggerUIDist is the only unpkg path the docs page may load from (the exact version swagger.html
// pins with SRI), not every package on unpkg.
const swaggerUIDist = "https://unpkg.com/swagger-ui-dist@5.27.1/"

const docsCSP = "default-src 'none'; script-src 'self' " + swaggerUIDist + "; " +
	"style-src 'self' " + swaggerUIDist + " 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; " +
	"connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'"

func (s *Server) openapiYAML(c *gin.Context) {
	s.serveDoc(c, "docs/openapi.yaml", "application/yaml; charset=utf-8")
}

func (s *Server) swaggerUI(c *gin.Context) {
	s.serveDoc(c, "docs/swagger.html", "text/html; charset=utf-8")
}

func (s *Server) swaggerInit(c *gin.Context) {
	s.serveDoc(c, "docs/swagger-init.js", "text/javascript; charset=utf-8")
}

func (s *Server) serveDoc(c *gin.Context, name, contentType string) {
	raw, err := apiDocs.ReadFile(name)
	if err != nil {
		c.String(http.StatusInternalServerError, name+" missing")
		return
	}
	c.Header("Content-Security-Policy", docsCSP)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Referrer-Policy", "no-referrer")
	c.Data(http.StatusOK, contentType, raw)
}
