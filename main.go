package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	// Kubernetes imports
	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// Max length for output sent to Process Tracking message field
	maxProcessTrackingMessageLength = 1000
)

// ParameterOption defines the structure for options within a script's *top-level* parameter definition
// (Used if the script itself is presented like a parameter with predefined choices, like in the previous Go structure)
// This might become less relevant with the new response structure but keep for loading compatibility for now.
type ParameterOption struct {
	Name string `json:"name"` // Required
	ID   string `json:"id"`   // Required
}

// InputParameterDef defines the structure for parameters accepted *by* a script
// Used in the nested "parameters" array in the new desired response structure
type InputParameterDef struct {
	Name        string `json:"name"`           // Required
	Type        string `json:"type,omitempty"` // Required (Defaults to string if omitted? TBC)
	Description string `json:"description,omitempty"`
	Optional    bool   `json:"optional,omitempty"`
	// Add other fields seen in Java example if needed (e.g., dataset_id?)
}

// ScriptDefinition holds the combined definition loaded from scripts.json
type ScriptDefinition struct {
	// Fields for identifying the script and its command
	ID      string `json:"id,omitempty"` // Optional - will be auto-generated from name if not provided
	Name    string `json:"name"`         // Required (This will be the top-level "name" in the response)
	Command string `json:"command"`      // Required

	// Input parameters the script accepts
	Parameters []InputParameterDef `json:"parameters,omitempty"`

	// Process tracking fields
	Stage          string `json:"stage,omitempty"`          // Process tracking stage for this script
	MonitorProcess bool   `json:"monitorProcess,omitempty"` // Whether to monitor this script with process tracking

	// Optional descriptive fields (Not directly used in new response structure but maybe useful internally)
	Description string            `json:"description,omitempty"`
	Label       string            `json:"label,omitempty"`
	Type        string            `json:"type,omitempty"`     // Top-level type if script itself is treated like a parameter
	Optional    bool              `json:"optional,omitempty"` // Top-level optional flag
	Options     []ParameterOption `json:"options,omitempty"`  // Top-level options
}

// ScriptResponse is the structure returned by the /v1/options endpoint (matching Java example)
type ScriptResponse struct {
	Name       string              `json:"name"`
	Parameters []InputParameterDef `json:"parameters"`
}

// TaskServiceRequest defines the structure expected from the calling Task Service
type TaskServiceRequest struct {
	TaskName    string                 `json:"taskName"`
	LastRunTime int64                  `json:"lastRunTime"` // Changed type to int64 to accept number
	TrackingID  string                 `json:"trackingId"`
	TaskData    map[string]interface{} `json:"taskData"` // Use interface{} for flexible value types
}

// ProcessTrackingCreatePayload sent to initially create a process tracking record
// Based on Java ProcessCreationDTO
type ProcessTrackingCreatePayload struct {
	Name       string `json:"name"`      // Script Name
	TrackingID string `json:"processId"` // Mapped from X-Tracking-Id header
	Stage      string `json:"stage"`     // From config
	// Group, Label, Status removed
}

// ProcessTrackingCreateResponse - Not needed as we generate ID locally
// type ProcessTrackingCreateResponse struct { ... }

// ProcessTrackingUpdatePayload sent to update an existing process tracking record
// Based on Java ProcessUpdateDTO
type ProcessTrackingUpdatePayload struct {
	Status       string `json:"status"` // SUCCESSFUL, FAILED, PROGRESS
	Message      string `json:"message,omitempty"`
	MessageLevel string `json:"messageLevel"` // Added: INFO or ERROR based on Status
	// TrackingID removed
}

// Config holds application configuration
type Config struct {
	ScriptsPath      string
	PodLabelSelector string
	Namespace        string
	// Process Tracking Config
	ProcessTrackingURL   string
	ProcessTrackingStage string
	ProcessTrackingGroup string
}

