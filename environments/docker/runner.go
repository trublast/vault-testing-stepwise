// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package docker

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/moby/go-archive"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	docker "github.com/moby/moby/client"
)

// Runner manages the lifecycle of the Docker container
type Runner struct {
	dockerAPI       *docker.Client
	ContainerConfig *container.Config
	ContainerName   string
	NetName         string
	IP              string
	CopyFromTo      map[string]string
}

// Start is responsible for executing the Vault container. It consists of
// pulling the specified Vault image, creating the container, and copies the
// plugin binary into the container file system before starting the container
// itself.
func (d *Runner) Start(ctx context.Context) (*container.InspectResponse, error) {
	hostConfig := &container.HostConfig{
		PublishAllPorts: true,
		AutoRemove:      true,
	}

	networkingConfig := &network.NetworkingConfig{}
	switch d.NetName {
	case "":
	case "host":
		hostConfig.NetworkMode = "host"
	default:
		es := &network.EndpointSettings{
			Aliases: []string{d.ContainerName},
		}
		if len(d.IP) != 0 {
			addr, err := netip.ParseAddr(d.IP)
			if err != nil {
				return nil, fmt.Errorf("invalid IP address %q: %w", d.IP, err)
			}
			es.IPAMConfig = &network.EndpointIPAMConfig{
				IPv4Address: addr,
			}
		}
		networkingConfig.EndpointsConfig = map[string]*network.EndpointSettings{
			d.NetName: es,
		}
	}

	// Best-effort pull. ImagePull will use a matching image from the local
	// Docker library, or if not found pull the matching image from a registry.
	// Wait drains the response stream so the local image is fully available.
	resp, err := d.dockerAPI.ImagePull(ctx, d.ContainerConfig.Image, docker.ImagePullOptions{})
	if err != nil {
		return nil, err
	}
	if resp != nil {
		if err := resp.Wait(ctx); err != nil {
			return nil, err
		}
	}

	cfg := *d.ContainerConfig
	hostConfig.CapAdd = []string{"IPC_LOCK"}
	cfg.Hostname = d.ContainerName
	fullName := d.ContainerName
	newContainer, err := d.dockerAPI.ContainerCreate(ctx, docker.ContainerCreateOptions{
		Config:           &cfg,
		HostConfig:       hostConfig,
		NetworkingConfig: networkingConfig,
		Name:             fullName,
	})
	if err != nil {
		return nil, fmt.Errorf("container create failed: %v", err)
	}

	// copies the plugin binary into the Docker file system. This copy is only
	// allowed before the container is started
	for from, to := range d.CopyFromTo {
		srcInfo, err := archive.CopyInfoSourcePath(from, false)
		if err != nil {
			return nil, fmt.Errorf("error copying from source %q: %v", from, err)
		}

		srcArchive, err := archive.TarResource(srcInfo)
		if err != nil {
			return nil, fmt.Errorf("error creating tar from source %q: %v", from, err)
		}
		defer srcArchive.Close()

		dstInfo := archive.CopyInfo{Path: to}

		dstDir, content, err := archive.PrepareArchiveCopy(srcArchive, srcInfo, dstInfo)
		if err != nil {
			return nil, fmt.Errorf("error preparing copy from %q -> %q: %v", from, to, err)
		}
		defer content.Close()
		_, err = d.dockerAPI.CopyToContainer(ctx, newContainer.ID, docker.CopyToContainerOptions{
			DestinationPath: dstDir,
			Content:         content,
		})
		if err != nil {
			return nil, fmt.Errorf("error copying from %q -> %q: %v", from, to, err)
		}
	}

	_, err = d.dockerAPI.ContainerStart(ctx, newContainer.ID, docker.ContainerStartOptions{})
	if err != nil {
		return nil, fmt.Errorf("container start failed: %v", err)
	}

	inspect, err := d.dockerAPI.ContainerInspect(ctx, newContainer.ID, docker.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	return &inspect.Container, nil
}
