package server

import "embed"

// assets embeds the HTML templates and static front-end files so the resulting
// binary is fully self-contained (no external asset files needed at runtime).
//
//go:embed templates/*.html static/*
var assets embed.FS
