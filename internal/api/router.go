package api

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/service-discovery-api/internal/store"
)

var emptyServicePaths = []*regexp.Regexp{
	regexp.MustCompile(`^/api/v1/services/(/instances)?/?$`),
	regexp.MustCompile(`^/api/v1/services/(/instances/batch)/?$`),
	regexp.MustCompile(`^/api/v1/services/(/discover)/?$`),
	regexp.MustCompile(`^/api/v1/services/(/instances/[^/]+/heartbeat)/?$`),
	regexp.MustCompile(`^/api/v1/services/(/instances/weight)/?$`),
	regexp.MustCompile(`^/api/v1/services/(/instances/[^/]+/weight)/?$`),
	regexp.MustCompile(`^/api/v1/services/[^/]+/instances/(/weight)/?$`),
}

// Server wires the store to every public HTTP entry point.
type Server struct {
	store *store.Store
}

// NewRouter wires the public HTTP surface. The service contract in README.md
// describes the single-error-object shape every entry must keep.
func NewRouter(st *store.Store) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	server := &Server{store: st}

	router.GET("/healthz", func(c *gin.Context) {
		if err := st.Ping(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "storage_unavailable", "message": "database is not available"}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "database": "ok"})
	})

	// Registration and update share one upsert entry; repeated registration
	// of the same (service, instance) pair overwrites the current record.
	router.POST("/api/v1/services/:serviceName/instances", server.handleUpsert)
	router.PUT("/api/v1/services/:serviceName/instances/:instanceId", server.handleUpsert)
	router.POST("/api/v1/register", server.handleUpsert)
	router.POST("/api/v1/instances", server.handleUpsert)
	router.PUT("/api/v1/instances", server.handleUpsert)

	// Batch registration applies several upserts of one service in one
	// atomic request. The path entry takes its service name from the path;
	// the register entry takes it from the body's service_name field.
	router.POST("/api/v1/services/:serviceName/instances/batch", server.handleBatchUpsertByPath)
	router.POST("/api/v1/register/batch", server.handleBatchUpsertByBody)

	// Public instance query entries.
	// Read-only overview of every service that still has instance records.
	router.GET("/api/v1/services", server.handleListServices)
	router.GET("/api/v1/services/:serviceName/instances", server.handleListInstances)
	router.GET("/api/v1/services/:serviceName/instances/:instanceId", server.handleGetInstance)
	router.GET("/api/v1/services/:serviceName/instances/:instanceId/health", server.handleGetHealth)
	router.GET("/api/v1/services/:serviceName/instances/:instanceId/weight", server.handleGetWeight)
	router.GET("/api/v1/services/:serviceName/instances/:instanceId/heartbeat", server.handleGetHeartbeat)
	router.GET("/api/v1/instances", server.handleGetInstance)
	router.GET("/api/v1/instances/health", server.handleGetHealth)
	router.GET("/api/v1/instances/weight", server.handleGetWeight)
	router.GET("/api/v1/instances/heartbeat", server.handleGetHeartbeat)
	router.GET("/api/v1/list", server.handleListInstances)

	// Instance deletion.
	router.DELETE("/api/v1/services/:serviceName/instances/:instanceId", server.handleDelete)
	router.POST("/api/v1/services/:serviceName/instances/:instanceId/delete", server.handleDelete)
	router.DELETE("/api/v1/instances", server.handleDelete)
	router.POST("/api/v1/deregister", server.handleDelete)

	// Heartbeat renewal: only heartbeat_at is replaced; the caller never
	// resubmits address, port, health state or weight.
	router.POST("/api/v1/services/:serviceName/instances/:instanceId/heartbeat", server.handleHeartbeat)
	router.POST("/api/v1/heartbeat", server.handleBatchHeartbeat)

	// Weight-only updates replace just weight; address, port, health state
	// and heartbeat time keep their current values, and the upsert
	// overwrite semantics stay unchanged.
	router.PUT("/api/v1/services/:serviceName/instances/weight", server.handleBatchUpdateWeight)
	router.PUT("/api/v1/services/:serviceName/instances/:instanceId/weight", server.handleUpdateWeight)

	// Discovery of healthy instances with heartbeat-lost removal.
	router.GET("/api/v1/services/:serviceName/discover", server.handleDiscover)
	router.GET("/api/v1/discover", server.handleDiscover)
	router.POST("/api/v1/services/:serviceName/discover", server.handleDiscover)
	router.POST("/api/v1/discover", server.handleDiscover)

	// Standalone lost-instance cleanup across one or all services.
	router.POST("/api/v1/cleanup", server.handleCleanup)

	router.NoRoute(func(c *gin.Context) {
		// An empty final path segment (e.g. /api/v1/services//instances)
		// carries an empty service name and must be a parameter error.
		for _, pattern := range emptyServicePaths {
			if pattern.MatchString(c.Request.URL.Path) {
				switch {
				case strings.HasSuffix(c.Request.URL.Path, "/discover"):
					c.Params = gin.Params{{Key: "serviceName", Value: ""}}
					server.handleDiscover(c)
				case strings.HasSuffix(c.Request.URL.Path, "/heartbeat"):
					c.Params = gin.Params{{Key: "serviceName", Value: ""}}
					server.handleHeartbeat(c)
				case strings.HasSuffix(c.Request.URL.Path, "/instances/batch"):
					c.Params = gin.Params{{Key: "serviceName", Value: ""}}
					server.handleBatchUpsertByPath(c)
				case c.Request.Method == http.MethodPut && strings.HasSuffix(c.Request.URL.Path, "/instances/weight"):
					c.Params = gin.Params{{Key: "serviceName", Value: ""}}
					server.handleBatchUpdateWeight(c)
				case c.Request.Method == http.MethodPut && strings.HasSuffix(c.Request.URL.Path, "/weight"):
					emptyInstance := strings.HasSuffix(c.Request.URL.Path, "//weight")
					if emptyInstance {
						c.Params = gin.Params{
							{Key: "serviceName", Value: ""},
							{Key: "instanceId", Value: ""},
						}
					} else {
						c.Params = gin.Params{{Key: "serviceName", Value: ""}}
					}
					server.handleUpdateWeight(c)
				case c.Request.Method == http.MethodDelete || (c.Request.Method == http.MethodPost && strings.HasSuffix(c.Request.URL.Path, "/delete")):
					c.Params = gin.Params{{Key: "serviceName", Value: ""}}
					server.handleDelete(c)
				case c.Request.Method == http.MethodPost:
					c.Params = gin.Params{{Key: "serviceName", Value: ""}}
					server.handleUpsert(c)
				default:
					c.Params = gin.Params{{Key: "serviceName", Value: ""}}
					server.handleListInstances(c)
				}
				return
			}
		}
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "route_not_found", "message": "no route matches this path"}})
	})
	return router
}
