package api

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/service-discovery-api/internal/store"
)

func jsonWeight(weight float64) any {
	if weight == float64(int64(weight)) {
		return int64(weight)
	}
	return weight
}

func joinAddress(host string, port int) string {
	return host + ":" + strconv.Itoa(port)
}

func instanceJSON(instance store.Instance) gin.H {
	return gin.H{
		"service_name": instance.ServiceName,
		"instance_id":  instance.InstanceID,
		"host":         instance.Host,
		"port":         instance.Port,
		"address":      instance.Address,
		"healthy":      instance.Healthy,
		"weight":       jsonWeight(instance.Weight),
		"heartbeat_at": instance.HeartbeatAt.Format(time.RFC3339Nano),
	}
}

func parseRequestAt(bag *paramBag) (time.Time, *apiError) {
	value, ok := bag.get(requestAtKeys)
	if !ok {
		return time.Time{}, errInvalidParameter("request time is required")
	}
	requestAt, valid := parseTimeValue(value)
	if !valid {
		return time.Time{}, errInvalidParameter("request time must be a valid timestamp")
	}
	return requestAt.UTC(), nil
}

// parseRegistration reads and validates every registration field. The request
// time anchors the initial heartbeat: an explicit heartbeat later than it is a
// parameter error, and a missing heartbeat defaults to the request time.
func parseRegistration(bag *paramBag) (store.Instance, *apiError) {
	serviceName, apiErr := bag.requiredText(serviceNameKeys, "service_name must not be empty")
	if apiErr != nil {
		return store.Instance{}, apiErr
	}
	instanceID, apiErr := bag.requiredText(instanceIDKeys, "instance_id must not be empty")
	if apiErr != nil {
		return store.Instance{}, apiErr
	}

	host := ""
	if hostValue, ok := bag.get(hostKeys); ok {
		host = strings.TrimSpace(stringValue(hostValue))
	}
	port, hasPort := 0, false
	if rawPort, ok := bag.get(portKeys); ok {
		parsedPort, valid := portValue(rawPort)
		if !valid {
			return store.Instance{}, errInvalidParameter("port must be an integer between 0 and 65535")
		}
		port, hasPort = parsedPort, true
	}
	// Fall back to the legacy "host:port" address field when host or port is
	// not provided explicitly.
	if host == "" || !hasPort {
		if addressValue, ok := bag.get(addressKeys); ok {
			fallbackHost, fallbackPort, parsed := splitHostPort(stringValue(addressValue))
			if parsed {
				if host == "" {
					host = fallbackHost
				}
				if !hasPort {
					port, hasPort = fallbackPort, true
				}
			}
		}
	}
	if host == "" {
		return store.Instance{}, errInvalidParameter("host must not be empty")
	}
	if !hasPort {
		return store.Instance{}, errInvalidParameter("port is required")
	}

	weightValue, ok := bag.get(weightKeys)
	if !ok {
		return store.Instance{}, errInvalidParameter("weight must be greater than zero")
	}
	weight, ok := positiveNumber(weightValue)
	if !ok {
		return store.Instance{}, errInvalidParameter("weight must be greater than zero")
	}

	healthy := false
	if healthValue, present := bag.get(healthKeys); present {
		healthy, ok = parseHealthValue(healthValue)
		if !ok {
			return store.Instance{}, errInvalidParameter("healthy must be a boolean health state")
		}
	}

	requestAt, apiErr := parseRequestAt(bag)
	if apiErr != nil {
		return store.Instance{}, apiErr
	}
	heartbeatAt := requestAt
	if heartbeatValue, present := bag.get(heartbeatKeys); present {
		parsed, valid := parseTimeValue(heartbeatValue)
		if !valid {
			return store.Instance{}, errInvalidParameter("heartbeat_at must be a valid timestamp")
		}
		heartbeatAt = parsed
	}
	if heartbeatAt.After(requestAt) {
		return store.Instance{}, errInvalidParameter("heartbeat_at must not be later than request time")
	}

	return store.Instance{
		ServiceName: serviceName,
		InstanceID:  instanceID,
		Host:        host,
		Port:        port,
		Address:     joinAddress(host, port),
		Healthy:     healthy,
		Weight:      weight,
		HeartbeatAt: heartbeatAt.UTC(),
	}, nil
}

// handleUpsert stores a registration, overwriting the record of the same
// (service name, instance id) pair. The response reports whether the pair was
// already registered together with the current record.
func (s *Server) handleUpsert(c *gin.Context) {
	bag, apiErr := buildParamBag(c)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	instance, apiErr := parseRegistration(bag)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	saved, alreadyRegistered, err := s.store.UpsertInstance(instance)
	if err != nil {
		writeError(c, errStorageUnavailable())
		return
	}
	c.JSON(200, gin.H{
		"registered":         true,
		"already_registered": alreadyRegistered,
		"instance":           instanceJSON(saved),
	})
}

