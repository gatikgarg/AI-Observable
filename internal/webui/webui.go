// Package webui embeds the offline API Manager shell. There is no CDN and no
// frontend build step: the binary carries the assets it serves.
package webui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"path"
)

//go:embed assets
var files embed.FS

// Index is the shell template. It is parsed by the admin package so the CSRF
// token and URLs are filled server-side.
const Index = "assets/index.html"

// Asset returns an embedded asset and its content hash. The hash is what the
// shell puts in the URL, so `Cache-Control: immutable` stays honest.
func Asset(name string) (body []byte, sum string, ok bool) {
	name = path.Clean("/" + name)[1:]
	switch name {
	case "index.html", "":
		return nil, "", false
	}
	b, err := files.ReadFile("assets/" + name)
	if err != nil {
		return nil, "", false
	}
	h := sha256.Sum256(b)
	return b, hex.EncodeToString(h[:8]), true
}

// Shell returns the raw shell template bytes.
func Shell() []byte {
	b, err := files.ReadFile(Index)
	if err != nil {
		return []byte("<!doctype html><title>AI-Observable API Manager</title>")
	}
	return b
}
