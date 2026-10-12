package webui

import (
	"embed"
	"io/fs"
)

//go:embed static/*
var content embed.FS

func Files() fs.FS { files, _ := fs.Sub(content, "static"); return files }
