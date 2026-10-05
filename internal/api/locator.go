package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// servicesPathPrefix introduces every path-style entry whose locators are
// carried by the URL path rather than the query string or JSON body.
const servicesPathPrefix = "/api/v1/services/"

// servicePathKind classifies a recognized /api/v1/services/... route shape.
type servicePathKind string

const (
	shapeCollection    servicePathKind = "collection"     // /services/{s}/instances
	shapeBatchRegister servicePathKind = "batch-register" // /services/{s}/instances/batch
	shapeBatchWeight   servicePathKind = "batch-weight"   // PUT  /services/{s}/instances/weight
	shapeBatchHealth   servicePathKind = "batch-health"   // PUT  /services/{s}/instances/health
	shapeItem          servicePathKind = "item"           // /services/{s}/instances/{i}
	shapeItemAction    servicePathKind = "item-action"    // /services/{s}/instances/{i}/{action}
	shapeDiscover      servicePathKind = "discover"       // /services/{s}/discover
)

// servicePathShape describes one recognized path-style route shape. The
// captured segment values come straight from the (percent-decoded) URL path,
// so an empty or whitespace-only segment stays visible even when gin drops an
// empty wildcard parameter before the handler runs.
type servicePathShape struct {
	matched     bool
	kind        servicePathKind
	action      string // "heartbeat", "weight", "health" or "delete"
	service     string
	instance    string
	hasService  bool
	hasInstance bool
}

// parseServicesPath classifies the request path. The method matters because
// the static segments "batch", "weight" and "health" name batch routes for
// POST/PUT while they are ordinary instance ids for the other verbs (gin
// builds a separate route tree per method).
func parseServicesPath(method, path string) servicePathShape {
	if !strings.HasPrefix(path, servicesPathPrefix) {
		return servicePathShape{}
	}
	rest := strings.TrimPrefix(path, servicesPathPrefix)
	// A slash-only tail (e.g. /api/v1/services/ or .../services//) names an
	// empty service on the collection entry.
	if strings.Trim(rest, "/") == "" {
		return servicePathShape{matched: true, kind: shapeCollection,
			service: "", hasService: true}
	}
	rest = strings.TrimSuffix(rest, "/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		return servicePathShape{}
	}
	service := parts[0]

	if parts[1] == "discover" {
		if len(parts) == 2 {
			return servicePathShape{matched: true, kind: shapeDiscover,
				service: service, hasService: true}
		}
		return servicePathShape{}
	}
	if parts[1] != "instances" {
		return servicePathShape{}
	}

	switch len(parts) {
	case 2:
		return servicePathShape{matched: true, kind: shapeCollection,
			service: service, hasService: true}
	case 3:
		switch parts[2] {
		case "batch":
			if method == http.MethodPost {
				return servicePathShape{matched: true, kind: shapeBatchRegister,
					service: service, hasService: true}
			}
		case "weight":
			if method == http.MethodPut {
				return servicePathShape{matched: true, kind: shapeBatchWeight,
					service: service, hasService: true}
			}
		case "health":
			if method == http.MethodPut {
				return servicePathShape{matched: true, kind: shapeBatchHealth,
					service: service, hasService: true}
			}
		}
		// GET/PUT/DELETE address a concrete instance id here (which may
		// literally be "batch", "weight" or "health") on the item route.
		return servicePathShape{matched: true, kind: shapeItem,
			service: service, hasService: true,
			instance: parts[2], hasInstance: true}
	case 4:
		switch parts[3] {
		case "heartbeat", "weight", "health", "delete":
			return servicePathShape{matched: true, kind: shapeItemAction,
				action:  parts[3],
				service: service, hasService: true,
				instance: parts[2], hasInstance: true}
		}
	}
	return servicePathShape{}
}

type locatorTarget int

const (
	locServiceOnly        locatorTarget = iota // only the service name is path-addressed
	locServiceAndInstance                      // both locators are path-addressed
)

// resolveLocator returns the service name and, when requested, the instance
// id a handler must act on.
//
// On a path-style entry a concrete path segment is always authoritative: a
// conflicting value, an empty value, a non-string value or any supported
// synonym found in the JSON body or query string cannot change it. A path
// segment that trims empty is a parameter error and must not be filled from
// another source. When the path does not carry a locator (the parameter-style
// entries and the single-instance collection route, which only names the
// service), the field keeps its existing query/body lookup rules.
func resolveLocator(c *gin.Context, bag *paramBag, target locatorTarget) (string, string, *apiError) {
	shape := parseServicesPath(c.Request.Method, c.Request.URL.Path)

	serviceName := ""
	if shape.matched && shape.hasService {
		serviceName = strings.TrimSpace(shape.service)
		if serviceName == "" {
			return "", "", errInvalidParameter("service_name must not be empty")
		}
	} else {
		var apiErr *apiError
		serviceName, apiErr = bag.requiredText(serviceNameKeys, "service_name must not be empty")
		if apiErr != nil {
			return "", "", apiErr
		}
	}

	instanceID := ""
	if target == locServiceAndInstance {
		if shape.matched && shape.hasInstance {
			instanceID = strings.TrimSpace(shape.instance)
			if instanceID == "" {
				return "", "", errInvalidParameter("instance_id must not be empty")
			}
		} else {
			var apiErr *apiError
			instanceID, apiErr = bag.requiredText(instanceIDKeys, "instance_id must not be empty")
			if apiErr != nil {
				return "", "", apiErr
			}
		}
	}
	return serviceName, instanceID, nil
}
