package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/gin-gonic/gin"
)

// ParameterOption defines the structure for options within a script definition
type ParameterOption struct {
	Name string `json:"name"` // Required
	ID   string `json:"id"`   // Required
}

// ScriptDefinition holds the combined parameter options and command execution details
type ScriptDefinition struct {
	// Fields for /v1/options endpoint
	ID          string            `json:"id"`   // Required
	Name        string            `json:"name"` // Required
	Description string            `json:"description,omitempty"`
	Label       string            `json:"label,omitempty"`
	Type        string            `json:"type,omitempty"` // Defaults to "string"
	Optional    bool              `json:"optional,omitempty"`
	Options     []ParameterOption `json:"options,omitempty"`

	// Field for /v1/execute endpoint
	Command string `json:"command"` // Required
}

// ParameterView is the structure returned by the /v1/options endpoint
type ParameterView struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Label       string            `json:"label,omitempty"`
	Type        string            `json:"type,omitempty"`
	Optional    bool              `json:"optional,omitempty"`
	Options     []ParameterOption `json:"options,omitempty"`
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

	// Validate definitions and set defaults
	for i := range definitions {
		if definitions[i].ID == "" {
			return nil, fmt.Errorf("script definition %d in '%s' is missing required 'id' field", i, filePath)
		}
		if definitions[i].Name == "" {
			return nil, fmt.Errorf("script definition %d (id: %s) in '%s' is missing required 'name' field", i, definitions[i].ID, filePath)
		}
		if definitions[i].Command == "" {
			return nil, fmt.Errorf("script definition %d (id: %s) in '%s' is missing required 'command' field", i, definitions[i].ID, filePath)
		}
		if definitions[i].Type == "" {
			definitions[i].Type = "string" // Set default type
		}

		// Validate options within definitions
		for j, option := range definitions[i].Options {
			if option.ID == "" {
				return nil, fmt.Errorf("option %d for script definition '%s' in '%s' is missing required 'id' field", j, definitions[i].ID, filePath)
			}
			if option.Name == "" {
				return nil, fmt.Errorf("option %d (id: %s) for script definition '%s' in '%s' is missing required 'name' field", j, option.ID, definitions[i].ID, filePath)
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
// It loads the script definitions, formats them as ParameterView, and returns raw JSON.
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
		c.String(statusCode, "[]") // Send raw string "[]"
		return
	}

	// Create the view model, excluding the 'command' field
	parameterViews := make([]ParameterView, len(definitions))
	for i, def := range definitions {
		parameterViews[i] = ParameterView{
			ID:          def.ID,
			Name:        def.Name,
			Description: def.Description,
			Label:       def.Label,
			Type:        def.Type,
			Optional:    def.Optional,
			Options:     def.Options,
		}
	}

	// Manually marshal the data to JSON bytes
	jsonData, err := json.Marshal(parameterViews)
	if err != nil {
		// This error should be rare if the structs are well-defined
		log.Printf("Error marshaling parameter views to JSON: %v", err)
		// Return status code and an empty JSON array body "[]"
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.String(http.StatusInternalServerError, "[]")
		return
	}

	// Set Content-Type and write raw JSON bytes using c.Data
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Data(http.StatusOK, "application/json", jsonData)
}

// executeScript handles the /v1/execute endpoint.
// It finds the requested script by name, prepares environment variables from parameters,
// and executes its command.
func executeScript(c *gin.Context) {
	config := loadConfig()

	var request struct {
		ScriptName string            `json:"script_name"`          // Client specifies script by name
		Parameters map[string]string `json:"parameters,omitempty"` // Optional key-value parameters
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request payload"})
		return
	}

	// Load script definitions
	definitions, err := loadScriptDefinitions(config.ScriptsPath)
	if err != nil {
		log.Printf("Error loading script definitions during execute: %v", err)
		statusCode := http.StatusInternalServerError
		errMsg := fmt.Sprintf("Server configuration error: Failed to load or parse script definitions file: %v", err)
		if os.IsNotExist(err) {
			errMsg = fmt.Sprintf("Server configuration error: Script definitions file not found at %s", config.ScriptsPath)
		}
		c.JSON(statusCode, gin.H{"error": errMsg})
		return
	}

	// Find the requested script definition by name
	var selectedDefinition *ScriptDefinition
	for i := range definitions {
		if definitions[i].Name == request.ScriptName {
			selectedDefinition = &definitions[i]
			break
		}
	}

	if selectedDefinition == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Script '%s' not found", request.ScriptName)})
		return
	}

	// Get the target pod
	targetPod, err := getTargetPod(config.Namespace, config.PodLabelSelector)
	if err != nil {
		// Error already logged in getTargetPod if needed, return specific error to client
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to find target pod: %v", err)})
		return
	}

	// Prepare environment variables from parameters
	envPrefix := ""
	if len(request.Parameters) > 0 {
		var envVars []string
		for key, value := range request.Parameters {
			// Basic validation for environment variable names (adjust regex as needed)
			// This prevents injecting arbitrary commands via the key
			if !isValidEnvVarName(key) {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid parameter name (must be alphanumeric + underscore): %s", key)})
				return
			}
			// Quote the value to handle spaces and special characters safely for shell
			quotedValue := fmt.Sprintf("%q", value)
			envVars = append(envVars, fmt.Sprintf("%s=%s", key, quotedValue))
		}
		envPrefix = strings.Join(envVars, " ") + " " // Add trailing space
	}

	// Construct the command with environment variable prefix
	// IMPORTANT: Assumes the pod's default shell (/bin/bash here) interprets the env var setting prefix correctly.
	fullCommand := envPrefix + selectedDefinition.Command

	// Execute the script's command in the target pod
	// Use single quotes around the full command to prevent local shell expansion
	execCmd := fmt.Sprintf("kubectl exec -n %s %s -- /bin/bash -c '%s'",
		config.Namespace,
		targetPod,
		fullCommand, // Pass the command with potential env var prefix
	)
	cmd := exec.Command("sh", "-c", execCmd)

	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Error executing script '%s' (id: %s) in pod '%s': %v, output: %s", selectedDefinition.Name, selectedDefinition.ID, targetPod, err, string(output))
		c.JSON(http.StatusInternalServerError, gin.H{
			"script_name": selectedDefinition.Name, // Return name for consistency with request
			"script_id":   selectedDefinition.ID,
			"error":       fmt.Sprintf("Script execution failed: %v", err),
			"output":      string(output),
		})
		return
	}

	log.Printf("Successfully executed script '%s' (id: %s) in pod '%s'", selectedDefinition.Name, selectedDefinition.ID, targetPod)
	c.JSON(http.StatusOK, gin.H{
		"script_name": selectedDefinition.Name,
		"script_id":   selectedDefinition.ID,
		"output":      string(output),
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

func main() {
	config := loadConfig()
	log.Printf("Starting server with configuration:")
	log.Printf("- Scripts Definition Path: %s", config.ScriptsPath)
	log.Printf("- Pod Label Selector: %s", config.PodLabelSelector)
	log.Printf("- Namespace: %s", config.Namespace)

	r := gin.Default()

	// Define API routes
	r.GET("/v1/options", listScripts) // Serves parameter view
	r.POST("/v1/execute", executeScript)

	// Start server on port 8080
	port := "8080"
	log.Printf("Starting server on port %s...", port)
	if err := r.Run("0.0.0.0:" + port); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
