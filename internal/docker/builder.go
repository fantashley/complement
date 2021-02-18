// Copyright 2020 The Matrix.org Foundation C.I.C.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package docker

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	client "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	"github.com/sirupsen/logrus"

	"github.com/matrix-org/complement/internal/b"
	"github.com/matrix-org/complement/internal/config"
	"github.com/matrix-org/complement/internal/instruction"
)

var (
	// HostnameRunningComplement is the hostname of Complement from the perspective of a Homeserver.
	HostnameRunningComplement = "host.docker.internal"
	// HostnameRunningDocker is the hostname of the docker daemon from the perspective of Complement.
	HostnameRunningDocker = "localhost"
)

func init() {
	if os.Getenv("CI") == "true" {
		log.Println("Running under CI: redirecting localhost to docker host on 172.17.0.1")
		// this assumes we are running inside docker so they have
		// forwarded the docker socket to us and we're in a container.
		HostnameRunningDocker = "172.17.0.1"
	}
}

const complementLabel = "complement_context"

type Builder struct {
	BaseImage      string
	ImageArgs      []string
	KeepBlueprints []string
	CSAPIPort      int
	FederationPort int
	Docker         *client.Client
	debugLogging   bool
	config         *config.Complement
}

func NewBuilder(cfg *config.Complement) (*Builder, error) {
	cli, err := client.NewEnvClient()
	if err != nil {
		return nil, err
	}
	return &Builder{
		Docker:         cli,
		BaseImage:      cfg.BaseImageURI,
		ImageArgs:      cfg.BaseImageArgs,
		KeepBlueprints: cfg.KeepBlueprints,
		CSAPIPort:      8008,
		FederationPort: 8448,
		debugLogging:   cfg.DebugLoggingEnabled,
		config:         cfg,
	}, nil
}

func (d *Builder) log(str string, args ...interface{}) {
	if !d.debugLogging {
		return
	}
	log.Printf(str, args...)
}

func (d *Builder) Cleanup() {
	err := d.removeContainers()
	if err != nil {
		d.log("Cleanup: Failed to remove containers: %s", err)
	}
	err = d.removeImages()
	if err != nil {
		d.log("Cleanup: Failed to remove images: %s", err)
	}
	err = d.removeNetworks()
	if err != nil {
		d.log("Cleanup: Failed to remove networks: %s", err)
	}
}

// removeImages removes all images with `complementLabel`.
func (d *Builder) removeNetworks() error {
	networks, err := d.Docker.NetworkList(context.Background(), types.NetworkListOptions{
		Filters: label(complementLabel),
	})
	if err != nil {
		return err
	}
	for _, nw := range networks {
		err = d.Docker.NetworkRemove(context.Background(), nw.ID)
		if err != nil {
			return err
		}
	}

	return nil
}

