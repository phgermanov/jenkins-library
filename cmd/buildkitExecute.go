package cmd

import (
	"fmt"
	"strings"

	"github.com/pkg/errors"

	"github.com/SAP/jenkins-library/pkg/command"
	"github.com/SAP/jenkins-library/pkg/docker"
	"github.com/SAP/jenkins-library/pkg/log"
	"github.com/SAP/jenkins-library/pkg/piperutils"
	"github.com/SAP/jenkins-library/pkg/telemetry"
)

func buildkitExecute(config buildkitExecuteOptions, telemetryData *telemetry.CustomData, commonPipelineEnvironment *buildkitExecuteCommonPipelineEnvironment) {
	// for command execution use Command
	c := command.Command{
		ErrorCategoryMapping: map[string][]string{
			log.ErrorConfiguration.String(): {
				"unsupported status code 401",
			},
		},
		StepName: "buildkitExecute",
	}

	// reroute command output to logging framework
	c.Stdout(log.Writer())
	c.Stderr(log.Writer())

	fileUtils := &piperutils.Files{}

	err := runBuildkitExecute(&config, telemetryData, commonPipelineEnvironment, &c, fileUtils)
	if err != nil {
		log.Entry().WithError(err).Fatal("BuildKit execution failed")
	}
}

func runBuildkitExecute(config *buildkitExecuteOptions, telemetryData *telemetry.CustomData, commonPipelineEnvironment *buildkitExecuteCommonPipelineEnvironment, execRunner command.ExecRunner, fileUtils piperutils.FileUtils) error {
	// Setup Docker config.json for authentication
	dockerConfig := []byte(`{"auths":{}}`)

	// respect user provided docker config json file
	if len(config.DockerConfigJSON) > 0 {
		var err error
		dockerConfig, err = fileUtils.FileRead(config.DockerConfigJSON)
		if err != nil {
			return errors.Wrapf(err, "failed to read existing docker config json at '%v'", config.DockerConfigJSON)
		}
	}

	// if user provided credentials, create/update docker config json
	if len(config.ContainerRegistryURL) > 0 && len(config.ContainerRegistryPassword) > 0 && len(config.ContainerRegistryUser) > 0 {
		targetConfigJson := "/root/.docker/config.json"
		if len(config.DockerConfigJSON) > 0 {
			targetConfigJson = config.DockerConfigJSON
		}

		targetConfigJson, err := docker.CreateDockerConfigJSON(config.ContainerRegistryURL, config.ContainerRegistryUser, config.ContainerRegistryPassword, "", targetConfigJson, fileUtils)
		if err != nil {
			return errors.Wrap(err, "failed to create docker config json")
		}

		dockerConfig, err = fileUtils.FileRead(targetConfigJson)
		if err != nil {
			return errors.Wrapf(err, "failed to read docker config file at %v", targetConfigJson)
		}
	}

	// Write docker config to standard location
	if err := fileUtils.FileWrite("/root/.docker/config.json", dockerConfig, 0644); err != nil {
		return errors.Wrap(err, "failed to write file '/root/.docker/config.json'")
	}

	// Determine the image destination
	var destination string
	switch {
	case config.ContainerRegistryURL != "" && config.ContainerImageName != "" && config.ContainerImageTag != "":
		log.Entry().Debugf("Single image build for image name '%v'", config.ContainerImageName)

		containerRegistry, err := docker.ContainerRegistryFromURL(config.ContainerRegistryURL)
		if err != nil {
			log.SetErrorCategory(log.ErrorConfiguration)
			return errors.Wrapf(err, "failed to read registry url %v", config.ContainerRegistryURL)
		}

		// Docker image tags don't allow plus signs in tags, thus replacing with dash
		containerImageTag := strings.ReplaceAll(config.ContainerImageTag, "+", "-")
		containerImageNameAndTag := fmt.Sprintf("%v:%v", config.ContainerImageName, containerImageTag)

		commonPipelineEnvironment.container.registryURL = config.ContainerRegistryURL
		commonPipelineEnvironment.container.imageNameTag = containerImageNameAndTag
		destination = fmt.Sprintf("%v/%v", containerRegistry, containerImageNameAndTag)

	case config.ContainerImage != "":
		log.Entry().Debugf("Single image build for image '%v'", config.ContainerImage)

		containerRegistry, err := docker.ContainerRegistryFromImage(config.ContainerImage)
		if err != nil {
			log.SetErrorCategory(log.ErrorConfiguration)
			return errors.Wrapf(err, "invalid registry part in image %v", config.ContainerImage)
		}

		// errors are already caught with previous call to docker.ContainerRegistryFromImage
		containerImageNameTag, _ := docker.ContainerImageNameTagFromImage(config.ContainerImage)

		commonPipelineEnvironment.container.registryURL = fmt.Sprintf("https://%v", containerRegistry)
		commonPipelineEnvironment.container.imageNameTag = containerImageNameTag
		destination = config.ContainerImage
	default:
		return fmt.Errorf("either containerImage or (containerRegistryUrl, containerImageName, containerImageTag) must be provided")
	}

	if err := runBuildkit(config.DockerfilePath, destination, config.BuildOptions, execRunner, fileUtils, commonPipelineEnvironment); err != nil {
		return err
	}

	return nil
}

func runBuildkit(dockerFilepath string, destination string, buildOptions []string, execRunner command.ExecRunner, fileUtils piperutils.FileUtils, commonPipelineEnvironment *buildkitExecuteCommonPipelineEnvironment) error {
	cwd, err := fileUtils.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current working directory: %w", err)
	}

	log.Entry().Infof("Building image: %s", destination)

	// Start buildkitd in background
	log.Entry().Info("Starting BuildKit daemon...")
	if err := execRunner.RunExecutable("buildkitd", "--addr", "unix:///run/buildkit/buildkitd.sock", "--oci-worker-no-process-sandbox"); err != nil {
		// Try to start in background - this might fail if already running, which is OK
		log.Entry().Debug("BuildKit daemon start returned an error (may already be running)")
	}

	// Build the image using buildctl
	buildctlArgs := []string{
		"--addr", "unix:///run/buildkit/buildkitd.sock",
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + cwd,
		"--local", "dockerfile=" + cwd,
		"--opt", "filename=" + dockerFilepath,
		"--output", "type=image,name=" + destination + ",push=true",
	}

	// Add any additional build options
	buildctlArgs = append(buildctlArgs, buildOptions...)

	if GeneralConfig.Verbose {
		buildctlArgs = append(buildctlArgs, "--progress=plain")
	}

	log.Entry().Debugf("Running buildctl with args: %v", buildctlArgs)

	err = execRunner.RunExecutable("buildctl", buildctlArgs...)
	if err != nil {
		log.SetErrorCategory(log.ErrorBuild)
		return errors.Wrap(err, "execution of 'buildctl' failed")
	}

	log.Entry().Infof("Successfully built and pushed image: %s", destination)

	return nil
}
