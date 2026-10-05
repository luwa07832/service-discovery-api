package api

import (
	"errors"
	"sort"
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

	// The item route and parameter-style entries resolve both locators here.
	// The collection route's shape carries no instance segment, so the
	// resolver falls back to the existing query/body lookup for it: only its
	// service name is path-authoritative.
	serviceName, instanceID, apiErr := resolveLocator(c, bag, locServiceAndInstance)
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
	heartbeatValue, ok := bag.get(heartbeatKeys)
	if !ok {
		writeError(c, errInvalidParameter("heartbeat_at is required"))
		return
	}
	heartbeatAt, ok := parseTimeValue(heartbeatValue)
	if !ok {
		writeError(c, errInvalidParameter("heartbeat_at must be a valid timestamp"))
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

// handleBatchUpsertByPath serves POST /api/v1/services/{serviceName}/...:
// the path service name wins even when the body carries its own service_name.
func (s *Server) handleBatchUpsertByPath(c *gin.Context) {
	s.handleBatchUpsert(c, strings.TrimSpace(c.Param("serviceName")), true)
}

// handleBatchUpsertByBody serves POST /api/v1/register/batch and reads
// service_name from the request body like the single-instance register entry.
func (s *Server) handleBatchUpsertByBody(c *gin.Context) {
	s.handleBatchUpsert(c, "", false)
}

// handleBatchUpsert registers or overwrites several instances of one service
// in one request. Every entry is validated before any write, so an invalid
// batch changes no record; the store applies the whole batch atomically.
func (s *Server) handleBatchUpsert(c *gin.Context, pathServiceName string, serviceNameFromPath bool) {
	bag, apiErr := buildParamBag(c)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	serviceName := pathServiceName
	if !serviceNameFromPath {
		serviceName, apiErr = bag.requiredText(serviceNameKeys, "service_name must not be empty")
		if apiErr != nil {
			writeError(c, apiErr)
			return
		}
	} else if serviceName == "" {
		writeError(c, errInvalidParameter("service_name must not be empty"))
		return
	}

	rawEntries, ok := bag.get([]string{"instances"})
	if !ok {
		writeError(c, errInvalidParameter("instances must be a non-empty list"))
		return
	}
	entries, ok := rawEntries.([]any)
	if !ok || len(entries) == 0 {
		writeError(c, errInvalidParameter("instances must be a non-empty list"))
		return
	}

	inputs := make([]store.InstanceInput, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			writeError(c, errInvalidParameter("each instance must be an object"))
			return
		}
		instanceID := strings.TrimSpace(stringValue(entry["instance_id"]))
		if instanceID == "" {
			writeError(c, errInvalidParameter("instance_id must not be empty"))
			return
		}
		if _, duplicated := seen[instanceID]; duplicated {
			writeError(c, errInvalidParameter("instance_id must not be duplicated"))
			return
		}
		weightValue, present := entry["weight"]
		if !present {
			writeError(c, errInvalidParameter("weight must be a positive number"))
			return
		}
		weight, ok := positiveNumber(weightValue)
		if !ok {
			writeError(c, errInvalidParameter("weight must be a positive number"))
			return
		}
		port := int64(0)
		if portValue, present := entry["port"]; present {
			port, ok = parsePortValue(portValue)
			if !ok {
				writeError(c, errInvalidParameter("port must be a non-negative integer"))
				return
			}
		}
		healthy := false
		if healthValue, present := entry["healthy"]; present {
			healthy, ok = strictBoolValue(healthValue)
			if !ok {
				writeError(c, errInvalidParameter("healthy must be a boolean true or false"))
				return
			}
		}
		heartbeatValue, present := entry["heartbeat_at"]
		if !present {
			writeError(c, errInvalidParameter("heartbeat_at is required"))
			return
		}
		heartbeatAt, parsed := parseTimeValue(heartbeatValue)
		if !parsed {
			writeError(c, errInvalidParameter("heartbeat_at must be a valid timestamp"))
			return
		}
		address := ""
		if addressValue, present := entry["address"]; present {
			address = stringValue(addressValue)
		}
		seen[instanceID] = struct{}{}
		inputs = append(inputs, store.InstanceInput{
			ServiceName: serviceName,
			InstanceID:  instanceID,
			Address:     address,
			Port:        port,
			Healthy:     healthy,
			Weight:      weight,
			HeartbeatAt: heartbeatAt,
		})
	}

	instances, err := s.store.UpsertInstances(inputs)
	if err != nil {
		writeError(c, errStorageUnavailable())
		return
	}
	results := make([]gin.H, 0, len(instances))
	for _, instance := range instances {
		results = append(results, instanceJSON(instance))
	}
	c.JSON(200, gin.H{
		"service_name": serviceName,
		"registered":   len(results),
		"instances":    results,
	})
}

func (s *Server) handleDelete(c *gin.Context) {
	bag, apiErr := buildParamBag(c)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	serviceName, instanceID, apiErr := resolveLocator(c, bag, locServiceAndInstance)
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

// handleHeartbeat renews heartbeat_at of one path-addressed instance. The
// caller does not resubmit address, port, health state or weight: only
// heartbeat_at is replaced and every other field keeps its current value.
func (s *Server) handleHeartbeat(c *gin.Context) {
	bag, apiErr := buildParamBag(c)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	serviceName, instanceID, apiErr := resolveLocator(c, bag, locServiceAndInstance)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	heartbeatAt, apiErr := requireHeartbeat(bag)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	updated, err := s.store.TouchHeartbeats(serviceName, []store.HeartbeatTarget{
		{InstanceID: instanceID, HeartbeatAt: heartbeatAt},
	})
	if apiErr := mapTargetError(err); apiErr != nil {
		writeError(c, apiErr)
		return
	}
	c.JSON(200, gin.H{"instance": instanceJSON(updated[0])})
}

// handleBatchHeartbeat renews heartbeat_at for several instances of one
// service. Every entry is validated before any write, so a missing, empty,
// duplicated or malformed request changes no record.
func (s *Server) handleBatchHeartbeat(c *gin.Context) {
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
	rawEntries, ok := bag.get([]string{"instances"})
	if !ok {
		writeError(c, errInvalidParameter("instances must be a non-empty list"))
		return
	}
	entries, ok := rawEntries.([]any)
	if !ok || len(entries) == 0 {
		writeError(c, errInvalidParameter("instances must be a non-empty list"))
		return
	}
	targets := make([]store.HeartbeatTarget, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			writeError(c, errInvalidParameter("each instance must be an object"))
			return
		}
		instanceID := strings.TrimSpace(stringValue(entry["instance_id"]))
		if instanceID == "" {
			writeError(c, errInvalidParameter("instance_id must not be empty"))
			return
		}
		if _, duplicated := seen[instanceID]; duplicated {
			writeError(c, errInvalidParameter("instance_id must not be duplicated"))
			return
		}
		heartbeatValue, present := entry["heartbeat_at"]
		if !present {
			writeError(c, errInvalidParameter("heartbeat_at is required"))
			return
		}
		heartbeatAt, parsed := parseTimeValue(heartbeatValue)
		if !parsed {
			writeError(c, errInvalidParameter("heartbeat_at must be a valid timestamp"))
			return
		}
		seen[instanceID] = struct{}{}
		targets = append(targets, store.HeartbeatTarget{
			InstanceID:  instanceID,
			HeartbeatAt: heartbeatAt,
		})
	}
	updated, err := s.store.TouchHeartbeats(serviceName, targets)
	if apiErr := mapTargetError(err); apiErr != nil {
		writeError(c, apiErr)
		return
	}
	instances := make([]gin.H, 0, len(updated))
	for _, instance := range updated {
		instances = append(instances, instanceJSON(instance))
	}
	c.JSON(200, gin.H{"updated": len(instances), "instances": instances})
}

func requireHeartbeat(bag *paramBag) (time.Time, *apiError) {
	heartbeatValue, ok := bag.get(heartbeatKeys)
	if !ok {
		return time.Time{}, errInvalidParameter("heartbeat_at is required")
	}
	heartbeatAt, ok := parseTimeValue(heartbeatValue)
	if !ok {
		return time.Time{}, errInvalidParameter("heartbeat_at must be a valid timestamp")
	}
	return heartbeatAt, nil
}

// handleUpdateWeight changes only the weight of one path-addressed instance.
// The caller reads weight from the JSON body or the query parameter (the JSON
// body wins when both carry it) and never resubmits address, port, health
// state or heartbeat time: every other field keeps its current value.
func (s *Server) handleUpdateWeight(c *gin.Context) {
	bag, apiErr := buildParamBag(c)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	serviceName, instanceID, apiErr := resolveLocator(c, bag, locServiceAndInstance)
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
	updated, err := s.store.UpdateWeights(serviceName, []store.WeightTarget{
		{InstanceID: instanceID, Weight: weight},
	})
	if apiErr := mapTargetError(err); apiErr != nil {
		writeError(c, apiErr)
		return
	}
	c.JSON(200, gin.H{"instance": instanceJSON(updated[0])})
}

// handleBatchUpdateWeight changes only the weights of several instances of
// one path-addressed service. The body is one JSON object with a non-empty
// updates list; every entry is validated and every target confirmed to exist
// before any write, so an invalid or incomplete batch changes no record and
// the store applies the whole batch atomically.
func (s *Server) handleBatchUpdateWeight(c *gin.Context) {
	bag, apiErr := buildParamBag(c)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	serviceName := strings.TrimSpace(c.Param("serviceName"))
	if serviceName == "" {
		writeError(c, errInvalidParameter("service_name must not be empty"))
		return
	}

	rawEntries, ok := bag.get([]string{"updates"})
	if !ok {
		writeError(c, errInvalidParameter("updates must be a non-empty list"))
		return
	}
	entries, ok := rawEntries.([]any)
	if !ok || len(entries) == 0 {
		writeError(c, errInvalidParameter("updates must be a non-empty list"))
		return
	}

	targets := make([]store.WeightTarget, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			writeError(c, errInvalidParameter("each instance must be an object"))
			return
		}
		instanceID := strings.TrimSpace(stringValue(entry["instance_id"]))
		if instanceID == "" {
			writeError(c, errInvalidParameter("instance_id must not be empty"))
			return
		}
		if _, duplicated := seen[instanceID]; duplicated {
			writeError(c, errInvalidParameter("instance_id must not be duplicated"))
			return
		}
		weightValue, present := entry["weight"]
		if !present {
			writeError(c, errInvalidParameter("weight must be a positive number"))
			return
		}
		weight, ok := positiveNumber(weightValue)
		if !ok {
			writeError(c, errInvalidParameter("weight must be a positive number"))
			return
		}
		seen[instanceID] = struct{}{}
		targets = append(targets, store.WeightTarget{
			InstanceID: instanceID,
			Weight:     weight,
		})
	}

	updated, err := s.store.UpdateWeights(serviceName, targets)
	if apiErr := mapTargetError(err); apiErr != nil {
		writeError(c, apiErr)
		return
	}
	instances := make([]gin.H, 0, len(updated))
	for _, instance := range updated {
		instances = append(instances, instanceJSON(instance))
	}
	c.JSON(200, gin.H{"updated": len(instances), "instances": instances})
}