// removeImages removes all images with `complementLabel`.
func (d *Builder) removeImages() error {
	images, err := d.Docker.ImageList(context.Background(), types.ImageListOptions{
		Filters: label(complementLabel),
	})
	if err != nil {
		return err
	}
	for _, img := range images {
		bprintName := img.Labels["complement_blueprint"]
		keep := false
		for _, keepBprint := range d.KeepBlueprints {
			if bprintName == keepBprint {
				keep = true
				break
			}
		}
		if keep {
			d.log("Keeping image created from blueprint %s", bprintName)
			continue
		}
		_, err = d.Docker.ImageRemove(context.Background(), img.ID, types.ImageRemoveOptions{
			Force: true,
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// removeContainers removes all containers with `complementLabel`.
func (d *Builder) removeContainers() error {
	containers, err := d.Docker.ContainerList(context.Background(), types.ContainerListOptions{
		All:     true,
		Filters: label(complementLabel),
	})
	if err != nil {
		return err
	}
	for _, c := range containers {
		err = d.Docker.ContainerRemove(context.Background(), c.ID, types.ContainerRemoveOptions{
			Force: true,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *Builder) ConstructBlueprintsIfNotExist(bs []b.Blueprint) error {
	var blueprintsToBuild []b.Blueprint
	for _, bprint := range bs {
		images, err := d.Docker.ImageList(context.Background(), types.ImageListOptions{
			Filters: label("complement_blueprint=" + bprint.Name),
		})
		if err != nil {
			return fmt.Errorf("ConstructBlueprintsIfNotExist: failed to ImageList: %w", err)
		}
		if len(images) == 0 {
			blueprintsToBuild = append(blueprintsToBuild, bprint)
		}
	}
	return d.ConstructBlueprints(blueprintsToBuild)
}

func (d *Builder) ConstructBlueprints(bs []b.Blueprint) error {
	errc := make(chan []error, len(bs))
	for _, bprint := range bs {
		go (func(bprint b.Blueprint) {
			errc <- d.construct(bprint)
		})(bprint)
	}
	var errs []error
	for i := 0; i < len(bs); i++ {
		// the channel returns a slice of errors;
		// spread and append them to the error slice
		// (nothing will be appended if the slice is empty)
		errs = append(errs, <-errc...)
	}
	close(errc)
	if len(errs) > 0 {
		for _, err := range errs {
			d.log("could not construct blueprint: %s", err)
		}
		return errs[0]
	}

	// wait a bit for images/containers to show up in 'image ls'
	foundImages := false
	for i := 0; i < 50; i++ { // max 5s
		images, err := d.Docker.ImageList(context.Background(), types.ImageListOptions{
			Filters: label(complementLabel),
		})
		if err != nil {
			return err
		}
		if len(images) < len(bs) {
			time.Sleep(100 * time.Millisecond)
		} else {
			foundImages = true
			break
		}
	}
	// do this after we have found images so we know that the containers have been detached so
	// we can actually remove the networks.
	d.removeNetworks()
	if !foundImages {
		return fmt.Errorf("failed to find built images via ImageList: did they all build ok?")
	}
	return nil
}

// construct all Homeservers sequentially then commits them
func (d *Builder) construct(bprint b.Blueprint) (errs []error) {
	networkID, err := CreateNetwork(d.Docker, bprint.Name)
	if err != nil {
		return []error{err}
	}

	runner := instruction.NewRunner(bprint.Name, d.debugLogging)
	results := make([]result, len(bprint.Homeservers))
	for i, hs := range bprint.Homeservers {
		res := d.constructHomeserver(bprint.Name, runner, hs, networkID)
		if res.err != nil {
			errs = append(errs, res.err)
			if res.containerID != "" {
				// something went wrong, but we have a container which may have interesting logs
				printLogs(d.Docker, res.containerID, res.contextStr)
			}
		}

		// kill the container
		defer func(r result) {
			killErr := d.Docker.ContainerKill(context.Background(), r.containerID, "KILL")
			if killErr != nil {
				d.log("%s : Failed to kill container %s: %s\n", r.contextStr, r.containerID, killErr)
			}
		}(res)
		results[i] = res
	}

	// commit containers
	for _, res := range results {
		if res.err != nil {
			continue
		}
		labels := labelsForTokens(runner.AccessTokens(res.homeserver.Name))

		// Combine the labels for tokens and application services
		asLabels := labelsForApplicationServices(res.homeserver)
		for k, v := range asLabels {
			labels[k] = v
		}

		// commit the container
		commit, err := d.Docker.ContainerCommit(context.Background(), res.containerID, types.ContainerCommitOptions{
			Author:    "Complement",
			Pause:     true,
			Reference: "localhost/complement:" + res.contextStr,
			Config: &container.Config{
				Labels: labels,
			},
		})
		if err != nil {
			d.log("%s : failed to ContainerCommit: %s\n", res.contextStr, err)
			errs = append(errs, fmt.Errorf("%s : failed to ContainerCommit: %w", res.contextStr, err))
			continue
		}
		imageID := strings.Replace(commit.ID, "sha256:", "", 1)
		d.log("%s => %s\n", res.contextStr, imageID)
	}
	return errs
}

// construct this homeserver and execute its instructions, keeping the container alive.
func (d *Builder) constructHomeserver(blueprintName string, runner *instruction.Runner, hs b.Homeserver, networkID string) result {
	contextStr := fmt.Sprintf("%s.%s", blueprintName, hs.Name)
	d.log("%s : constructing homeserver...\n", contextStr)
	dep, err := d.deployBaseImage(blueprintName, hs, contextStr, networkID)
	if err != nil {
		log.Printf("%s : failed to deployBaseImage: %s\n", contextStr, err)
		containerID := ""
		if dep != nil {
			containerID = dep.ContainerID
		}
		return result{
			err:         err,
			containerID: containerID,
			contextStr:  contextStr,
			homeserver:  hs,
		}
	}
	d.log("%s : deployed base image to %s (%s)\n", contextStr, dep.BaseURL, dep.ContainerID)
	err = runner.Run(hs, dep.BaseURL)
	if err != nil {
		d.log("%s : failed to run instructions: %s\n", contextStr, err)
	}
	return result{
		err:         err,
		containerID: dep.ContainerID,
		contextStr:  contextStr,
		homeserver:  hs,
	}
}

// deployBaseImage runs the base image and returns the baseURL, containerID or an error.
func (d *Builder) deployBaseImage(blueprintName string, hs b.Homeserver, contextStr, networkID string) (*HomeserverDeployment, error) {
	asIDToRegistrationMap := asIDToRegistrationFromLabels(labelsForApplicationServices(hs))

	return deployImage(
		d.Docker, d.BaseImage, d.CSAPIPort, fmt.Sprintf("complement_%s", contextStr), blueprintName, hs.Name, asIDToRegistrationMap, contextStr,
		networkID, d.config.VersionCheckIterations,
	)
}

// getCaVolume returns the correct mounts and volumes for providing a CA to homeserver containers.
func getCaVolume(docker *client.Client, ctx context.Context) (map[string]struct{}, []mount.Mount, error) {
	var caVolume map[string]struct{}
	var caMount []mount.Mount

	if os.Getenv("CI") == "true" {
		// When in CI, Complement itself is a container with the CA volume mounted at /ca.
		// We need to mount this volume to all homeserver containers to synchronize the CA cert.
		// This is needed to establish trust among all containers.

		// Get volume mounted at /ca. First we get the container ID
		// /proc/1/cpuset should be /docker/<containerId>
		cpuset, err := ioutil.ReadFile("/proc/1/cpuset")
		if err != nil {
			return nil, nil, err
		}
		if !strings.Contains(string(cpuset), "docker") {
			return nil, nil, errors.New("Could not identify container ID using /proc/1/cpuset")
		}
		cpusetList := strings.Split(strings.TrimSpace(string(cpuset)), "/")
		containerId := cpusetList[len(cpusetList)-1]
		container, err := docker.ContainerInspect(ctx, containerId)
		if err != nil {
			return nil, nil, err
		}
		// Get the volume that matches the destination in our complement container
		var volumeName string
		for i := range container.Mounts {
			if container.Mounts[i].Destination == "/ca" {
				volumeName = container.Mounts[i].Name
			}
		}
		if volumeName == "" {
			// We did not find a volume. This container might be created without a volume,
			// or CI=true is passed but we are not running in a container.
			// todo: log that we do not provide a CA volume mount?
			return nil, nil, nil
		} else {
			caVolume = map[string]struct{}{
				"/ca": {},
			}
			caMount = []mount.Mount{
				{
					Type:   mount.TypeVolume,
					Source: volumeName,
					Target: "/ca",
				},
			}
		}
	} else {
		// When not in CI, our CA cert is placed in the current working dir.
		// We bind mount this directory to all homeserver containers.
		cwd, err := os.Getwd()
		if err != nil {
			return nil, nil, err
		}
		caCertificateDirHost := path.Join(cwd, "ca")
		if _, err := os.Stat(caCertificateDirHost); os.IsNotExist(err) {
			err = os.Mkdir(caCertificateDirHost, 0770)
			if err != nil {
				return nil, nil, err
			}
		}
		caMount = []mount.Mount{
			{
				Type:   mount.TypeBind,
				Source: path.Join(cwd, "ca"),
				Target: "/ca",
			},
		}
	}
	return caVolume, caMount, nil
}

func generateASRegistrationYaml(as b.ApplicationService) string {
	return fmt.Sprintf("id: %s\n", as.ID) +
		fmt.Sprintf("hs_token: %s\n", as.HSToken) +
		fmt.Sprintf("as_token: %s\n", as.ASToken) +
		fmt.Sprintf("url: '%s'\n", as.URL) +
		fmt.Sprintf("sender_localpart: %s\n", as.SenderLocalpart) +
		fmt.Sprintf("rate_limited: %v\n", as.RateLimited) +
		"namespaces:\n" +
		"  users: []\n" +
		"  rooms: []\n" +
		"  aliases: []\n"
}

func idsFromApplicationServices(asList []b.ApplicationService) []string {
	ids := make([]string, len(asList))
	for i, as := range asList {
		ids[i] = as.ID
	}

	return ids
}

func deployImage(
	docker *client.Client, imageID string, csPort int, containerName, blueprintName, hsName string, asIDToRegistrationMap map[string]string, contextStr, networkID string, versionCheckIterations int,
) (*HomeserverDeployment, error) {
	ctx := context.Background()
	var extraHosts []string
	var caVolume map[string]struct{}
	var caMount []mount.Mount
	var err error

	if runtime.GOOS == "linux" {
		// By default docker for linux does not expose this, so do it now.
		// When https://github.com/moby/moby/pull/40007 lands in Docker 20, we should
		// change this to be  `host.docker.internal:host-gateway`
		extraHosts = []string{HostnameRunningComplement + ":172.17.0.1"}
	}

	if os.Getenv("COMPLEMENT_CA") == "true" {
		caVolume, caMount, err = getCaVolume(docker, ctx)
		if err != nil {
			return nil, err
		}
	}

	env := []string{
		"SERVER_NAME=" + hsName,
		"COMPLEMENT_CA=" + os.Getenv("COMPLEMENT_CA"),
	}

	var asIDs []string
	for asID, registration := range asIDToRegistrationMap {
		env = append(env, fmt.Sprintf("AS_REGISTRATION_%s=", asID)+registration)
		asIDs = append(asIDs, asID)
	}

	env = append(env, "AS_REGISTRATION_IDS="+strings.Join(asIDs, " "))

	body, err := docker.ContainerCreate(ctx, &container.Config{
		Image: imageID,
		Env:   env,
		//Cmd:   d.ImageArgs,
		Labels: map[string]string{
			complementLabel:        contextStr,
			"complement_blueprint": blueprintName,
			"complement_hs_name":   hsName,
		},
		Volumes: caVolume,
	}, &container.HostConfig{
		PublishAllPorts: true,
		ExtraHosts:      extraHosts,
		Mounts:          caMount,
	}, &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			hsName: {
				NetworkID: networkID,
				Aliases:   []string{hsName},
			},
		},
	}, containerName)
	if err != nil {
		return nil, err
	}
	containerID := body.ID
	err = docker.ContainerStart(ctx, containerID, types.ContainerStartOptions{})
	if err != nil {
		return nil, err
	}
	inspect, err := docker.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, err
	}
	baseURL, fedBaseURL, err := endpoints(inspect.NetworkSettings.Ports, 8008, 8448)
	if err != nil {
		return nil, fmt.Errorf("%s : image %s : %w", contextStr, imageID, err)
	}
	versionsURL := fmt.Sprintf("%s/_matrix/client/versions", baseURL)
	// hit /versions to check it is up
	var lastErr error
	for i := 0; i < versionCheckIterations; i++ {
		res, err := http.Get(versionsURL)
		if err != nil {
			lastErr = fmt.Errorf("GET %s => error: %s", versionsURL, err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if res.StatusCode != 200 {
			lastErr = fmt.Errorf("GET %s => HTTP %s", versionsURL, res.Status)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		lastErr = nil
		break
	}

	logrus.WithFields(logrus.Fields{
		"inspect.Config.Labels": inspect.Config.Labels,
	}).Error("fwewfeaafewffffwewfe")

	d := &HomeserverDeployment{
		BaseURL:             baseURL,
		FedBaseURL:          fedBaseURL,
		ContainerID:         containerID,
		AccessTokens:        tokensFromLabels(inspect.Config.Labels),
		ApplicationServices: asIDToRegistrationFromLabels(inspect.Config.Labels),
	}
	if lastErr != nil {
		return d, fmt.Errorf("%s: failed to check server is up. %w", contextStr, lastErr)
	}
	return d, nil
}

// CreateNetwork creates a docker network and returns its id.
// ID is guaranteed not to be empty when err == nil
func CreateNetwork(docker *client.Client, blueprintName string) (networkID string, err error) {
	// make a user-defined network so we get DNS based on the container name
	nw, err := docker.NetworkCreate(context.Background(), "complement_"+blueprintName, types.NetworkCreate{
		Labels: map[string]string{
			complementLabel:        blueprintName,
			"complement_blueprint": blueprintName,
		},
	})
	if err != nil {
		return "", fmt.Errorf("%s: failed to create docker network. %w", blueprintName, err)
	}
	if nw.Warning != "" {
		if nw.ID == "" {
			return "", fmt.Errorf("%s: fatal warning while creating docker network. %s", blueprintName, nw.Warning)
		}
		log.Printf("WARNING: %s\n", nw.Warning)
	}
	if nw.ID == "" {
		return "", fmt.Errorf("%s: unexpected empty ID while creating networkID", blueprintName)
	}
	return nw.ID, nil
}

func printLogs(docker *client.Client, containerID, contextStr string) {
	reader, err := docker.ContainerLogs(context.Background(), containerID, types.ContainerLogsOptions{
		ShowStderr: true,
		ShowStdout: true,
		Follow:     false,
	})
	if err != nil {
		log.Printf("%s : Failed to extract container logs: %s\n", contextStr, err)
		return
	}
	log.Printf("============================================\n\n\n")
	log.Printf("%s : Server logs:\n", contextStr)
	stdcopy.StdCopy(log.Writer(), log.Writer(), reader)
	log.Printf("============== %s : END LOGS ==============\n\n\n", contextStr)
}

func label(in string) filters.Args {
	f := filters.NewArgs()
	f.Add("label", in)
	return f
}

func tokensFromLabels(labels map[string]string) map[string]string {
	userIDToToken := make(map[string]string)
	for k, v := range labels {
		if strings.HasPrefix(k, "access_token_") {
			userIDToToken[strings.TrimPrefix(k, "access_token_")] = v
		}
	}
	return userIDToToken
}

func labelsForTokens(userIDToToken map[string]string) map[string]string {
	labels := make(map[string]string)
	// collect and store access tokens as labels 'access_token_$userid: $token'
	for k, v := range userIDToToken {
		labels["access_token_"+k] = v
	}
	return labels
}

func asIDToRegistrationFromLabels(labels map[string]string) map[string]string {
	asMap := make(map[string]string)
	for k, v := range labels {
		if strings.HasPrefix(k, "application_service_") {
			asMap[strings.TrimPrefix(k, "application_service_")] = v
		}
	}
	return asMap
}

func labelsForApplicationServices(hs b.Homeserver) map[string]string {
	labels := make(map[string]string)
	// collect and store app service registrations as labels 'application_service_$as_id: $registration'
	// collect and store app service access tokens as labels 'access_token_$sender_localpart: $as_token'
	for _, as := range hs.ApplicationServices {
		// TODO: Generate unique tokens on each run
		as.HSToken = "27562ff25dd2eb69361ac1eb67e3a3cd38ab9509c1483234ec8dfec0f247c73e"
		as.ASToken = "f872531e387377686989e792c723e646f7823643e747a0521e94770a721f40fc"

		labels["application_service_"+as.ID] = generateASRegistrationYaml(as)

		labels["access_token_@"+as.SenderLocalpart+":"+hs.Name] = as.ASToken
	}
	return labels
}

func endpoints(p nat.PortMap, csPort, ssPort int) (baseURL, fedBaseURL string, err error) {
	csapiPort := fmt.Sprintf("%d/tcp", csPort)
	csapiPortInfo, ok := p[nat.Port(csapiPort)]
	if !ok {
		return "", "", fmt.Errorf("port %s not exposed - exposed ports: %v", csapiPort, p)
	}
	baseURL = fmt.Sprintf("http://"+HostnameRunningDocker+":%s", csapiPortInfo[0].HostPort)

	ssapiPort := fmt.Sprintf("%d/tcp", ssPort)
	ssapiPortInfo, ok := p[nat.Port(ssapiPort)]
	if !ok {
		return "", "", fmt.Errorf("port %s not exposed - exposed ports: %v", ssapiPort, p)
	}
	fedBaseURL = fmt.Sprintf("https://"+HostnameRunningDocker+":%s", ssapiPortInfo[0].HostPort)
	return
}

type result struct {
	err         error
	containerID string
	contextStr  string
	homeserver  b.Homeserver
}
