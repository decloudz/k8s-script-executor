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

// Load scripts from JSON file
func loadScripts(filePath string) ([]Script, error) {
	file, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var scripts []Script
	err = json.Unmarshal(file, &scripts)
	if err != nil {
		return nil, err
	}

	return scripts, nil
}

// Get the first pod running query-server
func getTargetPod(namespace string) (string, error) {
	cmd := exec.Command("sh", "-c", fmt.Sprintf("kubectl get pods -n %s -l app=query-server -o jsonpath='{.items[0].metadata.name}'", namespace))
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	podName := strings.TrimSpace(string(out))
	if podName == "" {
		return "", fmt.Errorf("no target pod found")
	}
	return podName, nil
}

// List scripts from scripts.json
func listScripts(c *gin.Context) {
	scripts, err := loadScripts("/scripts/scripts.json")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load scripts"})
		return
	}
	c.JSON(http.StatusOK, scripts)
}

// Execute a script inside the target pod
func executeScript(c *gin.Context) {
	var request struct {
		ScriptName string `json:"script_name"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request payload"})
		return
	}

	// Load scripts
	scripts, err := loadScripts("/scripts/scripts.json")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load scripts"})
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
	namespace := os.Getenv("NAMESPACE")
	targetPod, err := getTargetPod(namespace)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Execute the script in the target pod
	execCmd := fmt.Sprintf("kubectl exec -n %s %s -- /bin/bash -c '%s'", namespace, targetPod, selectedScript.Command)
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
	r := gin.Default()

	// Define API routes
	r.GET("/scripts", listScripts)
	r.POST("/execute", executeScript)

	// Start server on port 8080
	port := "8080"
	log.Printf("Starting server on port %s...", port)
	r.Run("0.0.0.0:" + port)
}
