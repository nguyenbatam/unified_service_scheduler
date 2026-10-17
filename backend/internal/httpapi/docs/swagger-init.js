// Served from /api/v1/docs/swagger-init.js so the docs page needs no inline script under its CSP.
window.ui = SwaggerUIBundle({
  url: "/api/v1/openapi.yaml",
  dom_id: "#swagger-ui",
  deepLinking: true,
  presets: [SwaggerUIBundle.presets.apis],
  layout: "BaseLayout",
  tryItOutEnabled: true,
  persistAuthorization: true,
  validatorUrl: null,
});
