@Library('aes@4.x') _
@Library('devops-sre@master')

def config = [
    details: [
        applicationName: "pcs-script-executor",
        serviceName: "pcs-script-executor"
    ],
    deploy: [
        nonDefaultBranch: [
            enabled: false
        ]
    ],
    semver: [
        gitVersionTool: "5.3.7"
    ],
    container: [
        registry: "ghcr.io",
        imageName: "alvdevcl/go-script-executor"
    ],
    helm: [
        installVersion: "v3.14.1",
        installPath: "/usr/local/bin/helm",
        chartPath: "deploy/chart",
        repoRegistry: "ghcr.io",
        repoName: "alvdevcl/helm-charts"
    ],
    git_cred: "git-credentials",
    secrets_cred_id: "pipeline-secret",
    secrets_user_id: "pipeline-secret",
    verify: true
]

def git_commit_details = null
def version = ""
def semVer = ""
def chartVersion = ""
def imageTags = []

node("centos-imagefactory") {

    stage("Git Checkout") {
        checkout scm
        git_commit_details = gitUtils.getGitCommitDetails()
        
        // Generate the version from git tags
        semVer = sh(script: "git describe --tags --abbrev=0 || echo v0.1.0", returnStdout: true).trim()
        version = semVer.startsWith('v') ? semVer.substring(1) : semVer
        echo "Building version: ${version}"
    }

    stage("Install Tools") {
        echo "Installing Helm Version: ${config.helm.installVersion}"
        sh "curl -fsSL -o get_helm.sh https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3"
        sh "chmod +x get_helm.sh"
        sh "./get_helm.sh --version ${config.helm.installVersion}"
        sh "${config.helm.installPath} version"
        
        echo "Installing Go"
        def goVersion = "1.21.6" // specify your Go version
        sh """
            curl -fsSL https://golang.org/dl/go\${goVersion}.linux-amd64.tar.gz -o go.tar.gz
            rm -rf /usr/local/go && tar -C /usr/local -xzf go.tar.gz
            export PATH=\$PATH:/usr/local/go/bin
            go version
        """
        
        echo "Installing Podman"
        sh """
            # For RHEL/CentOS
            yum -y install podman
            # If on Ubuntu/Debian, use:
            # apt-get update && apt-get install -y podman
            podman --version
        """
    }

    stage("Build Container Image") {
        // Add Go to path for this stage as well
        sh "export PATH=\$PATH:/usr/local/go/bin"
        
        // Log in to Container registry
        withCredentials([usernamePassword(credentialsId: "${config.git_cred}", 
                                          usernameVariable: 'REGISTRY_USER', 
                                          passwordVariable: 'REGISTRY_PASS')]) {
            sh "echo ${REGISTRY_PASS} | podman login ${config.container.registry} -u ${REGISTRY_USER} --password-stdin"
        }

        // Build tags based on versioning strategy
        imageTags = [
            "${config.container.registry}/${config.container.imageName}:latest",
            "${config.container.registry}/${config.container.imageName}:${version}",
            "${config.container.registry}/${config.container.imageName}:v${version}"
        ]
        
        def tagsString = imageTags.collect { "-t ${it}" }.join(' ')
        
        // Build and push Container image using Podman
        sh "podman build ${tagsString} ."
        
        imageTags.each { tag ->
            sh "podman push ${tag}"
        }
        
        echo "Container image built and pushed successfully: ${imageTags.join(', ')}"
    }

    stage("Package and Push Helm Chart") {
        // Extract current chart version
        def chartYamlPath = "${config.helm.chartPath}/Chart.yaml"
        chartVersion = sh(script: "grep -E '^version:' ${chartYamlPath} | awk '{print \$2}'", returnStdout: true).trim()
        echo "Current Helm chart version: ${chartVersion}"
        
        // Update appVersion in Chart.yaml to match the current build version
        sh "sed -i 's/^appVersion:.*/appVersion: \"${version}\"/' ${chartYamlPath}"
        
        // Create a new version by incrementing the patch number
        def versionParts = chartVersion.tokenize('.')
        def newPatchVersion = versionParts[2].toInteger() + 1
        def newChartVersion = "${versionParts[0]}.${versionParts[1]}.${newPatchVersion}"
        
        // Update chart version
        sh "sed -i 's/^version:.*/version: ${newChartVersion}/' ${chartYamlPath}"
        echo "Updated Helm chart version to: ${newChartVersion}"
        
        // Update the image.tag in values.yaml to point to the new Container image
        sh "sed -i 's|tag: .*|tag: \"${version}\"|' ${config.helm.chartPath}/values.yaml"
        
        // Package the Helm chart
        sh "mkdir -p .deploy"
        sh "${config.helm.installPath} package ${config.helm.chartPath} -d .deploy"
        
        // Login to Helm Registry
        withCredentials([usernamePassword(credentialsId: "${config.git_cred}", 
                                          usernameVariable: 'HELM_USER', 
                                          passwordVariable: 'HELM_PASS')]) {
            sh "${config.helm.installPath} registry login -u ${HELM_USER} -p ${HELM_PASS} ${config.helm.repoRegistry}"
        }
        
        // Push Helm chart to registry
        def chartPackage = "k8s-script-executor-${newChartVersion}.tgz"
        sh "${config.helm.installPath} push .deploy/${chartPackage} oci://${config.helm.repoRegistry}/${config.helm.repoName}"
        
        echo "Helm chart packaged and pushed successfully: ${chartPackage}"
        
        // Update Git with the new chart version
        sh """
            git config --global user.email "jenkins@example.com"
            git config --global user.name "Jenkins CI"
            git add ${chartYamlPath} ${config.helm.chartPath}/values.yaml
            git commit -m "Updating Helm chart version to ${newChartVersion} and Container image tag to ${version}"
            git push origin HEAD
        """
    }
}