// Load configuration from environment variables with fallbacks
func loadConfig() *Config {
	return &Config{
		ScriptsPath:          getEnvOrDefault("SCRIPTS_PATH", "/config/scripts.json"),
		PodLabelSelector:     getEnvOrDefault("POD_LABEL_SELECTOR", "app=query-server"),
		Namespace:            getEnvOrDefault("NAMESPACE", "default"),
		ProcessTrackingURL:   os.Getenv("PROCESS_TRACKING_SERVICE_URL"),                    // Mandatory? Add check if so.
		ProcessTrackingStage: getEnvOrDefault("PROCESS_TRACKING_STAGE", "EXECUTION"),       // Example default
		ProcessTrackingGroup: getEnvOrDefault("PROCESS_TRACKING_GROUP", "ScriptExecution"), // Example default
	}
}

// Get environment variable with fallback
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// loadScriptDefinitions reads, parses, and validates the scripts definition file.
func loadScriptDefinitions(filePath string) ([]ScriptDefinition, error) {
	file, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read script definitions file '%s': %v", filePath, err)
	}

	var definitions []ScriptDefinition
	err = json.Unmarshal(file, &definitions)
	if err != nil {
		return nil, fmt.Errorf("failed to parse script definitions JSON from '%s': %v", filePath, err)
	}

	// Validate definitions
	for i := range definitions {
		// Validate top-level required fields and set default ID if needed
		if definitions[i].ID == "" {
			// Generate an ID based on name if not provided
			definitions[i].ID = strings.ToLower(strings.ReplaceAll(definitions[i].Name, " ", "-"))
			log.Printf("Auto-generated ID '%s' for script definition with name '%s'", definitions[i].ID, definitions[i].Name)
		}

		if definitions[i].Name == "" {
			return nil, fmt.Errorf("script definition %d (id: %s) in '%s' is missing required 'name' field", i, definitions[i].ID, filePath)
		}
		if definitions[i].Command == "" {
			return nil, fmt.Errorf("script definition %d (id: %s) in '%s' is missing required 'command' field", i, definitions[i].ID, filePath)
		}

		// Validate nested Parameters
		for j, param := range definitions[i].Parameters {
			if param.Name == "" {
				return nil, fmt.Errorf("input parameter %d for script '%s' in '%s' is missing required 'name' field", j, definitions[i].ID, filePath)
			}
			// Optional: Validate or default param.Type if needed
			if param.Type == "" {
				// Decide: either error out or default it
				definitions[i].Parameters[j].Type = "string" // Example: Defaulting to string
				// return nil, fmt.Errorf("input parameter '%s' for script '%s' in '%s' is missing required 'type' field", param.Name, definitions[i].ID, filePath)
			}
		}

		// Retain validation for top-level options if they are still used/defined
		for j, option := range definitions[i].Options {
			if option.ID == "" {
				return nil, fmt.Errorf("top-level option %d for script definition '%s' in '%s' is missing required 'id' field", j, definitions[i].ID, filePath)
			}
			if option.Name == "" {
				return nil, fmt.Errorf("top-level option %d (id: %s) for script definition '%s' in '%s' is missing required 'name' field", j, option.ID, definitions[i].ID, filePath)
			}
		}
	}

	return definitions, nil
}

// Get the first pod matching the label selector (used by executeScript)
func getTargetPod(namespace, labelSelector string) (string, error) {
	cmd := exec.Command("sh", "-c", fmt.Sprintf("kubectl get pods -n %s -l %s -o jsonpath='{.items[0].metadata.name}'", namespace, labelSelector))
	out, err := cmd.Output()
	if err != nil {
		// Improve error logging
		stderr := ""
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = string(exitErr.Stderr)
		}
		return "", fmt.Errorf("failed to get pod (namespace: %s, selector: %s): %v, stderr: %s", namespace, labelSelector, err, stderr)
	}
	podName := strings.TrimSpace(string(out))
	if podName == "" {
		return "", fmt.Errorf("no pod found matching label selector: %s in namespace %s", labelSelector, namespace)
	}
	return podName, nil
}

