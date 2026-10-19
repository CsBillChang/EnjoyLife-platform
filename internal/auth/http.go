package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const actorContextKey = "authenticated_actor"

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

type loginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

func (h *Handler) Login(c *gin.Context) {
	var request loginRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "INVALID_REQUEST", "message": "username and password are required"}})
		return
	}
	token, expiresAt, err := h.service.Login(c.Request.Context(), request.Username, request.Password)
	if errors.Is(err, ErrInvalidCredentials) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "INVALID_CREDENTIALS", "message": "invalid username or password"}})
		return
	}
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "AUTH_UNAVAILABLE", "message": "authentication service unavailable"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"access_token": token, "token_type": "Bearer", "expires_at": expiresAt}})
}

func (h *Handler) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		parts := strings.SplitN(header, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "UNAUTHORIZED", "message": "valid bearer token required"}})
			return
		}
		actor, err := h.service.Authenticate(c.Request.Context(), strings.TrimSpace(parts[1]))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "UNAUTHORIZED", "message": "valid bearer token required"}})
			return
		}
		c.Set(actorContextKey, actor)
		c.Next()
	}
}

func CurrentActor(c *gin.Context) (Actor, bool) {
	value, ok := c.Get(actorContextKey)
	if !ok {
		return Actor{}, false
	}
	actor, ok := value.(Actor)
	return actor, ok
}
