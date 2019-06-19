package mod

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudfoundry/libcfbuildpack/helper"
	"gopkg.in/yaml.v2"

	"github.com/cloudfoundry/libcfbuildpack/build"
	"github.com/cloudfoundry/libcfbuildpack/layers"
)

const (
	Dependency = "go-mod"
	Launch     = "app-binary"
)

type Runner interface {
	Run(bin, dir string, quiet bool, args ...string) error
	RunWithOutput(bin, dir string, quiet bool, args ...string) (string, error)
	SetEnv(variableName string, path string) error
}

type Logger interface {
	Info(format string, args ...interface{})
}

type PkgManager interface {
	Install(location, cacheDir string) error
}

type MetadataInterface interface {
	Identity() (name string, version string)
}

type Metadata struct {
	Name string
	Hash string
}

func (m Metadata) Identity() (name string, version string) {
	return m.Name, m.Hash
}

type Contributor struct {
	goModMetadata MetadataInterface
	goBinMetadata MetadataInterface
	goModLayer    layers.Layer
	launchLayer   layers.Layer
	runner        Runner
	appRoot       string
	logger        Logger
	launch        layers.Layers
	appName       string
	targets       []string
}

func NewContributor(context build.Build, runner Runner) Contributor {
	return Contributor{
		goModLayer:    context.Layers.Layer(Dependency),
		launchLayer:   context.Layers.Layer(Launch),
		goModMetadata: nil,
		goBinMetadata: nil,
		runner:        runner,
		appRoot:       context.Application.Root,
		logger:        context.Logger,
		launch:        context.Layers,
	}
}

func (c Contributor) Contribute() error {
	targets, err := c.determineTargets()
	if err != nil {
		return err
	}
	c.targets = targets

	if err := c.goModLayer.Contribute(c.goModMetadata, c.ContributeGoModules, []layers.Flag{layers.Cache}...); err != nil {
		return err
	}

	if err := c.setAppName(); err != nil {
		return err
	}

	if err := c.launchLayer.Contribute(c.goBinMetadata, c.ContributeBinLayer, []layers.Flag{layers.Launch}...); err != nil {
		return err
	}

	return c.setStartCommand()
}

func (c Contributor) ContributeGoModules(_ layers.Layer) error {
	c.logger.Info("Setting environment variables")
	if err := c.runner.SetEnv("GOPATH", c.goModLayer.Root); err != nil {
		return err
	}

	args := []string{"install", "-buildmode", "pie", "-tags", "cloudfoundry"}

	if exists, err := helper.FileExists(filepath.Join(c.appRoot, "vendor")); err != nil {
		return err
	} else if exists {
		args = append(args, "-mod=vendor")
	}

	for _, target := range c.targets {
		args = append(args, target)
	}

	c.logger.Info("Running `go install`")
	if err := c.runner.Run("go", c.appRoot, false, args...); err != nil {
		return err
	}

	return nil
}

func (c Contributor) ContributeBinLayer(binLayer layers.Layer) error {
	c.logger.Info("Contributing app binary layer")

	oldBinPath := filepath.Join(c.goModLayer.Root, "bin", c.appName)
	newBinPath := filepath.Join(c.launchLayer.Root, c.appName)

	if err := os.MkdirAll(c.launchLayer.Root, os.ModePerm); err != nil {
		return err
	}

	return os.Rename(oldBinPath, newBinPath)
}

func (c Contributor) Cleanup() error {
	contents, err := filepath.Glob(filepath.Join(c.appRoot, "*"))
	if err != nil {
		return err
	}

	for _, file := range contents {
		if err := os.RemoveAll(file); err != nil {
			return err
		}
	}

	return nil
}

type Module struct {
	Path string `json:"Path"`
}

func (c *Contributor) setAppName() error {
	if len(c.targets) != 0 {
		targetSegments := strings.Split(c.targets[0], "/")
		appName := targetSegments[len(targetSegments)-1]
		c.appName = appName
	} else {
		output, err := c.runner.RunWithOutput("go", c.appRoot, false, "list", "-m")
		if err != nil {
			return err
		}

		c.appName = parseAppNameFromOutput(output)
	}

	return nil
}

func (c Contributor) setStartCommand() error {
	c.logger.Info("contributing start command")
	launchPath := filepath.Join(c.launchLayer.Root, c.appName)

	return c.launch.WriteApplicationMetadata(layers.Metadata{Processes: []layers.Process{{"web", launchPath}}})
}

func parseAppNameFromOutput(output string) string {
	sanitizedOutput := sanitizeOutput(output)
	moduleNamePaths := strings.Split(sanitizedOutput, "/")
	return moduleNamePaths[len(moduleNamePaths)-1]
}

func sanitizeOutput(output string) string {
	lines := strings.Split(output, "\n")
	return lines[len(lines)-1]
}

type Config struct {
	Go struct {
		Targets []string `yaml:"targets"`
	} `yaml:"go"`
}

func (c Contributor) determineTargets() ([]string, error) {
	if buildTarget := os.Getenv("BP_GO_TARGETS"); buildTarget != "" {
		targets := strings.Split(buildTarget, ":")
		return targets, nil
	}

	configPath := filepath.Join(c.appRoot, "buildpack.yml")
	config := Config{}
	if _, err := os.Stat(configPath); err == nil {
		yamlFile, err := ioutil.ReadFile(configPath)
		if err != nil {
			return nil, err
		}
		err = yaml.Unmarshal(yamlFile, &config)
		if err != nil {
			return nil, err
		}
	}

	if len(config.Go.Targets) < 1 {
		return []string{}, nil
	}

	return config.Go.Targets, nil
}
