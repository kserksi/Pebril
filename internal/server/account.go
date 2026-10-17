package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/kserksi/Pebril/internal/auth"
	"github.com/kserksi/Pebril/internal/store"
	"github.com/kserksi/Pebril/internal/uploads"
)

const sessionCookie = "pebril_session"

func sameOrigin(c *gin.Context) bool {
	origin, err := url.Parse(c.GetHeader("Origin"))
	return err == nil && (origin.Scheme == "http" || origin.Scheme == "https") && origin.Host == c.Request.Host && origin.User == nil && origin.Path == "" && origin.RawQuery == "" && origin.Fragment == ""
}
func sessionFor(service *auth.Service, c *gin.Context) (auth.Session, error) {
	token, err := c.Cookie(sessionCookie)
	if err != nil {
		return auth.Session{}, err
	}
	return service.Session(token)
}
func requireSession(service *auth.Service, write bool, next func(auth.Session, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader("Authorization") != "" {
			c.JSON(403, gin.H{"error": "Upload tokens cannot access this endpoint"})
			return
		}
		session, err := sessionFor(service, c)
		if err != nil {
			c.JSON(401, gin.H{"error": "Sign in is required"})
			return
		}
		if write && (!sameOrigin(c) || !auth.CheckCSRF(c.GetHeader("X-CSRF-Token"), session.CSRF)) {
			c.JSON(403, gin.H{"error": "Origin or CSRF validation failed"})
			return
		}
		next(session, c)
	}
}
func accountRoutes(router *gin.Engine, service *auth.Service, upload *uploads.Service) {
	router.GET("/api/v1/account", func(c *gin.Context) {
		if c.GetHeader("Authorization") != "" {
			c.JSON(403, gin.H{"error": "Upload tokens cannot access account data"})
			return
		}
		initialized, err := service.Initialized()
		if err != nil {
			c.JSON(500, gin.H{"error": "Cannot read initialization status"})
			return
		}
		session, err := sessionFor(service, c)
		if err != nil {
			c.JSON(200, gin.H{"initialized": initialized})
			return
		}
		value := gin.H{"initialized": initialized, "user": session.User, "csrf": session.CSRF}
		if session.User.Admin && upload != nil && upload.Resources != nil {
			value["resources"] = upload.Resources.State()
		}
		c.JSON(200, value)
	})
	for _, path := range []string{"initialize", "login", "register"} {
		path := path
		router.POST("/api/v1/"+path, func(c *gin.Context) {
			if c.GetHeader("Authorization") != "" || !sameOrigin(c) {
				c.JSON(403, gin.H{"error": "Use the official same-origin web flow"})
				return
			}
			var credentials struct {
				Username string `json:"username"`
				Password string `json:"password"`
			}
			if !accountBody(c, map[string]any{"username": &credentials.Username, "password": &credentials.Password}, true) {
				c.JSON(400, gin.H{"error": "Invalid account request"})
				return
			}
			var session auth.Session
			var err error
			if path == "initialize" {
				_, err = service.Initialize(credentials.Username, credentials.Password)
			} else if path == "register" {
				_, err = service.Register(credentials.Username, credentials.Password)
			}
			if err == nil {
				session, err = service.Login(credentials.Username, credentials.Password)
			}
			if err != nil {
				if errors.Is(err, auth.ErrBanned) {
					c.JSON(403, gin.H{"error": err.Error(), "code": "account_banned"})
					return
				}
				status, message := 500, "Account operation failed"
				if errors.Is(err, auth.ErrInput) {
					status, message = 400, err.Error()
				}
				if errors.Is(err, auth.ErrInitialized) || errors.Is(err, auth.ErrNotInitialized) || errors.Is(err, auth.ErrUsernameTaken) {
					status, message = 409, err.Error()
				}
				if errors.Is(err, auth.ErrCredentials) {
					status, message = 401, err.Error()
				}
				c.JSON(status, gin.H{"error": message})
				return
			}
			origin, _ := url.Parse(c.GetHeader("Origin"))
			http.SetCookie(c.Writer, &http.Cookie{Name: sessionCookie, Value: session.Token, Path: "/", Expires: session.Expires, MaxAge: int(auth.SessionLifetime.Seconds()), HttpOnly: true, Secure: c.Request.TLS != nil || origin.Scheme == "https", SameSite: http.SameSiteStrictMode})
			value := gin.H{"user": session.User, "csrf": session.CSRF}
			if session.User.Admin && upload != nil && upload.Resources != nil {
				value["resource_warnings"] = upload.Resources.State().Warnings
			}
			c.JSON(200, value)
		})
	}
	router.POST("/api/v1/logout", requireSession(service, true, func(_ auth.Session, c *gin.Context) {
		token, _ := c.Cookie(sessionCookie)
		if err := service.Logout(token); err != nil {
			c.JSON(500, gin.H{"error": "Cannot revoke session"})
			return
		}
		http.SetCookie(c.Writer, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		c.JSON(200, gin.H{"signed_out": true})
	}))
	router.POST("/api/v1/upload-token", requireSession(service, true, func(session auth.Session, c *gin.Context) {
		token, err := service.IssueToken(session)
		if err != nil {
			if errors.Is(err, auth.ErrCredentials) {
				c.JSON(401, gin.H{"error": "Sign in is required"})
				return
			}
			c.JSON(500, gin.H{"error": "Cannot issue upload token"})
			return
		}
		c.JSON(201, gin.H{"token": token})
	}))
	router.DELETE("/api/v1/upload-token", requireSession(service, true, func(session auth.Session, c *gin.Context) {
		if err := service.RevokeToken(session); err != nil {
			if errors.Is(err, auth.ErrCredentials) {
				c.JSON(401, gin.H{"error": "Sign in is required"})
				return
			}
			c.JSON(500, gin.H{"error": "Cannot revoke upload token"})
			return
		}
		c.JSON(200, gin.H{"revoked": true})
	}))
	administrationRoutes(router, service)
	if upload == nil {
		return
	}
	galleryRoutes(router, service, upload)
	router.POST("/api/v1/uploads", func(c *gin.Context) {
		var user store.User
		var err error
		authorization := c.GetHeader("Authorization")
		if authorization != "" {
			if !strings.HasPrefix(authorization, "Bearer ") {
				c.JSON(401, gin.H{"error": "A valid upload token is required"})
				return
			}
			user, err = service.UploadUser(strings.TrimPrefix(authorization, "Bearer "))
		} else {
			var session auth.Session
			session, err = sessionFor(service, c)
			if err == nil {
				if !sameOrigin(c) || !auth.CheckCSRF(c.GetHeader("X-CSRF-Token"), session.CSRF) {
					c.JSON(403, gin.H{"error": "Origin or CSRF validation failed"})
					return
				}
				user = session.User
			}
		}
		if err != nil {
			c.JSON(401, gin.H{"error": "Upload authentication failed"})
			return
		}
		uploadHandler(upload, user, c)
	})
	router.GET("/api/v1/uploads/:id", requireSession(service, false, func(session auth.Session, c *gin.Context) {
		image, err := upload.Result(c.Param("id"), session.User.ID)
		if err != nil {
			c.JSON(404, gin.H{"error": "Upload not found"})
			return
		}
		c.JSON(200, image)
	}))
	router.POST("/api/v1/uploads/:id/retry", requireSession(service, true, func(session auth.Session, c *gin.Context) {
		if err := upload.Retry(c.Param("id"), session.User.ID); err != nil {
			status := 400
			if uploads.IsCapacity(err) {
				status = 429
			}
			c.JSON(status, gin.H{"error": "Cannot retry this image"})
			return
		}
		image, err := upload.Result(c.Param("id"), session.User.ID)
		if err != nil {
			c.Status(500)
			return
		}
		c.JSON(202, image)
	}))
	router.GET("/image/:id", func(c *gin.Context) { publicImage(upload, false, c) })
	router.GET("/image/:id/cover", func(c *gin.Context) { publicImage(upload, true, c) })
}

// accountBody enforces exact JSON field names and rejects ambiguous duplicate/null input.
func accountBody(c *gin.Context, fields map[string]any, requireAll bool) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8192)
	decoder := json.NewDecoder(c.Request.Body)
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	seen := make(map[string]bool, len(fields))
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return false
		}
		key, ok := token.(string)
		target, allowed := fields[key]
		if !ok || !allowed || seen[key] {
			return false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || string(value) == "null" || json.Unmarshal(value, target) != nil {
			return false
		}
		seen[key] = true
	}
	token, err = decoder.Token()
	return err == nil && token == json.Delim('}') && decoder.Decode(new(any)) == io.EOF && len(seen) > 0 && (!requireAll || len(seen) == len(fields))
}