// handleHeartbeat applies a health/weight/heartbeat update to an existing
// instance. Unknown instances report updated=false and are never created.
func (s *Server) handleHeartbeat(c *gin.Context) {
	bag, apiErr := buildParamBag(c)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	serviceName, apiErr := bag.requiredText(serviceNameKeys, "service_name must not be empty")
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	instanceID, apiErr := bag.requiredText(instanceIDKeys, "instance_id must not be empty")
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}

	healthy, hasHealth := false, false
	if healthValue, present := bag.get(healthKeys); present {
		parsed, valid := parseHealthValue(healthValue)
		if !valid {
			writeError(c, errInvalidParameter("healthy must be a boolean health state"))
			return
		}
		healthy, hasHealth = parsed, true
	}
	weight, hasWeight := 0.0, false
	if weightValue, present := bag.get(weightKeys); present {
		parsed, valid := positiveNumber(weightValue)
		if !valid {
			writeError(c, errInvalidParameter("weight must be greater than zero"))
			return
		}
		weight, hasWeight = parsed, true
	}

	requestAt, apiErr := parseRequestAt(bag)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	heartbeatAt := requestAt
	if heartbeatValue, present := bag.get(heartbeatKeys); present {
		parsed, valid := parseTimeValue(heartbeatValue)
		if !valid {
			writeError(c, errInvalidParameter("heartbeat_at must be a valid timestamp"))
			return
		}
		heartbeatAt = parsed
	}
	if heartbeatAt.After(requestAt) {
		writeError(c, errInvalidParameter("heartbeat_at must not be later than request time"))
		return
	}

	// A single atomic call reports unknown instances as updated=false and
	// never creates a record; omitted fields keep their current values.
	var healthPtr *bool
	if hasHealth {
		healthPtr = &healthy
	}
	var weightPtr *float64
	if hasWeight {
		weightPtr = &weight
	}
	updated, found := s.store.UpdateInstance(serviceName, instanceID, store.InstanceUpdate{
		Healthy:     healthPtr,
		Weight:      weightPtr,
		HeartbeatAt: heartbeatAt.UTC(),
	})
	if !found {
		c.JSON(200, gin.H{
			"updated":      false,
			"service_name": serviceName,
			"instance_id":  instanceID,
		})
		return
	}
	c.JSON(200, gin.H{
		"updated":      true,
		"service_name": serviceName,
		"instance_id":  instanceID,
		"instance":     instanceJSON(updated),
	})
}

func (s *Server) handleDelete(c *gin.Context) {
	bag, apiErr := buildParamBag(c)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	serviceName, apiErr := bag.requiredText(serviceNameKeys, "service_name must not be empty")
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	instanceID, apiErr := bag.requiredText(instanceIDKeys, "instance_id must not be empty")
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	deleted, err := s.store.DeleteInstance(serviceName, instanceID)
	if err != nil {
		writeError(c, errStorageUnavailable())
		return
	}
	if !deleted {
		writeError(c, errInstanceNotFound())
		return
	}
	c.JSON(200, gin.H{"deleted": true, "service_name": serviceName, "instance_id": instanceID})
}

func (s *Server) loadInstance(c *gin.Context) (store.Instance, *apiError) {
	bag, apiErr := buildParamBag(c)
	if apiErr != nil {
		return store.Instance{}, apiErr
	}
	serviceName, apiErr := bag.requiredText(serviceNameKeys, "service_name must not be empty")
	if apiErr != nil {
		return store.Instance{}, apiErr
	}
	instanceID, apiErr := bag.requiredText(instanceIDKeys, "instance_id must not be empty")
	if apiErr != nil {
		return store.Instance{}, apiErr
	}
	instance, found, err := s.store.GetInstance(serviceName, instanceID)
	if err != nil {
		return store.Instance{}, errStorageUnavailable()
	}
	if !found {
		return store.Instance{}, errInstanceNotFound()
	}
	return instance, nil
}

func (s *Server) handleGetInstance(c *gin.Context) {
	instance, apiErr := s.loadInstance(c)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	c.JSON(200, gin.H{"instance": instanceJSON(instance)})
}

func (s *Server) handleGetHealth(c *gin.Context) {
	instance, apiErr := s.loadInstance(c)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	health := "unhealthy"
	if instance.Healthy {
		health = "healthy"
	}
	c.JSON(200, gin.H{
		"service_name": instance.ServiceName,
		"instance_id":  instance.InstanceID,
		"healthy":      instance.Healthy,
		"health":       health,
	})
}

func (s *Server) handleGetWeight(c *gin.Context) {
	instance, apiErr := s.loadInstance(c)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	c.JSON(200, gin.H{
		"service_name": instance.ServiceName,
		"instance_id":  instance.InstanceID,
		"weight":       jsonWeight(instance.Weight),
	})
}

