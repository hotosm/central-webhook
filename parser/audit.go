package parser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
)

// ** Entities ** //
// Define the nested structs for the details field
type OdkEntityRef struct {
	Uuid    string `json:"uuid"` // Use string for UUID, as it may be 'uuid:xxx-xxx'
	Dataset string `json:"dataset"`
}

type OdkEntityDetails struct {
	Entity      OdkEntityRef `json:"entity"`
	EntityId    int          `json:"entityId"`
	EntityDefId int          `json:"entityDefId"`
}

// ** Submissions ** //

type OdkSubmissionDetails struct {
	InstanceId string `json:"instanceId"` // Use string for UUID, as it may be 'uuid:xxx-xxx'
	// The submissionId field is present, but it's a database reference only, so we ignore it
	// SubmissionId    int    `json:"submissionId"`
	SubmissionDefId int `json:"submissionDefId"`
}

// ** High level wrapper structs ** //

// OdkAuditLog represents the main structure for the audit log (returned by pg_notify)
type OdkAuditLog struct {
	Notes   *string     `json:"notes"` // Pointer to handle null values
	Action  string      `json:"action"`
	ActeeID string      `json:"acteeId"` // Use string for UUID
	ActorID int         `json:"actorId"`
	Details interface{} `json:"details"` // Use an interface to handle different detail types
	Data    interface{} `json:"data"`    // Use an interface to handle different data types

	// New fields for http extension
	HttpSent  bool   `json:"http_sent"`  // Indicates whether it has already been sent via HTTP
	Truncated bool   `json:"truncated"`  // Indicates whether the payload was truncated
	DmlAction string `json:"dml_action"` // INSERT, UPDATE, DELETE
}

// ProcessedEvent represents the final parsed event structure (to send to the webhook API)
type ProcessedEvent struct {
	Type string      `json:"type"` // The event type, entity update or new submission
	ID   string      `json:"id"`   // Entity UUID or Submission InstanceID
	Data interface{} `json:"data"` // The actual entity data or wrapped submission XML

	// New fields for http extension
	HttpSent  bool `json:"http_sent"`
	Truncated bool `json:"truncated"` // Used for monitoring in main
}

// ParseJsonString converts the pg_notify string to OdkAuditLog
func ParseJsonString(log *slog.Logger, data []byte) (*OdkAuditLog, error) {
	if len(data) == 0 {
		return nil, errors.New("empty input data")
	}

	var parsedData OdkAuditLog
	if err := json.Unmarshal(data, &parsedData); err != nil {
		log.Error("failed to parse JSON data", "error", err, "data", string(data))
		return nil, err
	}
	log.Debug("parsed notification data", "data", parsedData)
	return &parsedData, nil
}

// ParseEventJson parses the JSON data and extracts the relevant ID and data fields
func ParseEventJson(log *slog.Logger, ctx context.Context, data []byte) (*ProcessedEvent, error) {
	// Convert the raw pg_notify string to an OdkAuditLog
	rawLog, err := ParseJsonString(log, data)
	if err != nil {
		return nil, err
	}

	// Prepare the result structure
	var processedEvent ProcessedEvent

	// Hybrid fields to notify from HTTP_POST
	processedEvent.HttpSent = rawLog.HttpSent
	processedEvent.Truncated = rawLog.Truncated

	// If this event was already sent via HTTP, it need basic info for monitoring
	if rawLog.HttpSent {
		processedEvent.Type = rawLog.Action
		log.Debug("processing monitoring event (already sent via HTTP)", "action", rawLog.Action)
		return &processedEvent, nil
	}

	// Parse the details field based on the action
	switch rawLog.Action {
	case "entity.update.version":
		var entityDetails OdkEntityDetails
		if err := parseDetails(rawLog.Details, &entityDetails); err != nil {
			log.Error("failed to parse entity.update.version details", "error", err)
			return nil, err
		}
		processedEvent.Type = "entity.update.version"
		processedEvent.ID = entityDetails.Entity.Uuid
		processedEvent.Data = rawLog.Data

	case "submission.create":
		var submissionDetails OdkSubmissionDetails
		if err := parseDetails(rawLog.Details, &submissionDetails); err != nil {
			log.Error("failed to parse submission.create details", "error", err)
			return nil, err
		}
		processedEvent.Type = "submission.create"
		processedEvent.ID = submissionDetails.InstanceId

		// Parse the raw XML data
		rawData, ok := rawLog.Data.(map[string]interface{})
		if !ok {
			log.Error("invalid data type for submission.create", "data", rawLog.Data)
			return nil, errors.New("invalid data type for submission.create")
		}
		processedEvent.Data = rawData

	case "submission.update":
		var submissionDetails OdkSubmissionDetails
		if err := parseDetails(rawLog.Details, &submissionDetails); err != nil {
			log.Error("failed to parse submission.update details", "error", err)
			return nil, err
		}
		processedEvent.Type = "submission.update"
		processedEvent.ID = submissionDetails.InstanceId
		processedEvent.Data = rawLog.Data

	default:
		// No nothing if the event type is not supported
		log.Warn("unsupported action type", "action", rawLog.Action)
		return nil, fmt.Errorf("unsupported action type")
	}

	log.Debug("parsed event successfully", "processedEvent", processedEvent)
	return &processedEvent, nil
}

// parseDetails helps to unmarshal the details field into the appropriate structure
func parseDetails(details interface{}, target interface{}) error {
	detailsBytes, err := json.Marshal(details)
	if err != nil {
		return err
	}
	return json.Unmarshal(detailsBytes, target)
}
