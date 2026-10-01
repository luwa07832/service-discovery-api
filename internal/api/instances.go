package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/service-discovery-api/internal/store"
)

// instanceDTO is the public JSON shape of one current instance record.
type instanceDTO struct {
	ServiceName string  `json:"service_name"`
	InstanceID  string  `json:"instance_id"`
	Address     string  `json:"address"`
	Healthy     bool    `json:"healthy"`
	Weight      float64 `json:"weight"`
	HeartbeatAt string  `json:"heartbeat_at"`
}

// upsertRequest carries the values of a register/update call. Weight and heartbeat are
// pointers so missing values stay distinguishable from zero values.
type upsertRequest struct {
	ServiceName *string  `json:"service_name"`
	InstanceID  *string  `json:"instance_id"`
	Address     *string  `json:"address"`
	Healthy     bool     `json:"healthy"`
	Weight      *float64 `json:"weight"`
	HeartbeatAt *string  `json:"heartbeat_at"`
}

func toDTO(inst store.Instance) instanceDTO {
	return instanceDTO{
		ServiceName: inst.ServiceName,
		InstanceID:  inst.InstanceID,
		Address:     inst.Address,
		Healthy:     inst.Healthy,
		Weight:      inst.Weight,
		HeartbeatAt: inst.HeartbeatAt.Format(time.RFC3339Nano),
	}
}

func toDTOs(instances []store.Instance) []instanceDTO {
	out := make([]instanceDTO, 0, len(instances))
	for _, inst := range instances {
		out = append(out, toDTO(inst))
	}
	return out
}

func registerInstanceRoutes(router *gin.Engine, st *store.Store) {
	// Flat entries let callers supply every value, including empty ones, through JSON or
	// query strings so the published parameter rules are always expressible.
	router.PUT("/v1/instances", upsertHandler(st))
	router.POST("/v1/instances", upsertHandler(st))
	router.GET("/v1/instances", discoverHandler(st, ""))
	router.DELETE("/v1/instances", deleteHandler(st))

	// Resource-style entries.
	service := router.Group("/v1/services/:serviceName")
	service.GET("/instances", discoverHandler(st, "serviceName"))
	instance := service.Group("/instances/:instanceId")
	instance.PUT("", upsertHandler(st))
	instance.GET("", getInstanceHandler(st))
	instance.DELETE("", deleteHandler(st))
	instance.GET("/health", propertyHandler(st, "healthy"))
	instance.GET("/weight", propertyHandler(st, "weight"))
	instance.GET("/heartbeat", propertyHandler(st, "heartbeat"))
}

func upsertHandler(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req upsertRequest
		if c.Request.ContentLength != 0 {
			if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
				invalidParameter(c, "request body must be a valid JSON object")
				return
			}
		}

		serviceName := derefOr(req.ServiceName, c.Param("serviceName"))
		instanceID := derefOr(req.InstanceID, c.Param("instanceId"))
		address := derefOr(req.Address, "")
		heartbeatText := derefOr(req.HeartbeatAt, "")

		if req.Weight == nil {
			invalidParameter(c, "weight must be a non-negative number")
			return
		}
		heartbeatAt, err := parseRequiredTime(heartbeatText)
		if err != nil {
			invalidParameter(c, "heartbeat_at must be an RFC 3339 timestamp")
			return
		}

		inst := store.Instance{
			ServiceName: serviceName,
			InstanceID:  instanceID,
			Address:     address,
			Healthy:     req.Healthy,
			Weight:      *req.Weight,
			HeartbeatAt: heartbeatAt,
		}
		if err := st.UpsertInstance(c.Request.Context(), inst); err != nil {
			mapStoreError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"instance": toDTO(inst)})
	}
}

func discoverHandler(st *store.Store, serviceParam string) gin.HandlerFunc {
	return func(c *gin.Context) {
		serviceName := c.Query("service_name")
		if serviceParam != "" {
			serviceName = c.Param(serviceParam)
		}
		evaluateAt, err := parseRequiredTime(c.Query("evaluate_at"))
		if err != nil {
			invalidParameter(c, "evaluate_at must be an RFC 3339 timestamp")
			return
		}
		timeout, err := parseTimeout(c.Query("timeout"))
		if err != nil {
			invalidParameter(c, "timeout must be a Go duration or a number of seconds greater than zero")
			return
		}

		instances, err := st.Discover(c.Request.Context(), serviceName, evaluateAt, timeout)
		if err != nil {
			mapStoreError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"service_name": serviceName,
			"evaluate_at":  evaluateAt.Format(time.RFC3339Nano),
			"timeout":      timeout.String(),
			"instances":    toDTOs(instances),
		})
	}
}

func getInstanceHandler(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		inst, ok := loadInstance(c, st)
		if !ok {
			return
		}
		c.JSON(http.StatusOK, gin.H{"instance": toDTO(inst)})
	}
}

func propertyHandler(st *store.Store, property string) gin.HandlerFunc {
	return func(c *gin.Context) {
		inst, ok := loadInstance(c, st)
		if !ok {
			return
		}
		body := gin.H{
			"service_name": inst.ServiceName,
			"instance_id":  inst.InstanceID,
		}
		switch property {
		case "healthy":
			body["healthy"] = inst.Healthy
		case "weight":
			body["weight"] = inst.Weight
		case "heartbeat":
			body["heartbeat_at"] = inst.HeartbeatAt.Format(time.RFC3339Nano)
		}
		c.JSON(http.StatusOK, body)
	}
}

func deleteHandler(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		serviceName := c.Param("serviceName")
		instanceID := c.Param("instanceId")
		if serviceName == "" {
			serviceName = c.Query("service_name")
		}
		if instanceID == "" {
			instanceID = c.Query("instance_id")
		}
		if err := st.DeleteInstance(c.Request.Context(), serviceName, instanceID); err != nil {
			mapStoreError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"service_name": serviceName,
			"instance_id":  instanceID,
			"deleted":      true,
		})
	}
}

func loadInstance(c *gin.Context, st *store.Store) (store.Instance, bool) {
	inst, err := st.GetInstance(
		c.Request.Context(), c.Param("serviceName"), c.Param("instanceId"))
	if err != nil {
		mapStoreError(c, err)
		return store.Instance{}, false
	}
	return inst, true
}

func parseRequiredTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, errors.New("missing timestamp")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func parseTimeout(value string) (time.Duration, error) {
	if value == "" {
		return 0, errors.New("missing timeout")
	}
	if timeout, err := time.ParseDuration(value); err == nil {
		if timeout <= 0 {
			return 0, errors.New("timeout must be greater than zero")
		}
		return timeout, nil
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || seconds <= 0 {
		return 0, errors.New("invalid timeout")
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func derefOr(value *string, fallback string) string {
	if value != nil {
		return *value
	}
	return fallback
}

func invalidParameter(c *gin.Context, message string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
		"code":    "invalid_parameter",
		"message": message,
	}})
}

func mapStoreError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidArgument):
		invalidParameter(c, strings.TrimPrefix(err.Error(), store.ErrInvalidArgument.Error()+": "))
	case errors.Is(err, store.ErrEvaluateBeforeHeartbeat):
		invalidParameter(c, "evaluate_at must not be earlier than heartbeat_at")
	case errors.Is(err, store.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{
			"code":    "instance_not_found",
			"message": "no current instance record matches the service name and instance identifier",
		}})
	default:
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
			"code":    "storage_unavailable",
			"message": "database is not available",
		}})
	}
}