func (s *Server) handleGetHeartbeat(c *gin.Context) {
	instance, apiErr := s.loadInstance(c)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	c.JSON(200, gin.H{
		"service_name": instance.ServiceName,
		"instance_id":  instance.InstanceID,
		"heartbeat_at": instance.HeartbeatAt.Format(time.RFC3339Nano),
	})
}

// handleListInstances returns all records of a service, never removing any.
func (s *Server) handleListInstances(c *gin.Context) {
	bag, apiErr := buildParamBag(c)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	serviceName, apiErr := bag.requiredText(serviceNameKeys, "service_name must not be empty")
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	instances, err := s.store.ListInstances(serviceName)
	if err != nil {
		writeError(c, errStorageUnavailable())
		return
	}
	wantHealthy, hasFilter := false, false
	if healthFilter, present := bag.get(healthKeys); present {
		parsed, ok := parseHealthValue(healthFilter)
		if !ok {
			writeError(c, errInvalidParameter("healthy must be a boolean health state"))
			return
		}
		wantHealthy, hasFilter = parsed, true
	}
	results := make([]gin.H, 0, len(instances))
	for _, instance := range instances {
		if hasFilter && instance.Healthy != wantHealthy {
			continue
		}
		results = append(results, instanceJSON(instance))
	}
	sortInstances(results)
	c.JSON(200, gin.H{"service_name": serviceName, "instances": results})
}

// parseDiscoveryMoment reads the evaluation time and heartbeat timeout shared
// by discovery and cleanup requests.
func parseDiscoveryMoment(bag *paramBag) (time.Time, time.Duration, *apiError) {
	evaluateValue, ok := bag.get(evaluateAtKeys)
	if !ok {
		return time.Time{}, 0, errInvalidParameter("evaluate_at is required")
	}
	evaluateAt, ok := parseTimeValue(evaluateValue)
	if !ok {
		return time.Time{}, 0, errInvalidParameter("evaluate_at must be a valid timestamp")
	}
	timeoutValue, ok := bag.get(timeoutKeys)
	if !ok {
		return time.Time{}, 0, errInvalidParameter("heartbeat_timeout must be greater than zero")
	}
	timeout, ok := parseTimeout(timeoutValue)
	if !ok || timeout <= 0 {
		return time.Time{}, 0, errInvalidParameter("heartbeat_timeout must be greater than zero")
	}
	return evaluateAt.UTC(), timeout, nil
}

// handleDiscover returns the service's healthy instances that have not been
// lost. Discovery never mutates records; use handleCleanup to reap lost ones.
func (s *Server) handleDiscover(c *gin.Context) {
	bag, apiErr := buildParamBag(c)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	serviceName, apiErr := bag.requiredText(serviceNameKeys, "service_name must not be empty")
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	evaluateAt, timeout, apiErr := parseDiscoveryMoment(bag)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}

	instances, err := s.store.ListInstances(serviceName)
	if err != nil {
		writeError(c, errStorageUnavailable())
		return
	}
	deadline := evaluateAt.Add(-timeout)
	candidates := make([]gin.H, 0, len(instances))
	for _, instance := range instances {
		if !instance.Healthy {
			continue
		}
		// Only a heartbeat strictly earlier than the deadline counts as
		// lost; one at exactly the deadline stays discoverable.
		if instance.HeartbeatAt.Before(deadline) {
			continue
		}
		candidates = append(candidates, instanceJSON(instance))
	}
	sortInstances(candidates)
	c.JSON(200, gin.H{
		"service_name":      serviceName,
		"evaluate_at":       evaluateAt.Format(time.RFC3339Nano),
		"heartbeat_timeout": jsonWeight(timeout.Seconds()),
		"instances":         candidates,
	})
}

// handleCleanup removes every instance of the service whose heartbeat is
// earlier than evaluate_at minus the timeout and returns the deleted ids.
// Discovery and cleanup are separate entries; cleanup is the only one that
// deletes records.
func (s *Server) handleCleanup(c *gin.Context) {
	bag, apiErr := buildParamBag(c)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	serviceName, apiErr := bag.requiredText(serviceNameKeys, "service_name must not be empty")
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	evaluateAt, timeout, apiErr := parseDiscoveryMoment(bag)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}

	removed := s.store.RemoveLost(serviceName, evaluateAt, timeout)
	c.JSON(200, gin.H{
		"service_name":         serviceName,
		"evaluate_at":          evaluateAt.Format(time.RFC3339Nano),
		"heartbeat_timeout":    jsonWeight(timeout.Seconds()),
		"deleted_instance_ids": removed,
	})
}

// sortInstances orders by weight descending and instance id ascending, giving
// deterministic output for identical input.
func sortInstances(instances []gin.H) {
	sort.SliceStable(instances, func(i, j int) bool {
		left, right := instances[i], instances[j]
		leftWeight, _ := toFloat(left["weight"])
		rightWeight, _ := toFloat(right["weight"])
		if leftWeight != rightWeight {
			return leftWeight > rightWeight
		}
		return stringValue(left["instance_id"]) < stringValue(right["instance_id"])
	})
}