// listScripts handles the /v1/options endpoint.
// It loads script definitions and returns them in the Java service's format.
func listScripts(c *gin.Context) {
	config := loadConfig()

	definitions, err := loadScriptDefinitions(config.ScriptsPath)
	if err != nil {
		log.Printf("Error loading script definitions: %v", err)
		statusCode := http.StatusInternalServerError
		if os.IsNotExist(err) {
			log.Printf("Script definitions file not found at %s", config.ScriptsPath)
		}
		// On error, return status code and an empty JSON array body "[]"
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.String(statusCode, "[]")
		return
	}

	// Create the response structure matching the Java service
	scriptResponses := make([]ScriptResponse, len(definitions))
	for i, def := range definitions {
		// Ensure Parameters is not nil if Parameters is empty
		params := def.Parameters
		if params == nil {
			params = []InputParameterDef{} // Return empty array instead of null
		}
		scriptResponses[i] = ScriptResponse{
			Name:       def.Name,
			Parameters: params,
		}
	}

	// Manually marshal the new response structure to JSON bytes
	jsonData, err := json.Marshal(scriptResponses)
	if err != nil {
		log.Printf("Error marshaling script responses to JSON: %v", err)
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.String(http.StatusInternalServerError, "[]")
		return
	}

	// Set Content-Type and write raw JSON bytes
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Data(http.StatusOK, "application/json", jsonData)
}

// --- Process Tracking Helpers ---
var httpClient = &http.Client{Timeout: 10 * time.Second}

