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

// Script represents a script entry in scripts.json
type Script struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

// Config holds application configuration
type Config struct {
	ScriptsPath      string
	PodLabelSelector string
	Namespace        string
}

// Load configuration from environment variables with fallbacks
func loadConfig() *Config {
	return &Config{
		ScriptsPath:      getEnvOrDefault("SCRIPTS_PATH", "/scripts/scripts.json"),
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

// Load scripts from JSON file
func loadScripts(filePath string) ([]Script, error) {
	file, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read scripts file: %v", err)
	}

	var scripts []Script
	err = json.Unmarshal(file, &scripts)
	if err != nil {
		return nil, fmt.Errorf("failed to parse scripts JSON: %v", err)
	}

	return scripts, nil
}

// Get the first pod matching the label selector
func getTargetPod(namespace, labelSelector string) (string, error) {
	cmd := exec.Command("sh", "-c", fmt.Sprintf("kubectl get pods -n %s -l %s -o jsonpath='{.items[0].metadata.name}'", namespace, labelSelector))
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get pod: %v", err)
	}
	podName := strings.TrimSpace(string(out))
	if podName == "" {
		return "", fmt.Errorf("no pod found matching label selector: %s", labelSelector)
	}
	return podName, nil
}

// List scripts from scripts.json
func listScripts(c *gin.Context) {
	config := loadConfig()
	scripts, err := loadScripts(config.ScriptsPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, scripts)
}

// Execute a script inside the target pod
func executeScript(c *gin.Context) {
	config := loadConfig()

	var request struct {
		ScriptName string `json:"script_name"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request payload"})
		return
	}

	// Load scripts
	scripts, err := loadScripts(config.ScriptsPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Find the requested script
	var selectedScript *Script
	for _, script := range scripts {
		if script.Name == request.ScriptName {
			selectedScript = &script
			break
		}
	}

	if selectedScript == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Script '%s' not found", request.ScriptName)})
		return
	}

	// Get the target pod
	targetPod, err := getTargetPod(config.Namespace, config.PodLabelSelector)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Execute the script in the target pod
	execCmd := fmt.Sprintf("kubectl exec -n %s %s -- /bin/bash -c '%s'", config.Namespace, targetPod, selectedScript.Command)
	cmd := exec.Command("sh", "-c", execCmd)

	output, err := cmd.CombinedOutput()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"script_name": request.ScriptName,
			"error":       err.Error(),
			"output":      string(output),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"script_name": request.ScriptName,
		"output":      string(output),
	})
}

func main() {
	config := loadConfig()
	log.Printf("Starting server with configuration:")
	log.Printf("- Scripts path: %s", config.ScriptsPath)
	log.Printf("- Pod label selector: %s", config.PodLabelSelector)
	log.Printf("- Namespace: %s", config.Namespace)

	r := gin.Default()

	// Define API routes
	r.GET("/v1/scripts", listScripts)
	r.POST("/v1/execute", executeScript)

	// Start server on port 8080
	port := "8080"
	log.Printf("Starting server on port %s...", port)
	r.Run("0.0.0.0:" + port)
}
