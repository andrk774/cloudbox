// Package web встраивает статический веб-интерфейс в бинарник.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var content embed.FS

func FS() fs.FS {
	sub, err := fs.Sub(content, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
