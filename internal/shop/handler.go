package shop

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// Handler adapts the shop service to the public HTTP contract.
type Handler struct {
	service *Service
}

// shopDetailResponse avoids the reflection and key-sorting work of a dynamic
// map on the hottest public detail path while preserving the JSON contract.
type shopDetailResponse struct {
	Data Shop `json:"data"`
}

// NewHandler creates a handler for shop endpoints.
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterRoutes installs the read-only shop endpoints.
func (h *Handler) RegisterRoutes(router gin.IRouter) {
	router.GET("/shops", h.list)
	router.GET("/shops/:id", h.getByID)
}

func (h *Handler) list(c *gin.Context) {
	page, err := optionalPositiveInt(c.Query("page"), 1)
	if err != nil {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", ErrInvalidPagination.Error())
		return
	}
	pageSize, err := optionalPositiveInt(c.Query("page_size"), 0)
	if err != nil {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", ErrInvalidPagination.Error())
		return
	}

	result, err := h.service.List(c.Request.Context(), page, pageSize)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": result})
}

func (h *Handler) getByID(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", ErrInvalidShopID.Error())
		return
	}

	result, err := h.service.GetByID(c.Request.Context(), id)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, shopDetailResponse{Data: result})
}

func optionalPositiveInt(raw string, fallback int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 0, ErrInvalidPagination
	}
	return value, nil
}

func (h *Handler) writeServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrInvalidShopID), errors.Is(err, ErrInvalidPagination):
		h.writeError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	case errors.Is(err, ErrShopNotFound):
		h.writeError(c, http.StatusNotFound, "SHOP_NOT_FOUND", err.Error())
	default:
		h.writeError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "shop query failed")
	}
}

func (h *Handler) writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
