package api

import (
	"errors"
	"sort"
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

func instanceJSON(instance store.Instance) gin.H {
	return gin.H{
		"service_name": instance.ServiceName,
		"instance_id":  instance.InstanceID,
		"address":      instance.Address,
		"port":         instance.Port,
		"healthy":      instance.Healthy,
		"weight":       jsonWeight(instance.Weight),
		"heartbeat_at": instance.HeartbeatAt.Format(time.RFC3339Nano),
	}
}

// handleUpsert stores a registration or an update for one service instance.
// Repeating the same (service name, instance id) pair overwrites the record.
func (s *Server) handleUpsert(c *gin.Context) {
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
	weightValue, ok := bag.get(weightKeys)
	if !ok {
		writeError(c, errInvalidParameter("weight must be a positive number"))
		return
	}
	weight, ok := positiveNumber(weightValue)
	if !ok {
		writeError(c, errInvalidParameter("weight must be a positive number"))
		return
	}
	port := int64(0)
	if portValue, present := bag.get(portKeys); present {
		port, ok = parsePortValue(portValue)
		if !ok {
			writeError(c, errInvalidParameter("port must be a non-negative integer"))
			return
		}
	}
	heartbeatAt, apiErr := bag.requiredHeartbeat()
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}

	healthy := false
	if healthValue, ok := bag.get(healthKeys); ok {
		healthy, ok = strictBoolValue(healthValue)
		if !ok {
			writeError(c, errInvalidParameter("healthy must be a boolean true or false"))
			return
		}
	}
	address := ""
	if addressValue, ok := bag.get(addressKeys); ok {
		address = stringValue(addressValue)
	}

	// evaluate_at is optional on registrations. The clock-skew guard rejects a
	// heartbeat later than the caller's evaluation point.
	if evaluateValue, ok := bag.get(evaluateAtKeys); ok {
		evaluateAt, parsed := parseTimeValue(evaluateValue)
		if !parsed {
			writeError(c, errInvalidParameter("evaluate_at must be a valid timestamp"))
			return
		}
		if evaluateAt.Before(heartbeatAt) {
			writeError(c, errInvalidParameter("evaluate_at must not be earlier than heartbeat_at"))
			return
		}
	}

	input := store.InstanceInput{
		ServiceName: serviceName,
		InstanceID:  instanceID,
		Address:     address,
		Port:        port,
		Healthy:     healthy,
		Weight:      weight,
		HeartbeatAt: heartbeatAt,
	}
	instance, err := s.store.UpsertInstance(input)
	if err != nil {
		writeError(c, errStorageUnavailable())
		return
	}
	c.JSON(200, gin.H{"instance": instanceJSON(instance)})
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

// renewStoreError maps store renewal failures to the public error shape.
func renewStoreError(err error) *apiError {
	if errors.Is(err, store.ErrInstanceNotFound) {
		return errInstanceNotFound()
	}
	return errStorageUnavailable()
}

// handleRenewHeartbeat refreshes only the heartbeat timestamp of the instance
// named by the path; every other field keeps its current value.
func (s *Server) handleRenewHeartbeat(c *gin.Context) {
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
	heartbeatAt, apiErr := bag.requiredHeartbeat()
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	updated, err := s.store.RenewHeartbeats(serviceName, []store.HeartbeatRenewal{
		{InstanceID: instanceID, HeartbeatAt: heartbeatAt},
	})
	if err != nil {
		writeError(c, renewStoreError(err))
		return
	}
	c.JSON(200, gin.H{"instance": instanceJSON(updated[0])})
}

// handleRenewHeartbeats refreshes heartbeat timestamps for many instances of
// one service. Every entry is validated before any write and the store
// commits the batch atomically, so a rejected request changes nothing.
func (s *Server) handleRenewHeartbeats(c *gin.Context) {
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
	itemsValue, ok := bag.get(instancesKeys)
	if !ok {
		writeError(c, errInvalidParameter("instances must be a non-empty list"))
		return
	}
	items, ok := itemsValue.([]any)
	if !ok || len(items) == 0 {
		writeError(c, errInvalidParameter("instances must be a non-empty list"))
		return
	}
	renewals := make([]store.HeartbeatRenewal, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, raw := range items {
		fields, ok := raw.(map[string]any)
		if !ok {
			writeError(c, errInvalidParameter("instances entries must be JSON objects"))
			return
		}
		entry := &paramBag{values: fields}
		instanceID, apiErr := entry.requiredText(instanceIDKeys, "instance_id must not be empty")
		if apiErr != nil {
			writeError(c, apiErr)
			return
		}
		if _, duplicate := seen[instanceID]; duplicate {
			writeError(c, errInvalidParameter("instance_id must not contain duplicates"))
			return
		}
		seen[instanceID] = struct{}{}
		heartbeatAt, apiErr := entry.requiredHeartbeat()
		if apiErr != nil {
			writeError(c, apiErr)
			return
		}
		renewals = append(renewals, store.HeartbeatRenewal{InstanceID: instanceID, HeartbeatAt: heartbeatAt})
	}
	updated, err := s.store.RenewHeartbeats(serviceName, renewals)
	if err != nil {
		writeError(c, renewStoreError(err))
		return
	}
	instances := make([]gin.H, 0, len(updated))
	for _, instance := range updated {
		instances = append(instances, instanceJSON(instance))
	}
	c.JSON(200, gin.H{"updated": len(updated), "instances": instances})
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

// handleGetInstance serves the full record query entry.
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

// handleDiscover implements the weighted discovery entry: only explicitly
// healthy instances whose last heartbeat is not strictly past the lost
// boundary are returned. An instance exactly on the boundary is still
// online. Records strictly past the boundary are removed by the lost
// cleanup; online and unhealthy-but-fresh records are never touched.
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
	evaluateValue, ok := bag.get(evaluateAtKeys)
	if !ok {
		writeError(c, errInvalidParameter("evaluate_at is required"))
		return
	}
	evaluateAt, ok := parseTimeValue(evaluateValue)
	if !ok {
		writeError(c, errInvalidParameter("evaluate_at must be a valid timestamp"))
		return
	}
	timeoutValue, ok := bag.get(timeoutKeys)
	if !ok {
		writeError(c, errInvalidParameter("heartbeat_timeout must be greater than zero"))
		return
	}
	timeout, ok := parseTimeout(timeoutValue)
	if !ok || timeout <= 0 {
		writeError(c, errInvalidParameter("heartbeat_timeout must be greater than zero"))
		return
	}

	instances, err := s.store.ListInstances(serviceName)
	if err != nil {
		writeError(c, errStorageUnavailable())
		return
	}
	for _, instance := range instances {
		if evaluateAt.Before(instance.HeartbeatAt) {
			writeError(c, errInvalidParameter("evaluate_at must not be earlier than heartbeat_at"))
			return
		}
	}
	candidates := make([]gin.H, 0, len(instances))
	lost := make([]store.Instance, 0)
	for _, instance := range instances {
		if evaluateAt.After(instance.HeartbeatAt.Add(timeout)) {
			lost = append(lost, instance)
			continue
		}
		if !instance.Healthy {
			continue
		}
		candidates = append(candidates, instanceJSON(instance))
	}
	// Lost cleanup deletes records strictly past the timeout. It runs only
	// after the request is fully validated, so a rejected query changes
	// nothing.
	for _, instance := range lost {
		if _, err := s.store.DeleteInstance(instance.ServiceName, instance.InstanceID); err != nil {
			writeError(c, errStorageUnavailable())
			return
		}
	}
	sortDiscoverHits(candidates)
	c.JSON(200, gin.H{
		"service_name":      serviceName,
		"evaluate_at":       evaluateAt.Format(time.RFC3339Nano),
		"heartbeat_timeout": jsonWeight(timeout.Seconds()),
		"instances":         candidates,
	})
}

// sortDiscoverHits orders discovery hits by weight descending, then instance
// id ascending, giving deterministic output for identical input.
func sortDiscoverHits(instances []gin.H) {
	sort.SliceStable(instances, func(i, j int) bool {
		leftWeight, _ := toFloat(instances[i]["weight"])
		rightWeight, _ := toFloat(instances[j]["weight"])
		if leftWeight != rightWeight {
			return leftWeight > rightWeight
		}
		return stringValue(instances[i]["instance_id"]) < stringValue(instances[j]["instance_id"])
	})
}

// sortInstances orders by weight descending, heartbeat newest first, then
// instance id ascending, giving deterministic output for identical input.
func sortInstances(instances []gin.H) {
	sort.SliceStable(instances, func(i, j int) bool {
		left, right := instances[i], instances[j]
		leftWeight, _ := toFloat(left["weight"])
		rightWeight, _ := toFloat(right["weight"])
		if leftWeight != rightWeight {
			return leftWeight > rightWeight
		}
		leftHeartbeat, _ := parseTimeValue(left["heartbeat_at"])
		rightHeartbeat, _ := parseTimeValue(right["heartbeat_at"])
		if !leftHeartbeat.Equal(rightHeartbeat) {
			return leftHeartbeat.After(rightHeartbeat)
		}
		return stringValue(left["instance_id"]) < stringValue(right["instance_id"])
	})
}