func mapTargetError(err error) *apiError {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrInstanceNotFound):
		return errInstanceNotFound()
	default:
		return errStorageUnavailable()
	}
}

// handleUpdateHealth changes only the healthy flag of one path-addressed
// instance. It reads healthy from the JSON body or the query parameter (the
// JSON body wins when both carry it) and never resubmits address, port,
// weight or heartbeat time: every other field keeps its current value. No
// record is created when the target does not exist.
func (s *Server) handleUpdateHealth(c *gin.Context) {
	serviceName := strings.TrimSpace(c.Param("serviceName"))
	if serviceName == "" {
		writeError(c, errInvalidParameter("service_name must not be empty"))
		return
	}
	instanceID := strings.TrimSpace(c.Param("instanceId"))
	if instanceID == "" {
		writeError(c, errInvalidParameter("instance_id must not be empty"))
		return
	}
	healthy, apiErr := readHealthyInput(c)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	updated, err := s.store.UpdateHealth(serviceName, []store.HealthTarget{
		{InstanceID: instanceID, Healthy: healthy},
	})
	if apiErr := mapTargetError(err); apiErr != nil {
		writeError(c, apiErr)
		return
	}
	c.JSON(200, gin.H{"instance": instanceJSON(updated[0])})
}

// handleBatchUpdateHealth changes only the healthy flags of several
// instances of one path-addressed service. The body is one JSON object with
// a non-empty updates list; every entry is validated and every target
// confirmed to exist before any write, so an invalid or incomplete batch
// changes no record and the store applies the whole batch atomically.
func (s *Server) handleBatchUpdateHealth(c *gin.Context) {
	serviceName := strings.TrimSpace(c.Param("serviceName"))
	if serviceName == "" {
		writeError(c, errInvalidParameter("service_name must not be empty"))
		return
	}

	body, apiErr := readJSONObject(c)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	rawEntries, ok := body["updates"]
	if !ok {
		writeError(c, errInvalidParameter("updates must be a non-empty list"))
		return
	}
	entries, ok := rawEntries.([]any)
	if !ok || len(entries) == 0 {
		writeError(c, errInvalidParameter("updates must be a non-empty list"))
		return
	}

	targets := make([]store.HealthTarget, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			writeError(c, errInvalidParameter("each instance must be an object"))
			return
		}
		instanceID := strings.TrimSpace(stringValue(entry["instance_id"]))
		if instanceID == "" {
			writeError(c, errInvalidParameter("instance_id must not be empty"))
			return
		}
		if _, duplicated := seen[instanceID]; duplicated {
			writeError(c, errInvalidParameter("instance_id must not be duplicated"))
			return
		}
		healthValue, present := entry["healthy"]
		if !present {
			writeError(c, errInvalidParameter("healthy must be a boolean true or false"))
			return
		}
		healthy, ok := strictBoolValue(healthValue)
		if !ok {
			writeError(c, errInvalidParameter("healthy must be a boolean true or false"))
			return
		}
		seen[instanceID] = struct{}{}
		targets = append(targets, store.HealthTarget{
			InstanceID: instanceID,
			Healthy:    healthy,
		})
	}

	updated, err := s.store.UpdateHealth(serviceName, targets)
	if apiErr := mapTargetError(err); apiErr != nil {
		writeError(c, apiErr)
		return
	}
	instances := make([]gin.H, 0, len(updated))
	for _, instance := range updated {
		instances = append(instances, instanceJSON(instance))
	}
	c.JSON(200, gin.H{"updated": len(instances), "instances": instances})
}