// notifyProcessTrackingCreate sends the initial creation request SYNCHRONOUSLY
// and returns the numeric ProcessID from the response header.
func notifyProcessTrackingCreate(config *Config, payload ProcessTrackingCreatePayload) (int64, error) {
	if config.ProcessTrackingURL == "" {
		log.Printf("[ProcessTracking CREATE] Skipping creation for TrackingID %s: PROCESS_TRACKING_SERVICE_URL not set.", payload.TrackingID)
		return 0, fmt.Errorf("process tracking URL not configured") // Return error as creation is required
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[ProcessTracking CREATE] Error marshaling payload for TrackingID %s: %v", payload.TrackingID, err)
		return 0, fmt.Errorf("failed to marshal create payload: %w", err)
	}

	// POST to base URL
	req, err := http.NewRequest("POST", config.ProcessTrackingURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		log.Printf("[ProcessTracking CREATE] Error creating request for TrackingID %s: %v", payload.TrackingID, err)
		return 0, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// TODO: Add Cookie header if needed, based on Java impl: headers.set(HttpHeaders.COOKIE, "rights=1; rights_0=" + cookie);

	log.Printf("[ProcessTracking CREATE] Sending creation request for Name: %s, TrackingID: %s, Stage: %s", payload.Name, payload.TrackingID, payload.Stage)
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("[ProcessTracking CREATE] Error sending notification for TrackingID %s: %v", payload.TrackingID, err)
		return 0, fmt.Errorf("failed to send create request: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, readErr := ioutil.ReadAll(resp.Body) // Read body for logging context
	if readErr != nil {
		log.Printf("[ProcessTracking CREATE] Failed to read response body for TrackingID %s after status %d: %v", payload.TrackingID, resp.StatusCode, readErr)
		// Still might have the header, but log the read error
	}

	// Expect 201 CREATED
	if resp.StatusCode != http.StatusCreated {
		log.Printf("[ProcessTracking CREATE] Notification failed for TrackingID %s: Expected Status 201, Got %d, Body: %s", payload.TrackingID, resp.StatusCode, string(bodyBytes))
		return 0, fmt.Errorf("create request failed with status %d", resp.StatusCode)
	}

	// Get numeric ID from 'processid' header
	processIDHeader := resp.Header.Get("processid")
	if processIDHeader == "" {
		log.Printf("[ProcessTracking CREATE] Notification success (Status 201) but 'processid' header missing or empty for TrackingID %s. Body: %s", payload.TrackingID, string(bodyBytes))
		return 0, fmt.Errorf("'processid' header missing in create response")
	}

	numericProcessID, parseErr := strconv.ParseInt(processIDHeader, 10, 64)
	if parseErr != nil {
		log.Printf("[ProcessTracking CREATE] Failed to parse 'processid' header value '%s' to int64 for TrackingID %s: %v", processIDHeader, payload.TrackingID, parseErr)
		return 0, fmt.Errorf("failed to parse 'processid' header: %w", parseErr)
	}

	if numericProcessID == 0 {
		// This case might be valid depending on the backend, but log a warning
		log.Printf("[ProcessTracking CREATE] Warning: Received 'processid' header value was 0 for TrackingID %s.", payload.TrackingID)
	}

	log.Printf("[ProcessTracking CREATE] Notification successful for TrackingID %s. Received numeric ProcessID: %d", payload.TrackingID, numericProcessID)
	return numericProcessID, nil // Return the numeric ID from header
}

// notifyProcessTrackingUpdate sends the final status update using the numeric ProcessID obtained from creation.
func notifyProcessTrackingUpdate(config *Config, numericProcessID int64, payload ProcessTrackingUpdatePayload) {
	// Skip if URL not set OR if the numericProcessID is zero (indicating creation failed or header was missing/invalid)
	if config.ProcessTrackingURL == "" || numericProcessID == 0 {
		log.Printf("[ProcessTracking UPDATE] Skipping notification for numeric ProcessID %d: URL not set or ProcessID is zero.", numericProcessID)
		return
	}

	// Determine MessageLevel based on Status
	switch strings.ToUpper(payload.Status) {
	case "FAILED":
		payload.MessageLevel = "ERROR"
	default: // PROGRESS, SUCCESSFUL, COMPLETED, etc.
		payload.MessageLevel = "INFO"
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[ProcessTracking UPDATE] Error marshaling payload for numeric ProcessID %d: %v", numericProcessID, err)
		return
	}

	// Construct the specific update URL using the numeric ID
	processIDStr := strconv.FormatInt(numericProcessID, 10)
	updateURL := strings.TrimSuffix(config.ProcessTrackingURL, "/") + "/" + processIDStr

	// POST to /{id}
	req, err := http.NewRequest("POST", updateURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		log.Printf("[ProcessTracking UPDATE] Error creating request for numeric ProcessID %d: %v", numericProcessID, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// TODO: Add Cookie header if needed

	log.Printf("[ProcessTracking UPDATE] Sending status '%s' (Level: %s) for numeric ProcessID %d to %s", payload.Status, payload.MessageLevel, numericProcessID, updateURL)
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("[ProcessTracking UPDATE] Error sending notification for numeric ProcessID %d: %v", numericProcessID, err)
		return
	}
	defer resp.Body.Close()

	// Expect 200 OK
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := ioutil.ReadAll(resp.Body)
		log.Printf("[ProcessTracking UPDATE] Notification failed for numeric ProcessID %d: Expected Status 200, Got %d, Body: %s", numericProcessID, resp.StatusCode, string(bodyBytes))
	} else {
		log.Printf("[ProcessTracking UPDATE] Notification successful for numeric ProcessID %d (Status: %s)", numericProcessID, payload.Status)
	}
}

// executeScript handles the /v1/execute endpoint, integrating Process Tracking.
func executeScript(c *gin.Context) {
	config := loadConfig()
	var request TaskServiceRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body: " + err.Error()})
		return
	}

	// --- Use Tracking ID from Request BODY ---
	bodyTrackingID := request.TrackingID
	if bodyTrackingID == "" {
		// Generate a unique tracking ID if not provided - using timestamp
		bodyTrackingID = fmt.Sprintf("%d", time.Now().UnixNano())
		log.Printf("Auto-generated TrackingID '%s' because request TrackingID was empty.", bodyTrackingID)
	}
	log.Printf("Received execute request. Body TrackingID: '%s'", bodyTrackingID)

	// Extract actual script name
	scriptNameInterface, nameOk := request.TaskData["name"]
	if !nameOk {
		log.Printf("ERROR: taskData is missing the 'name' field. TrackingID: %s", bodyTrackingID)
		c.JSON(http.StatusBadRequest, gin.H{"error": "taskData must contain a 'name' field specifying the script to run"})
		return
	}
	actualScriptName, nameIsString := scriptNameInterface.(string)
	if !nameIsString || actualScriptName == "" {
		log.Printf("ERROR: taskData 'name' field is not a non-empty string ('%v'). TrackingID: %s", scriptNameInterface, bodyTrackingID)
		c.JSON(http.StatusBadRequest, gin.H{"error": "taskData 'name' field must be a non-empty string"})
		return
	}

	// Load script definitions - need to do this earlier to access the script's stage
	definitions, err := loadScriptDefinitions(config.ScriptsPath)
	if err != nil {
		log.Printf("Error loading script definitions during execute: %v, TrackingID: %s", err, bodyTrackingID)
		statusCode := http.StatusInternalServerError
		errMsgStr := fmt.Sprintf("Failed to load script definitions: %v", err)
		if os.IsNotExist(err) {
			errMsgStr = fmt.Sprintf("Server configuration error: Script definitions file not found at %s", config.ScriptsPath)
		}
		c.JSON(statusCode, gin.H{"error": errMsgStr})
		return
	}

	// Find the requested script definition
	var selectedDefinition *ScriptDefinition
	for i := range definitions {
		// Match against the name extracted from taskData.name
		if definitions[i].Name == actualScriptName {
			selectedDefinition = &definitions[i]
			break
		}
	}

	if selectedDefinition == nil {
		log.Printf("Execute request failed: Script with name '%s' (from taskData) not found in definitions. TrackingID: %s", actualScriptName, bodyTrackingID)
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Script '%s' not found", actualScriptName)})
		return
	}

	log.Printf("Found definition for script '%s' (ID: %s). TrackingID: %s", selectedDefinition.Name, selectedDefinition.ID, bodyTrackingID)

	// Skip process tracking if monitorProcess is explicitly set to false
	if !selectedDefinition.MonitorProcess {
		log.Printf("Process tracking disabled for script '%s', skipping tracking. TrackingID: %s", selectedDefinition.Name, bodyTrackingID)
	}

	// --- Process Tracking Start ---
	var numericProcessID int64 = 0
	if selectedDefinition.MonitorProcess || selectedDefinition.MonitorProcess == false /* default to true if not specified */ {
		// Determine stage to use: prefer script-specific stage if provided, fall back to config
		stage := config.ProcessTrackingStage // Default from config
		if selectedDefinition.Stage != "" {
			stage = selectedDefinition.Stage // Override with script-specific stage
			log.Printf("Using script-specific stage '%s' for process tracking. TrackingID: %s", stage, bodyTrackingID)
		}

		// Create the process record SYNCHRONOUSLY to get the numeric ID from the header
		var createErr error
		numericProcessID, createErr = notifyProcessTrackingCreate(config, ProcessTrackingCreatePayload{
			Name:       request.TaskName,
			TrackingID: bodyTrackingID,
			Stage:      stage, // Use script-specific stage or config default
		})

		if createErr != nil {
			// Log the creation error and fail the request
			log.Printf("ERROR: Failed to create initial process tracking record for script '%s', Body TrackingID '%s': %v", actualScriptName, bodyTrackingID, createErr)
			// Do NOT send an update notification here, as creation failed.
			// Return a server error. Do not set X-ProcessId header as we didn't get one.
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to initialize process tracking: %v", createErr)})
			return
		}

		// If we reach here, creation was successful and numericProcessID holds the ID from the header.
		log.Printf("Successfully created process tracking record. Numeric ProcessID: %d", numericProcessID)

		// Send a 'PROGRESS' update immediately after successful creation
		notifyProcessTrackingUpdate(config, numericProcessID, ProcessTrackingUpdatePayload{
			Status:  "PROGRESS",
			Message: "Script execution starting",
			// MessageLevel will be set to INFO inside notifyProcessTrackingUpdate
		})
	}

	// --- Resume normal execution flow ---
	log.Printf("Extracted actual script name '%s' from taskData. TrackingID: %s", actualScriptName, request.TrackingID)

	// Get the target pod
	targetPod, err := getTargetPod(config.Namespace, config.PodLabelSelector)
	if err != nil {
		log.Printf("Execute request failed for script '%s': Could not get target pod: %v. TrackingID: %s", selectedDefinition.Name, err, request.TrackingID)
		// Send FAILED status UPDATE using the OBTAINED numeric ID if process tracking is enabled
		if numericProcessID > 0 {
			notifyProcessTrackingUpdate(config, numericProcessID, ProcessTrackingUpdatePayload{Status: "FAILED", Message: fmt.Sprintf("Failed to find target pod: %v", err)})
			// Set Header and return error response
			c.Header("X-ProcessId", strconv.FormatInt(numericProcessID, 10))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to find target pod: %v", err)})
		return
	}

	log.Printf("Target pod for script '%s' execution: %s (Namespace: %s, Selector: %s). TrackingID: %s", selectedDefinition.Name, targetPod, config.Namespace, config.PodLabelSelector, request.TrackingID)

	// Prepare environment variables by extracting values from taskData based on script's Parameters
	envPrefix := ""
	if len(selectedDefinition.Parameters) > 0 {
		var envVars []string
		log.Printf("Processing %d parameters for script '%s'. TrackingID: %s", len(selectedDefinition.Parameters), selectedDefinition.Name, request.TrackingID)

		// Log all available taskData keys to help debugging
		var taskDataKeys []string
		for k := range request.TaskData {
			taskDataKeys = append(taskDataKeys, k)
		}
		log.Printf("Available taskData keys for script '%s': %v. TrackingID: %s",
			selectedDefinition.Name, taskDataKeys, bodyTrackingID)

		for _, paramDef := range selectedDefinition.Parameters {
			log.Printf("Looking for parameter '%s' (optional: %v) in taskData. TrackingID: %s",
				paramDef.Name, paramDef.Optional, bodyTrackingID)

			paramValueInterface, valueOk := request.TaskData[paramDef.Name] // Look for key matching paramDef.Name in taskData

			if !valueOk {
				// Handle missing parameter value - check if it was optional in definition
				if !paramDef.Optional {
					log.Printf("Execute request failed for script '%s': Required parameter '%s' missing in taskData. TrackingID: %s", selectedDefinition.Name, paramDef.Name, request.TrackingID)
					// Send FAILED status UPDATE using the OBTAINED numeric ID if process tracking is enabled
					if numericProcessID > 0 {
						notifyProcessTrackingUpdate(config, numericProcessID, ProcessTrackingUpdatePayload{Status: "FAILED", Message: fmt.Sprintf("Required parameter '%s' missing", paramDef.Name)})
						// Set Header (using OBTAINED numericProcessID)
						c.Header("X-ProcessId", strconv.FormatInt(numericProcessID, 10))
					}
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Required parameter '%s' is missing in taskData", paramDef.Name)})
					return
				} else {
					// Optional parameter is missing, skip setting env var for it
					log.Printf("Optional parameter '%s' for script '%s' missing in taskData, skipping. TrackingID: %s", paramDef.Name, selectedDefinition.Name, request.TrackingID)
					continue
				}
			}

			// Log the value type for debugging
			valueType := fmt.Sprintf("%T", paramValueInterface)
			log.Printf("Found parameter '%s' with value type '%s'. TrackingID: %s",
				paramDef.Name, valueType, bodyTrackingID)

			// Convert value to string
			paramValueStr := fmt.Sprintf("%v", paramValueInterface)

			// Sanitize the DEFINED parameter name for use as an env var key
			envVarName := sanitizeEnvVarName(paramDef.Name)
			if !isValidEnvVarName(envVarName) {
				// This should ideally not happen if sanitizeEnvVarName is robust
				log.Printf("Internal Error for script '%s': Sanitized parameter name '%s' (from '%s') is invalid. TrackingID: %s", selectedDefinition.Name, envVarName, paramDef.Name, request.TrackingID)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error processing parameter names", "trackingId": request.TrackingID})
				return
			}

			// Quote the string value for shell safety
			quotedValue := fmt.Sprintf("%q", paramValueStr)
			envVars = append(envVars, fmt.Sprintf("%s=%s", envVarName, quotedValue))
		}

		if len(envVars) > 0 {
			envPrefix = strings.Join(envVars, " ") + " "
			log.Printf("Prepared environment variables for script '%s': %s. TrackingID: %s", selectedDefinition.Name, strings.TrimSpace(envPrefix), request.TrackingID)
		}
	}

	// Construct the command
	fullCommand := envPrefix + selectedDefinition.Command
	execCmd := fmt.Sprintf("kubectl exec -n %s %s -- /bin/bash -c '%s'",
		config.Namespace,
		targetPod,
		fullCommand,
	)
	log.Printf("Constructed kubectl command for script '%s': %s. TrackingID: %s", selectedDefinition.Name, execCmd, request.TrackingID)

	// Execute command
	cmd := exec.Command("sh", "-c", execCmd)
	log.Printf("Executing command for script '%s' in pod '%s'... TrackingID: %s", selectedDefinition.Name, targetPod, request.TrackingID)

	output, err := cmd.CombinedOutput()
	outputStr := string(output)
	truncatedOutput := outputStr
	if len(truncatedOutput) > maxProcessTrackingMessageLength {
		truncatedOutput = truncatedOutput[:maxProcessTrackingMessageLength] + "... (truncated)"
	}

	if err != nil {
		errMsgStr := fmt.Sprintf("Execution error: %v", err)
		log.Printf("Execution FAILED for script '%s' (ID: %s) in pod '%s'. TrackingID: %s. Error: %v. Output: %s", selectedDefinition.Name, selectedDefinition.ID, targetPod, request.TrackingID, err, outputStr)
		// Send FAILED status UPDATE using the OBTAINED numeric ID if process tracking is enabled
		if numericProcessID > 0 {
			notifyProcessTrackingUpdate(config, numericProcessID, ProcessTrackingUpdatePayload{
				Status:  "FAILED",
				Message: fmt.Sprintf("%s\n--- Output ---\n%s", errMsgStr, truncatedOutput),
			})
			// Set Header (using OBTAINED numericProcessID)
			c.Header("X-ProcessId", strconv.FormatInt(numericProcessID, 10))
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"taskName":  actualScriptName,
			"script_id": selectedDefinition.ID,
			"error":     errMsgStr,
			"output":    outputStr,
		})
		return
	}

	// --- Execution Successful ---
	log.Printf("Execution SUCCESSFUL for script '%s' (ID: %s) in pod '%s'. TrackingID: %s. Output: %s", selectedDefinition.Name, selectedDefinition.ID, targetPod, request.TrackingID, outputStr)
	// Send COMPLETED/SUCCESSFUL status UPDATE using the OBTAINED numeric ID if process tracking is enabled
	if numericProcessID > 0 {
		notifyProcessTrackingUpdate(config, numericProcessID, ProcessTrackingUpdatePayload{
			Status:  "SUCCESSFUL", // Changed from COMPLETED to SUCCESSFUL
			Message: truncatedOutput,
		})
		// Set Header (using OBTAINED numericProcessID)
		c.Header("X-ProcessId", strconv.FormatInt(numericProcessID, 10))
	}
	// Return status OK with ONLY the header and NO body
	c.Status(http.StatusOK)
}

// isValidEnvVarName checks if a string is a valid environment variable name
// (typically alphanumeric + underscore, not starting with a number).
// Basic check, can be refined.
func isValidEnvVarName(name string) bool {
	if name == "" {
		return false
	}
	// Simple check: Allow A-Z, a-z, 0-9, _
	// More robust check would use regex: ^[a-zA-Z_][a-zA-Z0-9_]*$
	for i, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r == '_') || (r >= '0' && r <= '9' && i > 0)) {
			return false
		}
	}
	return true
}

