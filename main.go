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
	ID      string `json:"id"`      // Required
	Name    string `json:"name"`    // Required (This will be the top-level "name" in the response)
	Command string `json:"command"` // Required

	// Input parameters the script accepts (new field)
	AcceptedParameters []InputParameterDef `json:"acceptedParameters,omitempty"`

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
type ProcessTrackingCreatePayload struct {
	Name       string `json:"name"` // Script Name
	Stage      string `json:"stage"`
	Group      string `json:"group"`
	Label      string `json:"label,omitempty"`
	TrackingID string `json:"trackingId,omitempty"` // Include Task Service Tracking ID for correlation
	// Status: "PROGRESS" // Status is implicitly set on creation by PT service?
}

// ProcessTrackingCreateResponse expected from the tracking service after creation
type ProcessTrackingCreateResponse struct {
	ProcessID int64 `json:"processId"`
	// Include other fields if needed
}

// ProcessTrackingUpdatePayload sent to update an existing process tracking record
type ProcessTrackingUpdatePayload struct {
	Status  string `json:"status"` // e.g., SUCCESSFUL, FAILED
	Message string `json:"message,omitempty"`
	// Add other optional update fields if needed
	// MessageLevel string `json:"messageLevel,omitempty"`
	// CurrentProgress *int `json:"currentProgress,omitempty"` // Use pointer for optional numbers
	// MaxProgress     *int `json:"maxProgress,omitempty"`
	// Metrics map[string]string `json:"metrics,omitempty"`
	// Comment string `json:"comment,omitempty"`
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
		// Validate top-level required fields
		if definitions[i].ID == "" {
			return nil, fmt.Errorf("script definition %d in '%s' is missing required 'id' field", i, filePath)
		}
		if definitions[i].Name == "" {
			return nil, fmt.Errorf("script definition %d (id: %s) in '%s' is missing required 'name' field", i, definitions[i].ID, filePath)
		}
		if definitions[i].Command == "" {
			return nil, fmt.Errorf("script definition %d (id: %s) in '%s' is missing required 'command' field", i, definitions[i].ID, filePath)
		}

		// Validate nested AcceptedParameters
		for j, param := range definitions[i].AcceptedParameters {
			if param.Name == "" {
				return nil, fmt.Errorf("input parameter %d for script '%s' in '%s' is missing required 'name' field", j, definitions[i].ID, filePath)
			}
			// Optional: Validate or default param.Type if needed
			if param.Type == "" {
				// Decide: either error out or default it
				definitions[i].AcceptedParameters[j].Type = "string" // Example: Defaulting to string
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
		// Ensure Parameters is not nil if AcceptedParameters is empty
		params := def.AcceptedParameters
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

// notifyProcessTrackingCreate sends the initial creation request and returns the generated numeric ProcessID
func notifyProcessTrackingCreate(config *Config, payload ProcessTrackingCreatePayload) (int64, error) {
	if config.ProcessTrackingURL == "" {
		log.Printf("[ProcessTracking CREATE] Skipping notification for TrackingID %s: PROCESS_TRACKING_SERVICE_URL not set.", payload.TrackingID)
		return 0, nil // Return 0 ID and no error if skipping
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[ProcessTracking CREATE] Error marshaling payload for TrackingID %s: %v", payload.TrackingID, err)
		return 0, fmt.Errorf("failed to marshal create payload: %w", err)
	}

	req, err := http.NewRequest("POST", config.ProcessTrackingURL, bytes.NewBuffer(payloadBytes)) // Use base URL
	if err != nil {
		log.Printf("[ProcessTracking CREATE] Error creating request for TrackingID %s: %v", payload.TrackingID, err)
		return 0, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	log.Printf("[ProcessTracking CREATE] Sending creation request for Name: %s, TrackingID: %s", payload.Name, payload.TrackingID)
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("[ProcessTracking CREATE] Error sending notification for TrackingID %s: %v", payload.TrackingID, err)
		return 0, fmt.Errorf("failed to send create request: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, readErr := ioutil.ReadAll(resp.Body) // Read body regardless of status
	if readErr != nil {
		log.Printf("[ProcessTracking CREATE] Failed to read response body for TrackingID %s after status %d: %v", payload.TrackingID, resp.StatusCode, readErr)
		// Return error even if status was 2xx because we need the ID
		return 0, fmt.Errorf("failed to read create response body: %w", readErr)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[ProcessTracking CREATE] Notification failed for TrackingID %s: Status %d, Body: %s", payload.TrackingID, resp.StatusCode, string(bodyBytes))
		return 0, fmt.Errorf("create request failed with status %d", resp.StatusCode)
	} else {
		// Attempt to parse the response to get the numeric ID
		var createResp ProcessTrackingCreateResponse
		if err := json.Unmarshal(bodyBytes, &createResp); err != nil {
			log.Printf("[ProcessTracking CREATE] Failed to unmarshal success response body for TrackingID %s: %v. Body: %s", payload.TrackingID, err, string(bodyBytes))
			return 0, fmt.Errorf("failed to parse create response: %w", err)
		}
		if createResp.ProcessID == 0 {
			log.Printf("[ProcessTracking CREATE] Got success status but processId was 0 or missing in response for TrackingID %s. Body: %s", payload.TrackingID, string(bodyBytes))
			return 0, fmt.Errorf("processId missing or zero in create response")
		}
		log.Printf("[ProcessTracking CREATE] Notification successful for TrackingID %s. Received ProcessID: %d", payload.TrackingID, createResp.ProcessID)
		return createResp.ProcessID, nil // Return the numeric ID
	}
}

// notifyProcessTrackingUpdate sends the final status update using the numeric processID
func notifyProcessTrackingUpdate(config *Config, numericProcessID int64, payload ProcessTrackingUpdatePayload) {
	if config.ProcessTrackingURL == "" || numericProcessID == 0 {
		log.Printf("[ProcessTracking UPDATE] Skipping notification for ProcessID %d: URL not set or ProcessID is zero.", numericProcessID)
		return
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[ProcessTracking UPDATE] Error marshaling payload for ProcessID %d: %v", numericProcessID, err)
		return
	}

	// Construct the specific update URL using the numeric ID
	processIDStr := strconv.FormatInt(numericProcessID, 10)
	updateURL := strings.TrimSuffix(config.ProcessTrackingURL, "/") + "/" + processIDStr

	req, err := http.NewRequest("POST", updateURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		log.Printf("[ProcessTracking UPDATE] Error creating request for ProcessID %d: %v", numericProcessID, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	log.Printf("[ProcessTracking UPDATE] Sending status '%s' for ProcessID %d", payload.Status, numericProcessID)
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("[ProcessTracking UPDATE] Error sending notification for ProcessID %d: %v", numericProcessID, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := ioutil.ReadAll(resp.Body)
		log.Printf("[ProcessTracking UPDATE] Notification failed for ProcessID %d: Status %d, Body: %s", numericProcessID, resp.StatusCode, string(bodyBytes))
	} else {
		log.Printf("[ProcessTracking UPDATE] Notification successful for ProcessID %d (Status: %s)", numericProcessID, payload.Status)
	}
}

// executeScript handles the /v1/execute endpoint, integrating Process Tracking.
func executeScript(c *gin.Context) {
	config := loadConfig()
	var request TaskServiceRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		log.Printf("Failed to bind JSON payload: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request payload format"})
		return
	}

	log.Printf("Received execute request via Task Service - TaskName(Instance): '%s', TrackingID: %s, TaskData: %v", request.TaskName, request.TrackingID, request.TaskData)

	// Extract actual script name
	scriptNameInterface, nameOk := request.TaskData["name"]
	if !nameOk {
		log.Printf("Execute request failed: taskData is missing the 'name' field. TrackingID: %s", request.TrackingID)
		c.JSON(http.StatusBadRequest, gin.H{"error": "taskData must contain a 'name' field specifying the script to run", "trackingId": request.TrackingID})
		return
	}
	actualScriptName, nameIsString := scriptNameInterface.(string)
	if !nameIsString || actualScriptName == "" {
		log.Printf("Execute request failed: taskData 'name' field is not a non-empty string ('%v'). TrackingID: %s", scriptNameInterface, request.TrackingID)
		c.JSON(http.StatusBadRequest, gin.H{"error": "taskData 'name' field must be a non-empty string", "trackingId": request.TrackingID})
		return
	}

	// --- Process Tracking Start ---
	// Create the initial process record SYNCHRONOUSLY to get the numeric ID
	numericProcessID, createErr := notifyProcessTrackingCreate(config, ProcessTrackingCreatePayload{
		Name:       actualScriptName,
		Stage:      config.ProcessTrackingStage,
		Group:      config.ProcessTrackingGroup,
		Label:      actualScriptName,
		TrackingID: request.TrackingID, // Pass Task Service TrackingID for correlation
	})

	if createErr != nil {
		// Log the creation error. Decide if we should stop execution.
		// For now, we log and continue, but updates will likely fail due to numericProcessID being 0.
		log.Printf("ERROR: Failed to create initial process tracking record for script '%s': %v. TrackingID: %s. Execution will continue, but status updates may fail.", actualScriptName, createErr, request.TrackingID)
		// Optionally: Fail fast
		// notifyProcessTrackingUpdate(config, 0, ProcessTrackingUpdatePayload{Status: "FAILED", Message: fmt.Sprintf("Failed to create tracking record: %v", createErr)}) // Won't work if ID is 0
		// c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to initialize process tracking", "trackingId": request.TrackingID})
		// return
	}

	// --- Resume normal execution flow ---
	log.Printf("Extracted actual script name '%s' from taskData. TrackingID: %s", actualScriptName, request.TrackingID)

	// Load script definitions
	definitions, err := loadScriptDefinitions(config.ScriptsPath)
	if err != nil {
		log.Printf("Error loading script definitions during execute: %v, TrackingID: %s", err, request.TrackingID)
		statusCode := http.StatusInternalServerError
		errMsg := fmt.Sprintf("Server configuration error: Failed to load or parse script definitions file: %v", err)
		if os.IsNotExist(err) {
			errMsg = fmt.Sprintf("Server configuration error: Script definitions file not found at %s", config.ScriptsPath)
		}
		// Send FAILED status UPDATE using numeric ID
		notifyProcessTrackingUpdate(config, numericProcessID, ProcessTrackingUpdatePayload{Status: "FAILED", Message: fmt.Sprintf("Failed to load script definitions: %v", err)})
		c.JSON(statusCode, gin.H{"error": errMsg, "trackingId": request.TrackingID, "processId": numericProcessID}) // Include ProcessID in response
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
		log.Printf("Execute request failed: Script with name '%s' (from taskData) not found in definitions. TrackingID: %s", actualScriptName, request.TrackingID)
		// Send FAILED status UPDATE using numeric ID
		notifyProcessTrackingUpdate(config, numericProcessID, ProcessTrackingUpdatePayload{Status: "FAILED", Message: fmt.Sprintf("Script '%s' not found", actualScriptName)})
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Script '%s' not found", actualScriptName), "trackingId": request.TrackingID, "processId": numericProcessID})
		return
	}

	log.Printf("Found definition for script '%s' (ID: %s). TrackingID: %s", selectedDefinition.Name, selectedDefinition.ID, request.TrackingID)

	// Get the target pod
	targetPod, err := getTargetPod(config.Namespace, config.PodLabelSelector)
	if err != nil {
		log.Printf("Execute request failed for script '%s': Could not get target pod: %v. TrackingID: %s", selectedDefinition.Name, err, request.TrackingID)
		// Send FAILED status UPDATE using numeric ID
		notifyProcessTrackingUpdate(config, numericProcessID, ProcessTrackingUpdatePayload{Status: "FAILED", Message: fmt.Sprintf("Failed to find target pod: %v", err)})
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to find target pod: %v", err), "trackingId": request.TrackingID, "processId": numericProcessID})
		return
	}

	log.Printf("Target pod for script '%s' execution: %s (Namespace: %s, Selector: %s). TrackingID: %s", selectedDefinition.Name, targetPod, config.Namespace, config.PodLabelSelector, request.TrackingID)

	// Prepare environment variables by extracting values from taskData based on script's AcceptedParameters
	envPrefix := ""
	if len(selectedDefinition.AcceptedParameters) > 0 {
		var envVars []string
		log.Printf("Processing %d accepted parameters for script '%s'. TrackingID: %s", len(selectedDefinition.AcceptedParameters), selectedDefinition.Name, request.TrackingID)

		for _, paramDef := range selectedDefinition.AcceptedParameters {
			paramValueInterface, valueOk := request.TaskData[paramDef.Name] // Look for key matching paramDef.Name in taskData

			if !valueOk {
				// Handle missing parameter value - check if it was optional in definition
				if !paramDef.Optional {
					log.Printf("Execute request failed for script '%s': Required parameter '%s' missing in taskData. TrackingID: %s", selectedDefinition.Name, paramDef.Name, request.TrackingID)
					// Send FAILED status UPDATE using numeric ID
					notifyProcessTrackingUpdate(config, numericProcessID, ProcessTrackingUpdatePayload{Status: "FAILED", Message: fmt.Sprintf("Required parameter '%s' missing", paramDef.Name)})
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Required parameter '%s' is missing in taskData", paramDef.Name), "trackingId": request.TrackingID, "processId": numericProcessID})
					return
				} else {
					// Optional parameter is missing, skip setting env var for it
					log.Printf("Optional parameter '%s' for script '%s' missing in taskData, skipping. TrackingID: %s", paramDef.Name, selectedDefinition.Name, request.TrackingID)
					continue
				}
			}

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
	if err != nil {
		log.Printf("Execution FAILED for script '%s' (ID: %s) in pod '%s'. TrackingID: %s. Error: %v. Output: %s", selectedDefinition.Name, selectedDefinition.ID, targetPod, request.TrackingID, err, string(output))
		// Send FAILED status UPDATE using numeric ID
		notifyProcessTrackingUpdate(config, numericProcessID, ProcessTrackingUpdatePayload{Status: "FAILED", Message: fmt.Sprintf("Execution error: %v", err)})
		c.JSON(http.StatusInternalServerError, gin.H{
			"taskName":   actualScriptName,
			"script_id":  selectedDefinition.ID,
			"trackingId": request.TrackingID,
			"processId":  numericProcessID, // Include ProcessID in response
			"error":      fmt.Sprintf("Script execution failed: %v", err),
			"output":     string(output),
		})
		return
	}

	// --- Execution Successful ---
	log.Printf("Execution SUCCESSFUL for script '%s' (ID: %s) in pod '%s'. TrackingID: %s. Output: %s", selectedDefinition.Name, selectedDefinition.ID, targetPod, request.TrackingID, string(output))
	// Send COMPLETED status UPDATE using numeric ID
	notifyProcessTrackingUpdate(config, numericProcessID, ProcessTrackingUpdatePayload{Status: "COMPLETED"})
	c.JSON(http.StatusOK, gin.H{
		"taskName":   actualScriptName,
		"script_id":  selectedDefinition.ID,
		"trackingId": request.TrackingID,
		"processId":  numericProcessID, // Include ProcessID in response
		"output":     string(output),
	})
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