func (s *Server) loadInstance(c *gin.Context) (store.Instance, *apiError) {
	bag, apiErr := buildParamBag(c)
	if apiErr != nil {
		return store.Instance{}, apiErr
	}
	serviceName, instanceID, apiErr := resolveLocator(c, bag, locServiceAndInstance)
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
	serviceName, _, apiErr := resolveLocator(c, bag, locServiceOnly)
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

// handleListServices serves the read-only service overview. It aggregates the
// current SQLite records of every service at the requested evaluation point
// without creating, updating or deleting anything, and in particular without
// running the discovery lost-record cleanup. Lost instances stay stored and
// are only counted as lost.
func (s *Server) handleListServices(c *gin.Context) {
	bag, apiErr := buildParamBag(c)
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

	instances, err := s.store.ListAllInstances()
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

	// Aggregate records per service. The same counters classify every
	// record exactly once, so total_instances is always the sum of the
	// three state counters.
	byService := make(map[string]*serviceCounters)
	for _, instance := range instances {
		counters := byService[instance.ServiceName]
		if counters == nil {
			counters = &serviceCounters{}
			byService[instance.ServiceName] = counters
		}
		counters.total++
		switch {
		case evaluateAt.After(instance.HeartbeatAt.Add(timeout)):
			counters.lost++
		case instance.Healthy:
			counters.available++
		default:
			counters.unhealthyFresh++
		}
	}

	serviceNames := make([]string, 0, len(byService))
	for serviceName := range byService {
		serviceNames = append(serviceNames, serviceName)
	}
	sort.Strings(serviceNames)

	services := make([]gin.H, 0, len(serviceNames))
	for _, serviceName := range serviceNames {
		counters := byService[serviceName]
		services = append(services, gin.H{
			"service_name":              serviceName,
			"total_instances":           counters.total,
			"available_instances":       counters.available,
			"unhealthy_fresh_instances": counters.unhealthyFresh,
			"lost_instances":            counters.lost,
		})
	}
	c.JSON(200, gin.H{"services": services})
}

type serviceCounters struct {
	total          int
	available      int
	unhealthyFresh int
	lost           int
}

// handleDiscover implements the weighted discovery entry: only explicitly
// healthy instances whose last heartbeat is not strictly past the lost
// boundary are returned. An instance exactly on the boundary is still
// online. Records strictly past the boundary are removed by the lost
// cleanup in a single transaction, so a storage failure leaves every
// record unchanged and yields no candidate list; online and
// unhealthy-but-fresh records are never touched.
func (s *Server) handleDiscover(c *gin.Context) {
	bag, apiErr := buildParamBag(c)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	serviceName, _, apiErr := resolveLocator(c, bag, locServiceOnly)
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
	lostKeys := make([]store.InstanceKey, 0)
	for _, instance := range instances {
		if evaluateAt.After(instance.HeartbeatAt.Add(timeout)) {
			lostKeys = append(lostKeys, store.InstanceKey{
				ServiceName: instance.ServiceName,
				InstanceID:  instance.InstanceID,
			})
			continue
		}
		if !instance.Healthy {
			continue
		}
		candidates = append(candidates, instanceJSON(instance))
	}
	// Lost cleanup deletes records strictly past the timeout. It runs only
	// after the request is fully validated, and all lost records are removed
	// in one transaction: a storage failure rolls everything back, so a
	// failed request never leaves partial deletions behind.
	if len(lostKeys) > 0 {
		if err := s.store.DeleteInstances(lostKeys); err != nil {
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

// handleBatchDiscover implements POST /api/v1/discover/batch. Several
// services share one evaluation point and timeout. Parameters and stored
// data are fully validated before any record is removed: every specified
// service is read first, the clock-skew guard covers every instance of
// every specified service, and lost records across all services are removed
// in one transaction so a failure never leaves partial deletions. Services
// that are not requested are never read or touched; a requested service
// without records yields an empty instance list while keeping its place in
// the request order.
func (s *Server) handleBatchDiscover(c *gin.Context) {
	body, apiErr := readJSONObject(c)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}

	rawNames, present := body["service_names"]
	if !present {
		writeError(c, errInvalidParameter("service_names must be a non-empty list of unique service names"))
		return
	}
	entries, ok := rawNames.([]any)
	if !ok || len(entries) == 0 {
		writeError(c, errInvalidParameter("service_names must be a non-empty list of unique service names"))
		return
	}
	serviceNames := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, rawName := range entries {
		name, ok := rawName.(string)
		if !ok {
			writeError(c, errInvalidParameter("service_names must be a non-empty list of unique service names"))
			return
		}
		name = strings.TrimSpace(name)
		if name == "" {
			writeError(c, errInvalidParameter("service_names must be a non-empty list of unique service names"))
			return
		}
		if _, duplicated := seen[name]; duplicated {
			writeError(c, errInvalidParameter("service_names must not contain duplicates"))
			return
		}
		seen[name] = struct{}{}
		serviceNames = append(serviceNames, name)
	}

	evaluateValue, present := body["evaluate_at"]
	if !present {
		writeError(c, errInvalidParameter("evaluate_at is required"))
		return
	}
	evaluateAt, ok := parseTimeValue(evaluateValue)
	if !ok {
		writeError(c, errInvalidParameter("evaluate_at must be a valid timestamp"))
		return
	}
	timeoutValue, present := body["heartbeat_timeout"]
	if !present {
		writeError(c, errInvalidParameter("heartbeat_timeout must be greater than zero"))
		return
	}
	timeout, ok := parseTimeout(timeoutValue)
	if !ok || timeout <= 0 {
		writeError(c, errInvalidParameter("heartbeat_timeout must be greater than zero"))
		return
	}

	// Read every requested service before any classification or deletion,
	// so a storage read failure changes nothing.
	byService := make(map[string][]store.Instance, len(serviceNames))
	for _, serviceName := range serviceNames {
		instances, err := s.store.ListInstances(serviceName)
		if err != nil {
			writeError(c, errStorageUnavailable())
			return
		}
		byService[serviceName] = instances
	}
	// The clock-skew guard covers all records before the lost boundary is
	// applied, rejecting the whole batch without deleting anything.
	for _, serviceName := range serviceNames {
		for _, instance := range byService[serviceName] {
			if evaluateAt.Before(instance.HeartbeatAt) {
				writeError(c, errInvalidParameter("evaluate_at must not be earlier than heartbeat_at"))
				return
			}
		}
	}

	lostKeys := make([]store.InstanceKey, 0)
	for _, serviceName := range serviceNames {
		for _, instance := range byService[serviceName] {
			if evaluateAt.After(instance.HeartbeatAt.Add(timeout)) {
				lostKeys = append(lostKeys, store.InstanceKey{
					ServiceName: instance.ServiceName,
					InstanceID:  instance.InstanceID,
				})
			}
		}
	}
	// All lost records are removed atomically across services.
	if len(lostKeys) > 0 {
		if err := s.store.DeleteInstances(lostKeys); err != nil {
			writeError(c, errStorageUnavailable())
			return
		}
	}

	services := make([]gin.H, 0, len(serviceNames))
	for _, serviceName := range serviceNames {
		candidates := make([]gin.H, 0)
		for _, instance := range byService[serviceName] {
			if evaluateAt.After(instance.HeartbeatAt.Add(timeout)) {
				continue
			}
			if !instance.Healthy {
				continue
			}
			candidates = append(candidates, instanceJSON(instance))
		}
		sortDiscoverHits(candidates)
		services = append(services, gin.H{
			"service_name": serviceName,
			"instances":    candidates,
		})
	}
	c.JSON(200, gin.H{
		"services":          services,
		"evaluate_at":       evaluateAt.Format(time.RFC3339Nano),
		"heartbeat_timeout": jsonWeight(timeout.Seconds()),
	})
}

// handleCleanup implements the standalone lost-instance cleanup entry. An
// omitted service_name evaluates every service; an explicit service_name
// must be non-empty and scopes the cleanup to that one service, leaving
// every other service untouched. Only records strictly past the lost
// boundary are deleted: records exactly on the boundary, still-online
// records and unhealthy-but-fresh records are all kept. Every parameter is
// validated before any deletion, and the deletion itself runs in one
// transaction so a storage failure leaves no partial result.
func (s *Server) handleCleanup(c *gin.Context) {
	bag, apiErr := buildParamBag(c)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}

	serviceName := ""
	scoped := false
	if value, present := bag.get(serviceNameKeys); present {
		serviceName = strings.TrimSpace(stringValue(value))
		if serviceName == "" {
			writeError(c, errInvalidParameter("service_name must not be empty"))
			return
		}
		scoped = true
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

	var instances []store.Instance
	var err error
	if scoped {
		instances, err = s.store.ListInstances(serviceName)
	} else {
		instances, err = s.store.ListAllInstances()
	}
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

	lost := make([]store.Instance, 0)
	keys := make([]store.InstanceKey, 0)
	for _, instance := range instances {
		if evaluateAt.After(instance.HeartbeatAt.Add(timeout)) {
			lost = append(lost, instance)
			keys = append(keys, store.InstanceKey{
				ServiceName: instance.ServiceName,
				InstanceID:  instance.InstanceID,
			})
		}
	}
	if len(keys) > 0 {
		if err := s.store.DeleteInstances(keys); err != nil {
			writeError(c, errStorageUnavailable())
			return
		}
	}

	sort.SliceStable(lost, func(i, j int) bool {
		if lost[i].ServiceName != lost[j].ServiceName {
			return lost[i].ServiceName < lost[j].ServiceName
		}
		return lost[i].InstanceID < lost[j].InstanceID
	})
	removed := make([]gin.H, 0, len(lost))
	for _, instance := range lost {
		removed = append(removed, instanceJSON(instance))
	}
	c.JSON(200, gin.H{
		"evaluate_at":       evaluateAt.Format(time.RFC3339Nano),
		"heartbeat_timeout": jsonWeight(timeout.Seconds()),
		"removed":           len(removed),
		"instances":         removed,
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
