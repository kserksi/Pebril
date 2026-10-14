package server

import (
	"embed"
	"github.com/gin-gonic/gin"
	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/nativeprobe"
	"github.com/kserksi/Pebril/internal/store"
	"io/fs"
	"net/http"
)

//go:embed web/*
var assets embed.FS

func Handler(version string, manager *config.Manager, database *store.Store, capability nativeprobe.Result) http.Handler {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		c.Next()
	})
	router.GET("/healthz", func(c *gin.Context) {
		if !capability.Ready || database.Ping() != nil {
			c.JSON(503, gin.H{"ready": false})
			return
		}
		c.JSON(200, gin.H{"ready": true})
	})
	router.GET("/api/v1/status", func(c *gin.Context) {
		c.JSON(200, gin.H{"name": "Pebril", "version": version, "stage": "foundation", "config_revision": manager.Snapshot().Revision, "sqlite": database.Ping() == nil, "native": gin.H{"ready": capability.Ready, "libvips_version": capability.VipsVersion, "svg": capability.SVG, "gif": capability.GIF, "avif_enabled": capability.AVIF}})
	})
	files, _ := fs.Sub(assets, "web")
	for _, name := range []string{"index.html", "app.js", "style.css"} {
		name := name
		route := "/" + name
		if name == "index.html" {
			route = "/"
		}
		router.GET(route, func(c *gin.Context) {
			data, err := fs.ReadFile(files, name)
			if err != nil {
				c.Status(500)
				return
			}
			kind := "text/html; charset=utf-8"
			if name == "app.js" {
				kind = "text/javascript; charset=utf-8"
			}
			if name == "style.css" {
				kind = "text/css; charset=utf-8"
			}
			c.Data(200, kind, data)
		})
	}
	return router
}
