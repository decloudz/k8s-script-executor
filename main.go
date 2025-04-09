package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"

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

// Config holds application configuration
type Config struct {
	ScriptsPath      string // Path to the combined script definitions file
	PodLabelSelector string
	Namespace        string
}

// Load configuration from environment variables with fallbacks
func loadConfig() *Config {
	return &Config{
		ScriptsPath:      getEnvOrDefault("SCRIPTS_PATH", "/config/scripts.json"), // Default matches deploy.yaml
		PodLabelSelector: getEnvOrDefault("POD_LABEL_SELECTOR", "app=query-server"),
		Namespace:        getEnvOrDefault("NAMESPACE", "default"),
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

// executeScript handles the /v1/execute endpoint, adapting to TaskService payload.
func executeScript(c *gin.Context) {
	config := loadConfig()

	var request TaskServiceRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		log.Printf("Failed to bind JSON payload: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request payload format"})
		return
	}

	// ---> Log: Received execution request
	log.Printf("Received execute request via Task Service - TaskName(Instance): '%s', TrackingID: %s, TaskData: %v", request.TaskName, request.TrackingID, request.TaskData)

	// --- Extract Actual Script Name from TaskData ---
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

	// ---> Log: Extracted script name
	log.Printf("Extracted actual script name '%s' from taskData. TrackingID: %s", actualScriptName, request.TrackingID)

	// Add validation for TrackingID if needed

	// Load script definitions
	definitions, err := loadScriptDefinitions(config.ScriptsPath)
	if err != nil {
		log.Printf("Error loading script definitions during execute: %v, TrackingID: %s", err, request.TrackingID)
		statusCode := http.StatusInternalServerError
		errMsg := fmt.Sprintf("Server configuration error: Failed to load or parse script definitions file: %v", err)
		if os.IsNotExist(err) {
			errMsg = fmt.Sprintf("Server configuration error: Script definitions file not found at %s", config.ScriptsPath)
		}
		c.JSON(statusCode, gin.H{"error": errMsg, "trackingId": request.TrackingID})
		return
	}

	// Find the requested script definition by the *actual* script name from taskData
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
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Script '%s' not found", actualScriptName), "trackingId": request.TrackingID})
		return
	}

	// ---> Log: Found script definition
	log.Printf("Found definition for script '%s' (ID: %s). TrackingID: %s", selectedDefinition.Name, selectedDefinition.ID, request.TrackingID)

	// Get the target pod
	targetPod, err := getTargetPod(config.Namespace, config.PodLabelSelector)
	if err != nil {
		log.Printf("Execute request failed for script '%s': Could not get target pod: %v. TrackingID: %s", selectedDefinition.Name, err, request.TrackingID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to find target pod: %v", err), "trackingId": request.TrackingID})
		return
	}

	// ---> Log: Target pod identified
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
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Required parameter '%s' is missing in taskData", paramDef.Name), "trackingId": request.TrackingID})
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

	cmd := exec.Command("sh", "-c", execCmd)
	log.Printf("Executing command for script '%s' in pod '%s'... TrackingID: %s", selectedDefinition.Name, targetPod, request.TrackingID)

	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Execution FAILED for script '%s' (ID: %s) in pod '%s'. TrackingID: %s. Error: %v. Output: %s", selectedDefinition.Name, selectedDefinition.ID, targetPod, request.TrackingID, err, string(output))
		c.JSON(http.StatusInternalServerError, gin.H{
			"taskName":   actualScriptName, // Return the actual script name
			"script_id":  selectedDefinition.ID,
			"trackingId": request.TrackingID,
			"error":      fmt.Sprintf("Script execution failed: %v", err),
			"output":     string(output),
		})
		return
	}

	log.Printf("Execution SUCCESSFUL for script '%s' (ID: %s) in pod '%s'. TrackingID: %s. Output: %s", selectedDefinition.Name, selectedDefinition.ID, targetPod, request.TrackingID, string(output))
	c.JSON(http.StatusOK, gin.H{
		"taskName":   actualScriptName, // Return the actual script name
		"script_id":  selectedDefinition.ID,
		"trackingId": request.TrackingID,
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
