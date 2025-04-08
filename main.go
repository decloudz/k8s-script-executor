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
// It loads the script definitions and returns only the parameter-related fields.
func listScripts(c *gin.Context) {
	config := loadConfig()

	definitions, err := loadScriptDefinitions(config.ScriptsPath)
	if err != nil {
		log.Printf("Error loading script definitions: %v", err)
		statusCode := http.StatusInternalServerError
		errMsg := fmt.Sprintf("Server configuration error: Failed to load or parse script definitions file: %v", err)
		if os.IsNotExist(err) {
			errMsg = fmt.Sprintf("Server configuration error: Script definitions file not found at %s", config.ScriptsPath)
		}
		c.JSON(statusCode, gin.H{"error": errMsg})
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

	c.JSON(http.StatusOK, parameterViews)
}

// executeScript handles the /v1/execute endpoint.
// It finds the requested script by name and executes its command.
func executeScript(c *gin.Context) {
	config := loadConfig()

	var request struct {
		ScriptName string `json:"script_name"` // Client specifies script by name
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

	// Execute the script's command in the target pod
	execCmd := fmt.Sprintf("kubectl exec -n %s %s -- /bin/bash -c '%s'", config.Namespace, targetPod, selectedDefinition.Command)
	cmd := exec.Command("sh", "-c", execCmd)

	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Error executing script '%s' (command: '%s') in pod '%s': %v, output: %s", selectedDefinition.Name, selectedDefinition.Command, targetPod, err, string(output))
		c.JSON(http.StatusInternalServerError, gin.H{
			"script_name": request.ScriptName,
			"error":       fmt.Sprintf("Script execution failed: %v", err), // Provide a slightly cleaner error
			"output":      string(output),
		})
		return
	}

	log.Printf("Successfully executed script '%s' in pod '%s'", selectedDefinition.Name, targetPod)
	c.JSON(http.StatusOK, gin.H{
		"script_name": request.ScriptName,
		"output":      string(output),
	})
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
