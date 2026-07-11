// Package web embeds the search UI served by internal/api: a Go
// html/template for the page shell, and the static CSS/JS/favicon it
// references. Kept as its own package because go:embed patterns cannot
// reach outside the directory of the file that declares them.
package web

import "embed"

//go:embed templates/*.html
var TemplatesFS embed.FS

//go:embed static/*
var StaticFS embed.FS
