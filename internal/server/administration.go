package server

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/kserksi/Pebril/internal/auth"
	"github.com/kserksi/Pebril/internal/store"
)

type administrationQuota int64

func (quota *administrationQuota) UnmarshalJSON(data []byte) error {
	value := string(data)
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != value {
		return store.ErrAccountInput
	}
	*quota = administrationQuota(n)
	return nil
}

func administrationUser(user store.User) gin.H {
	return gin.H{"id": user.ID, "username": user.Username, "admin": user.Admin, "status": user.Status, "quota": strconv.FormatInt(user.Quota, 10), "used": strconv.FormatInt(user.Used, 10)}
}

func requireAdministrator(service *auth.Service, write bool, next func(auth.Session, *gin.Context)) gin.HandlerFunc {
	return requireSession(service, write, func(session auth.Session, c *gin.Context) {
		if !session.User.Admin {
			c.JSON(403, gin.H{"error": "Administrator access is required"})
			return
		}
		next(session, c)
	})
}

func administrationError(c *gin.Context, err error) {
	status, message := 500, "Account operation failed"
	switch {
	case errors.Is(err, store.ErrAccountInput), errors.Is(err, auth.ErrInput):
		status, message = 400, "Invalid account request"
	case errors.Is(err, store.ErrAccountPermission):
		status, message = 403, "This account cannot be changed"
	case errors.Is(err, store.ErrAccountNotFound):
		status, message = 404, "Account not found"
	}
	c.JSON(status, gin.H{"error": message})
}

func administrationTarget(c *gin.Context) (int64, bool) {
	value := c.Param("id")
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != value {
		c.JSON(400, gin.H{"error": "Invalid account ID"})
		return 0, false
	}
	return id, true
}

func administrationRoutes(router *gin.Engine, service *auth.Service) {
	router.GET("/api/v1/admin/users", requireAdministrator(service, false, func(_ auth.Session, c *gin.Context) {
		users, err := service.DB.Users()
		if err != nil {
			administrationError(c, err)
			return
		}
		values := make([]gin.H, 0, len(users))
		for _, user := range users {
			values = append(values, administrationUser(user))
		}
		c.JSON(200, gin.H{"users": values})
	}))
	router.PATCH("/api/v1/admin/users/:id", requireAdministrator(service, true, func(session auth.Session, c *gin.Context) {
		id, ok := administrationTarget(c)
		if !ok {
			return
		}
		var status *string
		var quota *administrationQuota
		if !accountBody(c, map[string]any{"status": &status, "quota": &quota}, false) {
			c.JSON(400, gin.H{"error": "Invalid account request"})
			return
		}
		user, err := service.DB.UpdateUser(session.User.ID, id, status, (*int64)(quota))
		if err != nil {
			administrationError(c, err)
			return
		}
		c.JSON(200, gin.H{"user": administrationUser(user)})
	}))
	router.POST("/api/v1/admin/users/:id/password", requireAdministrator(service, true, func(session auth.Session, c *gin.Context) {
		id, ok := administrationTarget(c)
		if !ok {
			return
		}
		var password string
		if !accountBody(c, map[string]any{"password": &password}, true) {
			c.JSON(400, gin.H{"error": "Invalid account request"})
			return
		}
		user, err := service.ResetPassword(session.User.ID, id, password)
		if err != nil {
			administrationError(c, err)
			return
		}
		c.JSON(200, gin.H{"user": administrationUser(user)})
	}))
}
