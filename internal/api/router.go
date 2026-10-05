package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/service-discovery-api/internal/store"
)

// Server wires the store to every public HTTP entry point.
type Server struct {
	store *store.Store
}

// NewRouter wires the public HTTP surface. The service contract in README.md
// describes the single-error-object shape every entry must keep.
func NewRouter(st *store.Store) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	// An empty final segment (e.g. /services/svc/instances//) would otherwise
	// be hidden behind a trailing-slash redirect; it must reach the entry
	// point as the empty locator it is so it answers invalid_parameter.
	router.RedirectTrailingSlash = false
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

	// Health-state-only updates replace just the healthy flag; address,
	// port, weight and heartbeat time keep their current values, no missing
	// instance is created and no heartbeat is renewed.
	router.PUT("/api/v1/services/:serviceName/instances/health", server.handleBatchUpdateHealth)
	router.PUT("/api/v1/services/:serviceName/instances/:instanceId/health", server.handleUpdateHealth)

	// Discovery of healthy instances with heartbeat-lost removal.
	router.GET("/api/v1/services/:serviceName/discover", server.handleDiscover)
	router.GET("/api/v1/discover", server.handleDiscover)
	router.POST("/api/v1/services/:serviceName/discover", server.handleDiscover)
	router.POST("/api/v1/discover", server.handleDiscover)
	router.POST("/api/v1/discover/batch", server.handleBatchDiscover)

	// Standalone lost-instance cleanup across one or all services.
	router.POST("/api/v1/cleanup", server.handleCleanup)

	router.NoRoute(func(c *gin.Context) {
		// Classify the path by shape instead of relying on gin's wildcard
		// parameters: an empty wildcard is dropped in some positions, and an
		// empty final segment becomes a redirect otherwise. Every recognized
		// shape is sent to its real handler so a blank service or instance
		// segment answers invalid_parameter instead of falling back to query
		// parameters, while genuinely unknown paths stay 404s.
		shape := parseServicesPath(c.Request.Method, c.Request.URL.Path)
		if !shape.matched {
			c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "route_not_found", "message": "no route matches this path"}})
			return
		}
		if shape.hasService {
			c.Params = append(c.Params, gin.Param{Key: "serviceName", Value: shape.service})
		}
		if shape.hasInstance {
			c.Params = append(c.Params, gin.Param{Key: "instanceId", Value: shape.instance})
		}

		switch shape.kind {
		case shapeDiscover:
			server.handleDiscover(c)
		case shapeCollection:
			// A blank service segment is a parameter error on any verb; the
			// list handler reports it uniformly. A concrete service only
			// accepts the registered GET (list) and POST (register) verbs.
			if strings.TrimSpace(shape.service) == "" {
				server.handleListInstances(c)
			} else {
				switch c.Request.Method {
				case http.MethodGet:
					server.handleListInstances(c)
				case http.MethodPost:
					server.handleUpsert(c)
				default:
					routeNotFound(c)
				}
			}
		case shapeBatchRegister:
			server.handleBatchUpsertByPath(c)
		case shapeBatchWeight:
			server.handleBatchUpdateWeight(c)
		case shapeBatchHealth:
			server.handleBatchUpdateHealth(c)
		case shapeItem:
			switch c.Request.Method {
			case http.MethodGet:
				server.handleGetInstance(c)
			case http.MethodPut:
				server.handleUpsert(c)
			case http.MethodDelete:
				server.handleDelete(c)
			default:
				routeNotFound(c)
			}
		case shapeItemAction:
			switch shape.action {
			case "heartbeat":
				switch c.Request.Method {
				case http.MethodGet:
					server.handleGetHeartbeat(c)
				case http.MethodPost:
					server.handleHeartbeat(c)
				default:
					routeNotFound(c)
				}
			case "weight":
				switch c.Request.Method {
				case http.MethodGet:
					server.handleGetWeight(c)
				case http.MethodPut:
					server.handleUpdateWeight(c)
				default:
					routeNotFound(c)
				}
			case "health":
				switch c.Request.Method {
				case http.MethodGet:
					server.handleGetHealth(c)
				case http.MethodPut:
					server.handleUpdateHealth(c)
				default:
					routeNotFound(c)
				}
			case "delete":
				if c.Request.Method == http.MethodPost {
					server.handleDelete(c)
				} else {
					routeNotFound(c)
				}
			default:
				routeNotFound(c)
			}
		default:
			routeNotFound(c)
		}
	})
	return router
}

func routeNotFound(c *gin.Context) {
	c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "route_not_found", "message": "no route matches this path"}})
}