// sanitizeEnvVarName converts a parameter name into a potentially valid env var name
// Replaces spaces and invalid characters with underscores.
// WARNING: This is basic; ensure it doesn't cause collisions and meets shell requirements.
func sanitizeEnvVarName(name string) string {
	// Replace common invalid chars with underscore
	replaced := regexp.MustCompile(`[^a-zA-Z0-9_]+`).ReplaceAllString(name, "_")
	// Ensure it doesn't start with a number
	if len(replaced) > 0 && replaced[0] >= '0' && replaced[0] <= '9' {
		replaced = "_" + replaced
	}
	// Optional: Convert to uppercase?
	// return strings.ToUpper(replaced)
	return replaced
}

// checkPermissions verifies if the service account has the required RBAC permissions.
func checkPermissions(clientset *kubernetes.Clientset, namespace string) error {
	log.Printf("Checking required Kubernetes permissions in namespace '%s'...", namespace)

	requiredPermissions := []struct {
		verb        string
		resource    string
		subresource string
		description string
	}{
		{"get", "pods", "", "Get Pods"},
		{"create", "pods", "exec", "Create Pods/Exec"},
	}

	for _, perm := range requiredPermissions {
		ssar := &authv1.SelfSubjectAccessReview{
			Spec: authv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authv1.ResourceAttributes{
					Namespace:   namespace,
					Verb:        perm.verb,
					Resource:    perm.resource,
					Subresource: perm.subresource,
				},
			},
		}

		result, err := clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(context.TODO(), ssar, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to perform self subject access review for %s: %v", perm.description, err)
		}

		if !result.Status.Allowed {
			log.Printf("Permission check FAILED: '%s' permission is DENIED. Reason: %s", perm.description, result.Status.Reason)
			return fmt.Errorf("missing required Kubernetes permission: %s in namespace %s. Reason: %s", perm.description, namespace, result.Status.Reason)
		} else {
			log.Printf("Permission check PASSED: '%s' permission is allowed.", perm.description)
		}
	}

	log.Printf("All required Kubernetes permissions verified successfully in namespace '%s'.", namespace)
	return nil
}

