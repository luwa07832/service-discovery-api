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
	regexp.MustCompile(`^/api/v1/services/(/discover)/?$`),
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

	// Public instance query entries.
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

	// Discovery of healthy instances with heartbeat-lost removal.
	router.GET("/api/v1/services/:serviceName/discover", server.handleDiscover)
	router.GET("/api/v1/discover", server.handleDiscover)
	router.POST("/api/v1/services/:serviceName/discover", server.handleDiscover)
	router.POST("/api/v1/discover", server.handleDiscover)

	router.NoRoute(func(c *gin.Context) {
		// An empty final path segment (e.g. /api/v1/services//instances)
		// carries an empty service name and must be a parameter error.
		for _, pattern := range emptyServicePaths {
			if pattern.MatchString(c.Request.URL.Path) {
				c.Params = gin.Params{{Key: "serviceName", Value: ""}}
				switch {
				case strings.HasSuffix(c.Request.URL.Path, "/discover"):
					server.handleDiscover(c)
				case c.Request.Method == http.MethodDelete || (c.Request.Method == http.MethodPost && strings.HasSuffix(c.Request.URL.Path, "/delete")):
					server.handleDelete(c)
				case c.Request.Method == http.MethodPost:
					server.handleUpsert(c)
				default:
					server.handleListInstances(c)
				}
				return
			}
		}
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "route_not_found", "message": "no route matches this path"}})
	})
	return router
}