// healthzHandler handles the /healthz endpoint.
func healthzHandler(c *gin.Context) {
	// Simple health check - relies on the startup permission check having passed.
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func main() {
	config := loadConfig()
	log.Printf("Starting server with configuration:")
	log.Printf("- Scripts Definition Path: %s", config.ScriptsPath)
	log.Printf("- Pod Label Selector: %s", config.PodLabelSelector)
	log.Printf("- Namespace: %s", config.Namespace)

	// --- Kubernetes Client Setup ---
	log.Println("Initializing Kubernetes client...")
	k8sConfig, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("Failed to get in-cluster Kubernetes config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes clientset: %v", err)
	}
	log.Println("Kubernetes client initialized successfully.")

	// --- Startup Permission Check ---
	if err := checkPermissions(clientset, config.Namespace); err != nil {
		// Log fatal will exit the program
		log.Fatalf("Startup failed due to missing permissions: %v", err)
	}

	// --- Gin Router Setup ---
	r := gin.Default()

	// Define API routes
	r.GET("/v1/options", listScripts)
	r.POST("/v1/execute", executeScript)
	r.GET("/healthz", healthzHandler) // Add health check endpoint

	// Start server on port 8080
	port := "8080"
	log.Printf("Starting server on port %s...", port)
	if err := r.Run("0.0.0.0:" + port); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
